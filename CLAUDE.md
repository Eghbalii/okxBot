# CLAUDE.md — okxBot Project Memory

This file is the persistent design/context document for this repository. Read it fully before
making changes. Keep it updated whenever architecture, endpoints, or conventions change.

## 1. Goal

An OKX **futures/perpetual-swap** trading bot where a **Reinforcement Learning (RL) agent**
decides:
- when to open / close / resize a position
- what leverage to use
- implicit risk management (the reward function penalizes drawdown/liquidation risk)

The system is a **hybrid Go + Python** architecture:
- **Go** ("go-engine"): realtime market data ingestion (WebSocket), order execution, account/risk
  management, and the low-latency trading loop. Go is fast, has great concurrency primitives for
  a persistent WS connection + reconnect logic, and is what we want production trading
  infrastructure written in.
- **Python** ("rl-service"): RL environment, training, and model inference server. Python owns
  the ML stack (Gymnasium + Stable-Baselines3 + PyTorch). Exposed to Go via a small FastAPI HTTP
  service — this avoids the complexity/fragility of embedding Python in Go (cgo/gRPC-in-process)
  while keeping each language doing what it's best at.

Communication between the two is intentionally simple: **Redis** for realtime market data
streaming (Go publishes, Python consumes for feature building / research) and a **FastAPI REST
endpoint** (`POST /predict`) that Go calls synchronously in the trading loop to get the agent's
action. This is a well-understood microservice pattern — not overengineered, not a monolith.

## 2. Why RL, and why these libraries

- **Gymnasium** (maintained fork of OpenAI Gym) — standard env API (`reset`, `step`, `Env`).
- **Stable-Baselines3 (SB3)** — battle-tested PyTorch RL implementations (PPO, SAC, TD3, DQN).
  Best default choice for a first RL project: mature, well-documented, easy to swap algorithms.
- **PPO (Proximal Policy Optimization)** is the initial algorithm of choice:
  - Handles continuous action spaces (position size %, leverage) well.
  - More stable / forgiving of reward-shaping mistakes than off-policy algorithms — good for a
    first RL project and for financial envs where sample efficiency is secondary to stability.
  - Can later A/B against SAC (continuous, more sample-efficient but touchier) once we have a
    solid backtesting harness.
- Action space design: `Box` with 2 continuous dims:
  1. `target_exposure` in `[-1, 1]` — desired position as a fraction of max allowed notional
     (negative = short, positive = long, 0 = flat). The env translates the delta between current
     and target exposure into open/increase/decrease/close orders.
  2. `leverage_frac` in `[0, 1]` — mapped to `[1x, MAX_LEVERAGE]`.
- Reward shaping (initial): realized/unrealized PnL (fee- and funding-adjusted) minus a penalty
  term proportional to drawdown and to distance from liquidation price. This gets the "risk of
  opening/closing a position" behavior without a separate risk model — the agent learns it
  through the reward signal. We can later add a second head / auxiliary loss for explicit risk
  classification if needed.
- **Training data source: live paper-trading (forward-test), not historical backtesting.**
  Per explicit product decision, the model is *not* trained primarily against replayed historical
  candles. Instead: a Strategy generates a signal from real-time market data → the **Paper
  Trading Engine** (§8) opens a virtual order with full features (entry, SL, TP, size, leverage)
  at the live price → the order is tracked against the live price feed until SL or TP is hit →
  the closed trade (entry features, action taken, realized outcome) is persisted (§7) and used as
  training data. This is what actually improves the deployed model/strategies.
  - Trade-off to keep in mind: PPO-style RL typically wants hundreds of thousands of
    steps/episodes; a live-paced feed only produces on the order of tens of trades per
    strategy/token per day, so early training will accumulate experience slowly. This is an
    accepted, explicit trade-off in exchange for never training on data that doesn't reflect real
    order execution — do not "fix" this by silently reintroducing historical backtesting.
  - `rl_service/env/okx_futures_env.py` (the historical Gymnasium env) is kept only as an
    **optional, off-by-default sanity-check tool** for validating environment/reward-function
    code changes quickly with synthetic/historical data. It is not the source of the deployed
    model and should not be treated as one.
  - Live inference is still a frozen policy served over HTTP; periodic re-training consumes the
    paper-trading trade log and the model artifact is swapped (no true online/in-place weight
    updates in v1 — too risky with real money, and we want a reviewable model artifact per
    training run).

## 3. Repository layout

The layout below reflects the **clean-architecture** direction (§10), now implemented for the
core trading/paper-trading loops. Items marked `(planned)` don't exist yet — see the roadmap
(§14) for phasing.

```
okxBot/
├── CLAUDE.md                  # this file
├── README.md                  # human quickstart + infra/resource requirements
├── docker-compose.yml         # redis + timescaledb + go services + python service
├── go-engine/                 # Go module: data ingestion + order execution + risk + API
│   ├── cmd/
│   │   ├── ingestor/          # connects OKX public WS, publishes ticks/candles to Redis
│   │   ├── trader/            # main trading loop: reads state, calls RL service, executes orders
│   │   ├── paper-trader/      # forward-test engine, §8
│   │   └── api/               # dashboard/reporting HTTP API, §11
│   ├── internal/
│   │   ├── domain/            # core entities: Candle, Ticker, Position, Balance, Order,
│   │   │                        LeverageChange, Observation, Action — no framework/IO deps, §10
│   │   ├── usecase/           # application logic: Trader (live trading loop), PaperTrader
│   │   │                        (forward-test loop) — depend only on domain + port interfaces
│   │   ├── port/               # interfaces use-cases depend on: Repository, ExchangeClient,
│   │   │                        ModelClient, MarketDataConsumer/Publisher
│   │   ├── config/             # env/yaml config loading
│   │   ├── okx/                 # OKX adapter (implements port.ExchangeClient); converts wire
│   │   │   │                      types to/from domain at the boundary
│   │   │   ├── rest/            # signed REST client: orders, leverage, positions, balance
│   │   │   └── ws/              # public + private websocket clients (reconnect, heartbeat)
│   │   ├── postgres/            # TimescaleDB/Postgres adapter (implements port.Repository), §7
│   │   ├── stream/               # Redis pub/sub + stream helpers — the event bus, §12;
│   │   │                           implements port.MarketDataConsumer/Publisher
│   │   ├── strategy/              # strategy registry + indicator library, §9
│   │   ├── risk/                  # hard risk limits (circuit breakers) independent of the RL model
│   │   └── rlclient/               # HTTP client for the Python inference API (implements port.ModelClient)
│   └── configs/config.example.yaml
├── rl-service/                 # Python: env, training, inference
│   ├── requirements.txt
│   ├── rl_service/
│   │   ├── env/                # historical Gymnasium env — optional dev sanity-check only, §2
│   │   ├── data/                # historical data loading + feature engineering (shared feature
│   │   │                          code also used to build live observations)
│   │   ├── train.py             # SB3 PPO training entrypoint, consumes paper-trading trade log
│   │   ├── metrics.py            # (planned) CPU/RAM/GPU + training-progress reporting, §11
│   │   └── serve/                # FastAPI inference app
│   └── configs/config.example.yaml
├── panel/                      # React+TS+Vite frontend for cmd/api: resources/model/strategies/
│   │                             positions tabs, §11
└── data/                       # gitignored local data cache (candles, parquet, model artifacts)
```

## 4. OKX v5 API notes (reference — verify against official docs before relying on in prod)

- REST base: `https://www.okx.com`
- WebSocket public: `wss://ws.okx.com:8443/ws/v5/public` (tickers, candle*, books channels)
- WebSocket business: `wss://ws.okx.com:8443/ws/v5/business` (candlesticks in v5)
- WebSocket private: `wss://ws.okx.com:8443/ws/v5/private` (orders, account, positions) — requires
  login frame signed the same way as REST.
- Demo/paper trading: same hosts with header `x-simulated-trading: 1` on REST, and demo WS host
  `wss://wspap.okx.com:8443/ws/v5/public` / `/private`. **Always develop against demo trading
  first.**
- REST auth headers: `OK-ACCESS-KEY`, `OK-ACCESS-SIGN`, `OK-ACCESS-TIMESTAMP`,
  `OK-ACCESS-PASSPHRASE`. Signature = `base64(HMAC_SHA256(secret, timestamp + method + requestPath
  + body))`, timestamp is ISO8601 ms UTC.
- Key endpoints used:
  - `GET /api/v5/market/tickers?instType=SWAP`
  - `GET /api/v5/public/instruments?instType=SWAP`
  - `POST /api/v5/trade/order`
  - `POST /api/v5/trade/cancel-order`
  - `POST /api/v5/account/set-leverage`
  - `GET /api/v5/account/positions`
  - `GET /api/v5/account/balance`
- Never commit API keys/secrets. Use `.env` (gitignored) loaded via config package.

## 5. Risk management (non-negotiable, independent of the RL model)

The Go `internal/risk` package enforces **hard limits that the RL agent cannot override**:
- Max leverage cap (config, independent of RL's chosen leverage — RL output is clamped to this).
- Max position notional / max % of account equity per position.
- Daily max drawdown → auto flatten + halt trading until manual reset.
- Distance-to-liquidation floor (reject any action that would push liquidation price within X% of
  mark price).
- Kill switch (config flag / signal) to flatten all positions and stop the engine.

This is deliberately implemented in Go, not Python, so a bug or bad output in the RL service can
never bypass hard safety limits.

## 6. Conventions

- Go: standard `gofmt`, module `github.com/<user>/okxBot/go-engine` (adjust to actual remote once
  created), errors wrapped with `%w`, structured logging (`log/slog`).
- Python: `black` + `ruff` formatting, type hints throughout, Pydantic models for API
  request/response schemas.
- Commit style: one focused commit per logical step (see git log for the build history of this
  project). Conventional commit prefixes (`feat:`, `chore:`, `docs:`, `fix:`).
- Secrets: `.env` files and `configs/config.yaml` (real, non-example) are gitignored. Only
  `*.example.yaml` / `.env.example` are committed.

## 7. Data persistence

Two datastores, different jobs — this is not redundant, each is used for what it's good at:
- **Redis** (Streams) — the ephemeral real-time event bus (§12): ticks/candles/signals in transit
  between the ingestor and consumers (paper-trader, trader). Capped (~100k entries), not meant for
  long-term storage.
- **TimescaleDB** (Postgres + the Timescale extension, not a separate database) — durable storage.
  Schema (`internal/postgres/migrations`), all monetary/price columns `NUMERIC` (§ below on
  `decimal.Decimal`):
  - `candles` (hypertable: `inst_id`, `bar`, `ts`, OHLCV) — downsampled storage; raw tick-level
    data isn't persisted long-term, only finalized bars.
  - `paper_orders` — id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage,
    opened_at, closed_at, close_reason (`sl`|`tp`|`manual`), realized_pnl, features_json (the
    observation/indicator snapshot at entry, for training + the panel's order-detail view).
  - `strategies` — id, name, token(s) it's assigned to, config/version, enabled flag.
  - `training_runs` — (planned) started_at, finished_at, status, timesteps, model_artifact_path.

Implemented behind `port.Repository` (§10, `internal/postgres`), so use-cases never depend on pgx
directly. `internal/postgres.Migrate` applies embedded `.sql` migrations automatically on startup.
A finalized candle is written to Postgres by `PaperTrader` itself (from the candle consumer
handler), not by a separate DB-writer process.

## 8. Paper Trading Engine ("forward-test", not historical backtest)

This is the core training-data source, replacing historical backtesting per explicit product
decision (see §2). Flow:

1. A **Strategy** (§9) evaluates real-time market data for an instrument and emits a signal
   (buy/sell + suggested entry/SL/TP) — or the RL model does, once it's driving decisions.
2. The Paper Trading Engine opens a **virtual order** at the live price with the full feature set
   (entry, SL, TP, size, leverage) — no real order is sent to OKX.
3. The engine subscribes to the live price feed (via the Redis event bus, §12) and monitors the
   virtual order until SL or TP is hit (or a manual/timeout close condition).
4. On close, the engine persists the trade (entry features, action taken, realized outcome) to
   `paper_orders` (§7). This log is: (a) what the reporting panel (§11) displays, and (b) the
   training data consumed by `rl_service/train.py`.

This should run as its own process (`cmd/paper-trader`), independent of the live `cmd/trader`, so
enabling/disabling live trading never affects the continuous paper-trading/data-collection loop.

## 9. Strategy engine & multi-agent signal architecture

- **Strategy** = a pluggable signal generator: given market data + indicator values for an
  instrument, produce a signal (buy/sell/hold) plus suggested SL/TP/confidence. Lives behind a
  small `Strategy` interface (`Evaluate(candles, indicators) (Signal, error)`) so built-in and
  user-authored strategies look identical to the engine.
- **Built-in indicators/strategies (v1):** moving averages, RSI, MACD, and structured concepts
  like ICT/Smart-Money (order blocks, liquidity zones, fair value gaps) implemented as regular Go
  or Python functions over OHLCV data — no separate scripting layer needed for these.
- **Per-token strategy assignment:** each instrument has one or more enabled strategies; switching
  which strategy runs on which token is a config/DB change, not a code change.
- **User-authored strategies (v2):** "clone an existing strategy and edit it" is straightforward
  (copy config row). A full custom scripting layer (Pine-Script-like) is a substantial feature on
  its own — plan to start with a constrained, composable JSON/DSL of indicator conditions
  (`if rsi(14) < 30 and close > sma(50) then buy`) before considering embedding an actual
  scripting/sandboxed interpreter for arbitrary user code. Flagging this now so it isn't
  underestimated later.
- **RL sits above the strategy layer:** strategies decide *when* there's a tradeable signal; the
  RL agent decides *how much* (sizing/leverage) and can learn to weight/ignore particular
  strategies or tokens based on their live track record.
- **Future agents (sentiment/news):** designed as just another entry in the same signal registry
  — a `Strategy`-shaped adapter around an LLM-based sentiment score. No architecture change needed
  when this is added later, which is why the registry pattern is worth building now instead of a
  single hardcoded strategy path.
- **Multi-timeframe candles (implemented):** `paper_trading.bars` (config, a list, default
  `["1m"]`) controls how many OKX candle WS channels `cmd/ingestor` subscribes to — one
  connection per bar (`candle1m`, `candle15m`, `candle1H`, `candle4H`, `candle1D`; note OKX's
  casing — lowercase `m`, capital `H`/`D`), each publishing to its own Redis stream
  (`okx:candles:<bar>`). `PaperTrader` keeps one candle window per bar
  (`candles map[string][]domain.Candle`, guarded by a mutex — each bar's consumer runs in its own
  goroutine) and strategies are assigned to a specific bar via `StrategyAssignment{Bar,
  Strategy}`, so a strategy only re-evaluates when its own timeframe's candle closes. Real
  exchange-confirmed candles per timeframe, not self-aggregated from 1m. Live-verified against
  real OKX data across `["1m","15m","1H"]` — see git history for the dry-run details, including a
  concurrent-map-write crash found and fixed during that run.

## 10. Clean architecture for go-engine

To support swapping exchanges (Bybit/Binance later) or the database without touching business
logic, `go-engine` is organized ports-and-adapters style (see §3 for the layout). Implemented:

- `internal/domain` — plain entities (Candle, Ticker, Position, Balance, OrderRequest,
  OrderResult, LeverageChange, Observation, Action), no framework/IO imports.
- `internal/usecase` — application logic: `Trader` (the live decide-and-execute loop) and
  `PaperTrader` (the forward-test loop), depending only on `domain` and `port` interfaces — this
  is where the actual business rules live and what's unit-tested (`internal/usecase/*_test.go`,
  hand-rolled fakes) without a real exchange or database.
- `internal/port` — interfaces the use-cases depend on: `ExchangeClient`, `Repository`,
  `MarketDataConsumer`/`MarketDataPublisher`, `ModelClient` (the RL inference call).
- Adapters implement those ports: `internal/okx` (via `internal/okx/rest.Client`) implements
  `ExchangeClient`, `internal/postgres` implements `Repository`, `internal/stream` implements
  `MarketDataConsumer`/`Publisher`, `internal/rlclient` implements `ModelClient`. Adapters convert
  their wire/storage formats to and from `domain` types at the boundary — e.g. `internal/okx`
  keeps OKX-specific JSON field names and string-typed candle arrays internally, exposing
  `ToDomain()`/`FromDomain()` converters rather than leaking OKX shapes into `usecase`.

## 11. Dashboard / reporting API

**Network model (decision):** the panel is never exposed on the open internet. Access is only
over an OpenVPN tunnel into the server's private network; the panel itself has **no login/auth in
v1** — the VPN + firewall (only VPN-sourced traffic can reach `cmd/api`'s port) is the sole gate.
This is an explicit, revisitable trade-off — if the panel is ever exposed more broadly, add real
auth before that happens, don't rely on network isolation alone at that point.

A new `cmd/api` service (built on the same use-cases as the trading engine, per §10) exposing four
panel sections: **resources**, **RL model status**, **strategies**, **positions**. Foundations
(schema, ports, endpoints) are the priority for the first pass; the strategy timeline/chart view
and the parameter-edit-on-hover UI are `panel/` (frontend) concerns to build once the backend
foundation is in place and there's real data to look at.

### 11.1 Server resources

Handled entirely by **Prometheus + Grafana** (already implemented, see below) — `cmd/api` does not
duplicate this. The panel's "resources" tab embeds/links the relevant Grafana dashboard rather than
re-implementing CPU/RAM/GPU charts.

### 11.2 RL model status (live/crash/logs)

Deliberately **not** a custom in-process heartbeat table — a crashed process can't reliably report
its own crash. Instead this is standard process supervision, surfaced through `cmd/api`:
- **Liveness:** `cmd/api` polls the RL service's existing `GET /health` (`rl_service/serve/api.py`)
  — returns `{"status": "ok", "model_loaded": bool}`. `model_loaded: false` means the service is up
  but running with no trained model (safe no-op actions only, see `predict()`'s fail-safe path) —
  the panel must distinguish "down" from "up but unloaded," they mean very different things.
- **Uptime / crash / restart history:** owned by the process supervisor, not application code.
  `rl-service` and `go-engine` processes run under **systemd** (or the Docker restart policy in
  `docker-compose.yml`, `restart: unless-stopped` + `docker inspect` for restart count/start time)
  — `cmd/api` shells out to `systemctl show <unit> --property=ActiveState,SubState,ExecMainStartTimestamp,NRestarts`
  (or the Docker equivalent) to answer "is it live," "how long has it been alive," and "how many
  times has it crashed/restarted." Don't reinvent this in Go/Python.
- **Logs/errors:** `journalctl -u <unit> -n 200 --no-pager` (or `docker logs --tail 200`) tailed
  through a `cmd/api` endpoint, filterable to error-level lines. No separate log-shipping stack
  (Loki/ELK) for v1 — journald/docker's own log store is enough at this scale.
- `rl_service/metrics.py` (CPU/RAM/GPU + training-progress numbers, per the original plan) is still
  useful for the *training* process specifically (elapsed steps, timesteps/sec) and is planned but
  not implemented — separate concern from liveness/crash status above.

### 11.3 Strategies — parent + override model

Requirement driving this: after a crash/restart, the trader/paper-trader must reload from the
**database**, not memory, exactly which strategy variant is assigned to which token+timeframe and
with what parameters — strategy assignment is durable state, not runtime-only config.

- Every strategy row still descends from a **locked "origin"** row (`cloned_from IS NULL`,
  `is_origin = true`) representing the built-in Go implementation (§9) as shipped — origin rows
  are never edited in place, only read as the template new variants copy their `kind`/defaults
  from. This satisfies "keep the origin always."
- A **sub-strategy** is a child row (`cloned_from = <origin id>`) that stores only: its own
  `config` (full param JSON — the override values, not a diff, so a crash-recovery read is a
  single row fetch, no merge-with-parent logic needed at load time) and its `inst_ids` +
  `assignments` (which token/timeframe pairs it runs on, see below). Editing a sub-strategy edits
  its own row; the origin is untouched. "Reset to origin" = recopy the origin's default `config`
  into the child row.
- **Token+timeframe assignment** is now its own table (`strategy_assignments`: `strategy_id`,
  `inst_id`, `bar`, `enabled`), not just the `strategies.inst_ids` array — this is what lets the
  same origin strategy run as different tuned variants on different tokens/timeframes
  simultaneously, and is exactly the state `PaperTrader.Strategies []StrategyAssignment` (today
  built in Go code at startup, see `cmd/paper-trader/main.go`) needs to instead load from
  Postgres via `Repository.ListAssignments(...)` on every process start — this is the crash-safety
  requirement: the assignment set is never only-in-memory.
- `paper_orders.strategy_id` (already a column) must actually be populated when a paper order is
  opened — currently `buildPaperOrder` in `internal/usecase/papertrade.go` leaves it nil. Fixing
  this is required for the per-strategy stats below to be computable at all.
- **Per-strategy stats** (derived from `paper_orders` grouped by `strategy_id`, no new counters
  needed beyond what §7/§11.4 already store): running duration (now − first assignment/first
  order), signal count, win rate (`close_reason='tp'` vs `'sl'` ratio), realized PnL. Exposed via
  `cmd/api` as a computed view/query, not a separately maintained table.
- **Timeline/chart view** (price line with long/short entry markers, red/green SL/TP flags,
  hover-to-edit params) is explicitly a "foundations first" item — backend must expose the data
  (candles + paper_orders for a strategy+instrument+time range) via a clean endpoint, but the
  actual chart rendering is deferred to a later `panel/` pass.

### 11.4 Positions panel

One panel, three **modes** — `paper`, `demo`, `real` — as a `mode` column threaded through from
day one so the schema/API never need reshaping later, even though `real` has no live writer yet
(blocked on live `cmd/trader` wiring, §14):
- `paper` — from `paper_orders` (already implemented).
- `demo` — OKX demo trading (`x-simulated-trading: 1`, §4) via the same `ExchangeClient`/order
  path `cmd/trader` will use for real trading, just pointed at demo credentials/hosts.
- `real` — live `cmd/trader` against real OKX credentials; schema-ready, inactive until that
  roadmap item (§14) lands.
- Positions are classified **open** vs **closed** (`closed_at IS NULL` for paper; analogous for
  demo/real once those tables/rows exist) and sortable by token, date, close reason (SL/TP/manual),
  and PnL (highest profit/loss) — all directly supported by existing/planned indexed columns, no
  new derived-data pipeline required.
- **Sound + browser notification on state change**, per explicit request: distinct cues for (a)
  position opened, (b) closed by SL, (c) closed by TP. This is a `panel/` frontend concern — the
  frontend polls/subscribes to `cmd/api` (or a lightweight SSE/WebSocket stream off the same
  Postgres rows / Redis event bus, §12) for order-state transitions and triggers
  `new Audio(...).play()` + the browser Notification API client-side. No backend "notification
  service" needed; `cmd/api` just needs to expose the open/close events promptly.

### 11.5 Strategy CRUD + reports (as originally planned)

- **Strategy CRUD:** list, view, edit (a sub-strategy's own config), clone-to-create (from origin
  or from another sub-strategy), enable/disable per token+timeframe assignment (§11.3).
- **Reports:** total orders/profit/loss with hour/day/week filters, per-order detail (entry/SL/TP,
  timestamps, close reason) across all three position modes (§11.4).

### 11.6 Metrics (implemented): Prometheus + Grafana

Per explicit decision — not reinvented as a custom dashboard, kept separate from the
product-specific strategy/orders/model-status API above. `go-engine` services expose `/metrics`
via `internal/metrics` (`promhttp`), scraped per `prometheus.yml`:
- `okxbot_strategy_signals_total{strategy,inst_id,side}` — every strategy evaluation, incl. holds.
- `okxbot_paper_orders_opened_total{strategy,inst_id,side}` — virtual trades opened.
- `okxbot_paper_orders_closed_total{inst_id,reason}` — closed trades by reason; `reason="sl"` /
  `reason="tp"` gives the SL-hit / TP-hit counts directly (rate()/increase() in Grafana for
  hour/day/week windows, matching the panel's reporting requirement).
- `okxbot_paper_orders_open{inst_id}` — current open-order gauge.
- `okxbot_paper_orders_realized_pnl_usd_total{inst_id}` — cumulative realized PnL gauge.
- `okxbot_ingestor_events_total{kind,inst_id}` — WS ticks/candles received, for pipeline health.
- Standard Go process metrics (goroutines, GC, memory) come free from `promhttp`.
Grafana dashboards themselves (panels/layout) aren't built yet — only the metrics + scrape config
are wired; building the actual dashboard JSON is a small follow-up once there's real data to look
at. `rl_service/metrics.py` (CPU/RAM/GPU/training progress) is still planned, not implemented.

## 12. Event-driven design

Real-money live trading benefits from reacting to events (fills, price ticks, risk breaches)
rather than polling. The ingestion layer is already event-driven (OKX WS push → Redis Stream).
Decision: extend that pattern as the **internal event bus from day one** — ticks, strategy
signals, and order-fill events all flow through Redis Streams/pub-sub (which already supports
consumer groups), and internal components (risk manager, paper-trading engine, order executor)
are written as handlers subscribing to that bus. This gets most of the benefit of an event-driven
architecture without adopting heavier infra (Kafka/NATS) before it's needed. Code against the
`port.MarketDataConsumer`/`MarketDataPublisher` interfaces (§10) so migrating the transport later,
if scale ever demands it, is mechanical rather than a rewrite.

## 13. AI orchestration (LangChain/CrewAI/"Hermes"-style)

Not needed for the current scope — today's "agents" (technical indicator strategies + the RL
model) are deterministic/quantitative, not LLM-based, so there's nothing to orchestrate between
yet. This becomes genuinely useful once LLM-based sentiment/news agents are added (§9's "future
agents"), because at that point something needs to call heterogeneous agents and combine/weight
their outputs. Plan: start with a small custom "signal aggregator" in the strategy registry (§9)
rather than adopting a full orchestration framework, and only reach for a named framework if the
number/complexity of LLM-based agents grows enough to justify the dependency.

## 14. Roadmap / status

Phases 0-1 (architecture, OKX REST/WS clients, Postgres/TimescaleDB persistence, strategy
interface, Paper Trading Engine, Prometheus/Grafana metrics) are complete — see git history for
details. The one Phase 1 item that was open here (repointing `train.py` at the paper-trading log)
is now folded into Phase 5 (§15.8) below, since it's meaningless to do in isolation from the
per-token training design.

Paper-trading data flow (current design): the ingestor subscribes to
OKX's public `tickers` channel and business `candle{bar}` channel over a single WS connection
each, publishing every event to Redis Streams. The Paper Trading Engine consumes both via
consumer groups (`internal/stream.Consumer`): every **tick** triggers an immediate SL/TP check
against open virtual orders (no missed intra-bar wicks, no polling delay), and every **finalized
candle** triggers strategy re-evaluation and is persisted to Postgres. A REST call
(`GetCandles`) is used exactly once at startup, to seed the initial in-memory candle window —
not on an ongoing poll loop. This replaces the earlier REST-polling version, which had a real
correctness bug (checking SL/TP only against candle-close prices could silently miss a price wick
that touched SL/TP and reverted within the same bar) in addition to rate-limit/delay concerns.

Phase 2 — clean architecture refactor & live wiring (current phase):
- [x] All price/size/leverage/PnL/risk-limit fields migrated `float64` → `decimal.Decimal`
      (`github.com/shopspring/decimal`), including Postgres `NUMERIC` columns via
      `github.com/jackc/pgx-shopspring-decimal` — `float64` can't represent most decimal fractions
      exactly, which drifted repeated price arithmetic.
- [x] Refactored `go-engine` into `domain`/`usecase`/`port`/adapters (§10); added the repo's first
      unit tests (`internal/usecase/*_test.go`, hand-rolled fakes). Caught and fixed two real bugs
      along the way: (1) `risk.Manager.Approve` clamped position notional with a sign-unaware
      comparison, so RL-requested shorts were silently placed as longs; (2) OKX WS subscribe-ack
      frames were misclassified as data pushes, breaking every message decode — only surfaced once
      `cmd/ingestor` was run against live OKX WS for the first time. Known simplifications still
      open: liquidation-buffer estimate is a conservative `100/leverage` approximation (ignores
      maintenance margin), order sizing assumes a contract multiplier of 1 (no
      `/api/v5/public/instruments` lookup yet).
- [x] `cmd/ingestor` + `cmd/paper-trader` dry run validated end-to-end against live OKX public
      market data (top-10 crypto perpetuals by volume) and a local Redis/TimescaleDB — confirmed
      the full pipeline (WS → Redis Streams → strategy evaluation → Postgres persistence →
      Prometheus metrics) works correctly, including the decimal/`NUMERIC` migration against a
      real database for the first time.
- [ ] End-to-end dry run against OKX **demo trading** (`cmd/trader`, real order placement against
      OKX's sandboxed demo environment) — needs OKX demo API credentials; deferred until running on
      a server (not yet started).

Phase 3 — dashboard (current phase, §11):
- [x] Migration (`000003_strategy_assignments_and_positions_mode`) for `strategy_assignments` +
      `strategies.is_origin`; `mode` column on `paper_orders` (paper/demo/real, §11.4)
- [x] Fixed `buildPaperOrder` (`internal/usecase/papertrade.go`) to populate `strategy_id` —
      required for per-strategy stats to be computable at all
- [x] `port.Repository` additions: assignment CRUD, `StrategyStatsFor`, `ListPositions`
      (filterable/sortable across modes); `internal/strategy/factory.go` (kind -> constructor
      registry) so a DB row's Kind+Config resolves back into a live `strategy.Strategy`;
      `cmd/paper-trader` now seeds origin rows and loads `strategy_assignments` from Postgres on
      every start instead of a hardcoded Go slice — the crash-recovery requirement driving §11.3
- [x] `cmd/api` (`internal/api`): resources (Grafana link-through), RL model status (`/health`
      proxy + systemd/docker uptime+restart-count+log-tail via `internal/api/procstatus.go`),
      strategy CRUD + assignments + stats, positions list — no auth (OpenVPN-only network access,
      §11). `Dockerfile.api` + compose wiring (mounts the Docker socket read-only so it can query
      sibling containers' status/logs when `api.process_manager: docker`).
- [x] `panel/` frontend: **React 19 + TypeScript + Vite**, `react-router-dom` for the 4 tabs
      (Positions/Strategies/RL Model/Resources), no UI framework/component library added (kept
      dependencies minimal — hand-rolled CSS, dark theme). Same-origin `/api/*` calls in
      production (nginx reverse-proxies to `cmd/api` inside the compose network, matching the
      OpenVPN-only model — panel is never meant to be reachable outside that network); a Vite dev
      proxy (`vite.config.ts`) does the same for local dev against `127.0.0.1:8090`. Positions and
      Model Status poll on an interval (`usePolling` hook) rather than WebSocket/SSE — simplest
      option for v1's data volume, revisit if latency becomes noticeable. Sound + browser
      notifications for open/SL/TP (`usePositionAlerts` hook, generated WAV cues in `src/sounds/`)
      diff each poll's position list client-side — no backend "notification service" needed to
      support this. Strategies page includes the hover-to-edit param box on sub-strategies (origin
      rows render read-only) — the one "ideal view" item that *didn't* need deferring.
      **Deferred** (foundations-first, per explicit decision): the price-chart timeline overlay
      (candles + entry/SL/TP markers) — `panel/` has no charting library yet; add one only once
      there's a concrete design for it.
- [ ] `rl_service/metrics.py` (CPU/RAM/GPU/training progress) — separate from liveness/crash status
- [x] End-to-end verification of the panel against a live `cmd/api` + real TimescaleDB (docker
      compose) + a headless-browser pass (Playwright) over all four routes. Caught and fixed three
      real bugs this way — none of these showed up from `tsc`/`vite build` alone:
      1. `cmd/api` never seeded origin strategy rows itself (only `cmd/paper-trader` did) — the
         Strategies panel was empty on any install where `cmd/api` came up first. Extracted the
         seeding logic into `internal/strategy.SeedOrigins` (shared by both `main.go`s).
      2. `CreateStrategy` sent a nil `InstIDs` slice as SQL `NULL` (pgx does not fall back to the
         column's `NOT NULL DEFAULT '{}'` for a nil slice) — crashed `SeedOrigins` outright.
         Fixed in `internal/postgres/strategies.go`.
      3. Panel-side: `api.listStrategies`/`listAssignments`/`listPositions` crashed the Strategies
         page (`Cannot read properties of null (reading 'map')`) whenever Go returned an empty nil
         slice (`null`, not `[]`) — e.g. a fresh `strategy_assignments` table. Fixed with a
         `requestList` wrapper in `panel/src/api/client.ts` that normalizes `null` → `[]`.
         Separately, `CloneForm`/`AssignmentsPanel` initialized their `<select>`'s backing state
         from `origins[0]?.ID`/`strategies[0]?.ID` at first render, before either list had loaded
         — the dropdown displayed a selection once data arrived, but the state variable stayed
         stuck at the stale initial value, so submitting silently no-op'd on the `!id` guard.
         Fixed by making the state `nullable` and resolving the real default at submit time.
      Verified via curl against every `cmd/api` route (strategy CRUD, origin-protection 403s,
      assignments CRUD, positions filters/sort, model status/logs) and a headless Chromium pass
      that drove the actual clone-strategy and create-assignment forms through the rendered UI,
      confirming the POSTs fire and the new rows render — not just that the endpoints respond.

Phase 4 — later/optional:
- [x] Multi-timeframe candles (§9) — ingestor/PaperTrader multi-bar, live-verified
- [ ] User-authored strategy scripting layer (§9)
- [ ] Sentiment/news agents + signal aggregator (§9, §13)
- [ ] Evaluate migrating the event bus off Redis Streams if scale demands it (§12)

Phase 5 — global RL agent over price + strategy signals (§15, current phase):
- [x] Extend `strategy.Signal`/`domain.Observation`/`rlclient.Action`/`rl_service` Pydantic
      schemas for the first round of new fields: per-timeframe strategy-signal blocks,
      recent-performance tail, `strategy_weights`, `sl_adjust_pct`/`tp_adjust_pct`
      (`schema_version` 2) — done before the global-agent/raw-price-context revision below.
- [x] Bumped observation schema to v3: token-identity one-hot (`ActiveTokens`) and raw-price-context
      fields (`PriceContext.ClosePctChanges`, `DistToSwingHighPct`/`DistToSwingLowPct`,
      `DistToSLPct`/`DistToTPPct`) landed together with the version bump on both sides
      (`domain.ObservationSchemaVersion` / `OBSERVATION_SCHEMA_VERSION` = 3). Distance-to-key-MA
      was not added — `Features` (FEATURE_COLUMNS' `sma_ratio_*`) already covers this; revisit only
      if that proves insufficient once real training data exists.
- [x] Raised `risk.max_leverage` default 5 -> 100 and `risk.min_liquidation_buffer_pct` 15 -> 2 (an
      explicit, documented tradeoff of liquidation-distance safety margin for a usable leverage
      range — see the comments in `config.go`/`config.example.yaml`); `rl_service`'s
      `EnvConfig.max_leverage` raised to match. §5's hard risk-manager caps still independently
      bound worst case, confirmed via `risk.Manager.Approve`'s existing clamp/reject logic
      (untouched by this change).
- [x] Go-side ratchet clamp on SL/TP adjustments: `usecase.RatchetSLTP`
      (`internal/usecase/sltp_ratchet.go`), magnitude-clamped to `MaxSLTPAdjustPct` (±2%) then
      direction-checked so a proposal can only tighten, never widen or undo a prior tightening —
      8 unit tests covering long/short, widening rejection, ratchet-can't-be-undone, oversized-
      adjustment clamping, nil SL/TP, and TP-can't-cross-price.
- [x] Per-token budget tracking + zero/negative reset-with-logging (paper/demo mode only): new
      `token_budgets` table (migration 000004) + `port.Repository.GetTokenBudget`/`ApplyTokenPnL`;
      wired into `PaperTrader.monitorOpenOrders` on every baseline-order close, gated to
      `variant='baseline'` only (§15.4's fork mechanic below never double-counts) (§15.7).
- [x] Shadow-fork mechanic for SL/TP adjustment (design decision made and implemented during this
      phase, not originally scoped in §15.4's first draft — see §15.4's "Shadow-fork mechanic"
      note): `port.Repository.ForkPaperOrderWithSLTP` + migration 000004's `parent_order_id`/
      `variant` columns; `usecase.PaperTrader.adjustOpenOrdersWithRL` calls the model at
      candle-close for every open baseline order, ratchets the proposal, and forks a linked
      `rl_adjusted` copy rather than editing in place — verified with dedicated tests
      (fork-on-nonzero-adjustment, no-fork-on-zero-action, skip-when-no-baseline-orders).
- [x] Wired end-to-end with a still-untrained/no-op model: `cmd/paper-trader` constructs
      `rlclient.New(cfg.RLService.URL)` as `PaperTrader.Model` when the new
      `paper_trading.rl_sltp_adjust` config flag is enabled (off by default — additive, never
      required, matching §15's rollout posture throughout); `ActiveTokens`/`TokenBudgetUSD` wired
      from `Trading.InstIDs`/`PaperTrading.TokenBudgetUSD`. Against rl_service's existing fail-safe
      (no model loaded -> flat action, all adjust fields zero), this is a real no-op end-to-end
      loop today. Not yet run against a live OKX feed — that verification is still open (§15.9).
- [x] Persist the actual observation vector sent at decision time (not just the outcome) alongside
      `paper_orders`: `PaperTrader.evaluateStrategies` now marshals the same `domain.Observation`
      it would send to `/predict` into `FeaturesJSON` on order open, best-effort (never blocks
      opening the order) (§15.3, §15.8). Verified with a dedicated test asserting the persisted
      JSON round-trips as a valid `domain.Observation` with the right strategy/token fields set.
- [x] Warm-start replay training (§15.8's two-phase design: initialization-only replay, distinct
      from — and never a replacement for — continued live learning, which is unchanged from §2):
      `rl_service/env/replay_env.py`'s `ReplayEnv` pools all active tokens' real candle history
      (Postgres `candles`, written by `PaperTrader` since Phase 1 — no separate CSV pipeline) plus
      any strategy signals actually logged in `paper_orders.features_json` (empty/hold signal for
      unlogged quiet bars, matching live reality), and lets PPO do normal on-policy rollouts —
      the model's own current decisions against real market conditions, never a replay of past
      decisions. `rl_service/obs.py` extracted as the single shared observation-vectorization
      source of truth between `/predict` and the replay env, so they can never silently drift
      apart. `rl_service/data/postgres.py` (new `psycopg2-binary` dependency) is the read-only
      Postgres access layer. `train.py --warm-start` runs it, producing `models/ppo_global.zip`
      (`serve.model_path` in `config.example.yaml` updated to match). 5 new tests
      (`tests/test_replay_env.py`) verify shapes, the token-identity one-hot actually differs per
      token, multi-token sequencing, and a real PPO training run against the env completes and
      produces a loadable, predictable model.
- [ ] Live/continued-training mode (the second half of §15.8 — consuming real paper-trading
      outcomes as they accumulate, not just the warm-start replay) — not yet built; needs enough
      live paper-trading history to be meaningful, which is itself gated on running the Phase A
      no-op loop against a live OKX feed first (§15.9, still open).
- [x] `rl_service/serve/api.py`: loads the single global model, routes `/predict` for every token
      through it via the token-identity one-hot, rejects observation-schema-version mismatches with
      a 422 (§15.3, §15.8) — implemented as part of the v3 schema bump above.
- [ ] Per-token reward/PnL breakdown in training logs (not just aggregate) — the concrete detection
      mechanism for the "good on average, bad for one token" failure mode (§15.2, §15.5); required
      before Phase B expands token count. Meaningful once the live/continued-training mode above
      exists — warm-start alone doesn't yet produce a per-token-attributable training signal.
- [x] A/B comparison tooling for baseline vs. rl_adjusted forks (§15.4):
      `port.Repository.SLTPAdjustmentStats` (aggregate win-rate/PnL per variant, filterable by
      instrument and a `since` lower bound — "the last week" per §15.4 — only counting baseline
      orders that actually have a fork, so unpaired baselines don't dilute the comparison) and
      `ListSLTPAdjustmentPairs` (trade-level pairs) implemented in Postgres with unit tests against
      the fake repository; exposed via `cmd/api` (`GET /api/sltp-adjustments/stats`,
      `GET /api/sltp-adjustments/pairs`); `panel/` gains an "SL/TP A-B" tab
      (`SLTPComparisonPage.tsx`) showing both variants' closed-trade counts/win-rate/PnL side by
      side plus the paired-trades table. The "decide after ~a week" call itself is still yours to
      make by reading this page — no automated promote/reject action is taken on the comparison.

Update the checklist above as work progresses.

## 15. RL training architecture: one global agent, reasoning over price + strategy signals

This section is the concrete design for turning the frozen/no-op RL service (§2, §11.2) into an
actually-trained decision-maker. Decided 2026-08-26, **revised 2026-08-26** (same day: the
original per-token-agent framing was replaced by a single global agent after a design discussion —
see §15.1's "Rejected/superseded" note for why, kept rather than deleted since the tradeoff
reasoning is still relevant if per-token agents are revisited later at higher token counts).
Revisit only with an explicit reason (new hardware, materially more paper-trading data, or
evidence a design assumption below was wrong).

### 15.1 Core decision: one single global PPO agent, not one per token

Explicit product decision, superseding the original "one PPO per token" framing from earlier the
same day: **one shared policy, trained on the pooled experience of every active token, with token
identity as an observation input** rather than a separate model per token.

Why the switch: the per-token design was chosen primarily to solve credit-assignment /
specialization concerns, but at the actual data volumes here (tens of paper trades/day per token
in Phase A with only 2 tokens live) per-token agents are each individually data-starved, and a
single agent pooling all tokens' experience learns meaningfully faster and can transfer general
patterns (e.g. "reduce leverage in high volatility") across tokens before any one token has
produced much experience on its own. The specialization loss this trades away (a shared policy
converging toward "good on average" rather than "excellent per token," especially early in
training before the token-identity input is meaningfully learned) is an accepted, explicit
tradeoff for Phase A — not an oversight. Revisit toward per-token (or a warm-started/fine-tuned
hybrid — pretrain global, then fine-tune per-token copies from those weights) once (a) enough
tokens are live that pooled-vs-per-token data volume no longer favors pooling, or (b) evidence
shows the global agent's per-token performance (tracked individually, not just in aggregate — see
§15.5) is diverging in a way that hurts a specific token.

**What did NOT change**: strategies are still "a tool the agent uses," never the decision-maker
themselves — see §15.3's raw-price-context addition, which is a separate, related correction (the
observation must let the agent reason about price action directly, not only through strategies'
interpretation of it) made in the same revision.

**Resource math**: SB3 PPO's memory/CPU cost is dominated by the rollout buffer and parallel envs
*during an active training update* (`n_steps × n_envs` transitions resident, ~1 core saturated),
not by how many trained policies exist on disk or serve inference — a loaded-for-inference-only
PPO policy (small-to-medium MLP given the wider price-context observation, §15.3) costs well under
1GB RAM and near-zero CPU per `/predict` call. One global agent training is trivially affordable on
8 cores/16GB; this was never the binding constraint even under the per-token design (which fit
too, just with more moving parts) — the real reason for the switch is sample efficiency, not
resource limits.

### 15.2 Phased rollout (start small, expand only with evidence)

- **Phase A (start here):** 2 tokens (BTC-USDT-SWAP, XAU or its OKX equivalent instrument — verify
  the exact `instId` exists on OKX SWAP before wiring it in), ≤5 of the 12 strategies (pick the
  ones with the cleanest/most orthogonal signals — e.g. avoid shipping two near-duplicate
  moving-average-cross variants both in the initial 5), all 3 timeframes (5m, 15m, 1h) folded into
  the one global agent's observation, token identity as an explicit input field (one-hot over
  active tokens is enough at this scale — a learned embedding is unnecessary complexity for 2-10
  tokens). One model: `models/ppo_global.zip`.
- **Phase B:** expand token count toward the full ~10-token roster and/or the full 12-strategy
  roster, gated on the global agent showing real learning signal (reward trending up **per token**,
  not just in aggregate — §15.5) before adding more tokens on top of it.
- Watch specifically for the failure mode described in §15.1: aggregate reward improving while one
  token's individual performance degrades (e.g. a dominant-volume token's patterns overwriting a
  smaller token's). This is the concrete signal that would justify revisiting per-token or a
  fine-tuned-per-token-from-global-weights approach — don't wait for it to become an obvious loss
  before checking per-token breakdowns.
- Do not add a 4th timeframe or expand tokens/strategies "just because it's easy" — each addition
  grows the one global agent's observation width (§15.3) and the strategy-selection action width
  (§15.4); re-validate resource usage and per-token reward breakdown after each expansion, not just
  once at the end.

### 15.3 Observation space (one global agent, per inference call)

One flattened vector, built by `go-engine/internal/rlclient` from live state and sent to
`POST /predict` (extending the existing `domain.Observation`/rlclient.Action shapes, §11.2 — the
schema on both sides must stay in sync, same as today):
- **Token identity**: one-hot over the currently active token roster (Phase A: 2 slots) — this is
  what lets one shared policy still condition its behavior per token.
- **Raw price context** (added in this revision — see rationale below): a recent window of close
  prices per active timeframe, normalized as returns (not raw dollar values, which don't
  generalize across price regimes/tokens) — e.g. the last N closes' pct-change series. This sits
  *alongside*, not instead of, the derived features below, and exists specifically so the agent can
  reason about price action/shape on its own, independent of what any strategy chose to report.
  Also include a couple of cheap positional/distance features (distance from current price to
  recent swing high/low, distance to key moving averages, distance to the open position's own
  SL/TP) — cheap proxies for "how close is price to a level a discretionary trader would watch,"
  the kind of context a junior trader watching candles picks up visually.
- **Per-timeframe strategy signal block**, repeated for each active timeframe (5m/15m/1h in Phase
  A): for each assigned strategy — `Side` (encoded -1/0/1), `Confidence`, `SLPct`, `TPPct` (already
  emitted by `strategy.Signal`, §9) — plus that timeframe's own derived volatility/momentum
  features (reuse `rl_service/data/features.py`'s `FEATURE_COLUMNS`). Strategies remain **one input
  among several**, not the sole gate on what the agent can see or act on — the agent must be able
  to weigh raw price action against what a strategy is saying, including disagreeing with every
  strategy, which requires the raw-price-context field above to actually exist.
- **Account/position tail**: current exposure, current leverage, unrealized PnL %, equity ratio for
  *this token's* allocated sub-budget (§15.6) — not total account equity, since per-token reward
  attribution (§15.5) requires the agent to see the capital constraint it's actually operating
  under for that token, even though one policy serves all tokens.
- **Recent-performance-of-this-token tail** (needed for §15.7's "give it another chance" behavior
  to be learnable rather than hardcoded): a short rolling window of this token's own recent
  realized trade outcomes (e.g. last N paper_orders' PnL, win/loss) and time-since-last-loss — lets
  the agent itself learn to size down after a losing streak and back up after recovery, per token,
  despite sharing one policy.
- **Live mid-price** (`MidPrice`, unchanged from the original design): always present, always the
  current tick price — required for SL/TP-adjust decisions and PnL math regardless of any of the
  above. This was already correctly wired before this revision; not a new addition.

Keep the observation schema **additive and versioned** (`schema_version`, already implemented in
`ObservationSchemaVersion`/`OBSERVATION_SCHEMA_VERSION`) — Phase B's timeframe/strategy/token
expansion will change the vector width, and `/predict` rejects a shape it wasn't trained for
(already implemented as a 422 in `rl_service/serve/api.py`) rather than silently misaligning
features, which would corrupt training invisibly. Bump the version again once the raw-price-context
fields land in code (this revision moves the design from v2 to a v3 shape — implementation should
land the version bump together with the field changes, not separately).

### 15.4 Action space (per-request output, one shared policy)

Today's `Action{TargetExposure, LeverageFrac, Confidence}` (§2) becomes, per token:
1. **`strategy_weights`**: one continuous value per assigned strategy (softmax'd or clamped to
   [0,1] and normalized) — how much the agent trusts each strategy's current signal *right now*,
   combined with each strategy's `Confidence` to produce a single effective directional signal.
   This is what "combine the signal of multiple strategies" (per your original ask) resolves to
   concretely: a learned weighting, not a fixed voting rule.
2. **`target_exposure`** (unchanged, [-1, 1]): resulting position as a fraction of *this token's*
   max allowed notional, sign = side.
3. **`leverage_frac`** (unchanged, [0, 1]): mapped to `[1x, MAX_LEVERAGE]` — note your target range
   is 10x-100x, materially higher than the current `EnvConfig.max_leverage` default of 5.0 and
   `config.example.yaml`'s `risk.max_leverage: 5`; both must be raised together for Phase A,
   understanding that §5's hard risk caps (independent of RL, Go-side, non-overridable) are what
   actually bound worst-case loss — the RL agent proposing up to 100x is safe only because those
   caps clamp it, never trust the agent's own leverage choice as the safety boundary.
4. **`sl_adjust_pct`, `tp_adjust_pct`** (new, continuous, both allowed negative/positive within a
   clamped range e.g. ±2% per decision step): in-trade adjustments to the *open* position's SL/TP,
   evaluated on the same cadence as `monitorOpenOrders` (every tick) or throttled to e.g. once per
   candle close to avoid overreacting to noise — start with candle-close cadence in Phase A, tick
   cadence is a possible later tightening once the behavior is validated. This is what "trail SL
   into profit when safe" (per your ask) resolves to: not a hardcoded trailing-stop rule, but a
   learned adjustment the agent proposes and the reward function (§15.5) judges after the fact via
   whether it improved or hurt realized outcomes — exactly as you asked ("it should be screened by
   the model and trained, is it good or not").
   - Hard constraint regardless of what the agent proposes: an SL adjustment can never *widen* risk
     past the position's original risk budget, and can never move SL to a worse (more losing) price
     than a prior tightening already reached — i.e. only ratchet toward locking in profit / reducing
     risk, never away from it. Enforced as a pure Go-side clamp, `usecase.RatchetSLTP`
     (`internal/usecase/sltp_ratchet.go`), the same non-negotiable pattern as §5's other hard
     limits — don't rely on the trained policy alone to have learned not to do this. Any adjustment
     proposal is also magnitude-clamped to `usecase.MaxSLTPAdjustPct` (±2%) before the direction
     check, so a single decision step can only move SL/TP a bounded amount regardless of what the
     model outputs.
   - **Shadow-fork mechanic, not in-place editing** (explicit product decision): when the agent
     proposes a nonzero SL/TP adjustment on an open order, the original order is never edited.
     Instead a linked copy ("fork") is created — same inst_id/side/entry_px/strategy_id/size/
     leverage/opened_at, but with the ratcheted SL/TP applied — and both the original
     (`variant='baseline'`) and the fork (`variant='rl_adjusted'`, `parent_order_id` set) are
     monitored independently to completion against the live price feed. This is deliberate: it
     turns every adjustment decision into a same-entry, same-signal A/B (baseline vs.
     RL-adjusted) whose outcomes can be compared afterward (win rate, PnL) — the intended
     comparison window is roughly a week's worth of paired trades, decided by the operator or later
     folded into training as evidence for whether adjusting helped — rather than the adjustment
     silently overwriting ground truth about what the un-adjusted order would have done.
     `port.Repository.ForkPaperOrderWithSLTP` implements the clone; `usecase.PaperTrader.
     adjustOpenOrdersWithRL` (called at candle-close cadence, gated on `PaperTrading.RLSLTPAdjust`)
     is the call site. A fork is **tracking-only**: it is explicitly excluded from token
     budget/reward accounting (§15.6/§15.7) — only its baseline parent's realized PnL counts
     toward the token's real running budget, so one signal never draws down the budget twice.

### 15.5 Reward shaping

Extends the existing `okx_futures_env.py` shaping (realized PnL − fee/funding − drawdown penalty −
liquidation-proximity penalty, §2) with:
- A small penalty on `sl_adjust_pct`/`tp_adjust_pct` churn (e.g. proportional to the number of
  adjustments per trade) so the agent doesn't learn to twitch the SL every tick for free — every
  adjustment should earn its keep in realized outcome, not be free to try.
- The per-step/per-trade reward the global agent actually trains on is still computed **per
  token** (that token's own realized PnL/equity, not a blended cross-token number) — this doesn't
  change under the global-agent design (§15.1); pooling happens at the *training data* level (all
  tokens' transitions go into one shared policy update), not at the *reward computation* level.
  Conflating these two would make a profitable BTC trade mask a losing XAU trade in the numbers
  the agent sees for that XAU transition, which defeats the point of tracking reward at all.
- **Required monitoring, not just training-time reward**: because one policy now serves every
  token, track and log realized PnL / reward **broken out per token** (not only the aggregate
  training curve) from day one — this is the concrete mechanism for detecting the "good on average,
  bad for one token" failure mode §15.1/§15.2 call out. Surfacing this on the panel (§11) is a
  reasonable later addition; at minimum it must be visible in training logs before Phase B expands
  token count.

### 15.6 Capital allocation across tokens

**Fixed, not learned, in Phase A**: config-level per-token notional (your stated $10/token against
a <$50 total budget), not a decision the RL agent makes. Rejected letting even the (now single,
shared) agent dynamically reallocate capital across tokens for Phase A specifically because it
would make the per-token reward signal (§15.5) depend on a capital-allocation decision made from
the same pooled policy update — moving capital away from a temporarily-losing-but-still-learning
token would starve it of the very trades it needs to keep contributing to the shared policy's
training data, compounding rather than isolating a bad early streak. This is an explicit "later"
item (add a §14 roadmap entry only once there's a real per-token track record to allocate against),
not a "we'll get to it eventually, unscoped."

### 15.7 Handling a token's budget going to zero/negative ("give it another chance")

Per your explicit ask: a token whose $10 sub-budget is drawn down to zero (or below, if fees push
it negative) must not be permanently benched — it should get reset and get another shot, since a
losing streak early in training is expected/noisy, not necessarily evidence the token/strategy mix
is bad. Concretely:
- Track equity **per token**, not just per paper order (a new small piece of state — likely a
  `token_budgets` table or a computed running value from that token's `paper_orders`, TBD at
  implementation time) so "this token is at zero" is a fact the system can act on.
  - The recent-performance tail in the observation (§15.3) is what lets the *agent itself* learn to
    size down approaching zero, rather than needing a hardcoded halt.
- **Reset condition** (a hard Go-side rule, not RL-decided — this is a bookkeeping/risk-adjacent
  action, same "don't trust the model to have learned this" reasoning as §15.4's ratchet
  constraint): when a token's running budget hits zero/negative, top it back up to the configured
  per-token notional and record the reset (so training-run analysis can see how often this
  happens, and it doesn't look like unlimited free money if it resets constantly with no
  learning — a token resetting every day is itself a signal something's wrong with that
  token/strategy mix, worth surfacing on the panel eventually, not just silently papering over).
- This is paper/demo-mode behavior. Real-money trading (§14, not yet wired) must NOT auto-reset a
  drained budget the same way — that's real capital, and running out is a stop condition requiring
  a human decision, not an automatic top-up. Keep this distinction explicit whenever real-mode
  wiring happens; don't let the paper-mode reset logic leak into the real-mode path by accident.

### 15.8 Training loop shape: warm-start replay + continued live learning

**Two distinct phases, not one — this distinction matters and must not be collapsed:**

1. **Warm-start (initialization only, not evaluation)**: PPO is on-policy — it learns by rolling
   out its *own current* decisions against an environment and observing the outcome, not by
   replaying a fixed log of decisions someone/something else made (most of the paper-trading log
   so far reflects the still-untrained/no-op model or raw strategy signals, not the policy being
   trained). So training directly from logged `(state, action, reward)` tuples the way an
   offline-RL algorithm would is the wrong tool for PPO. Instead: a **replay environment**
   (`rl_service/env/`, a new Gymnasium env alongside — not replacing — `okx_futures_env.py`) plays
   back the *sequence* of real market conditions already persisted in Postgres's `candles` table
   (real OHLCV, written by `PaperTrader` since Phase 1 — not a separate historical CSV pipeline
   like `okx_futures_env.py`'s), reconstructing the same strategy-signal/price-context observation
   shape `rlclient` builds live (§15.3), for every active token in sequence. Critically, the
   **actions taken during these rollouts are the policy's own current choices**, produced fresh at
   each step exactly like a live rollout — only the *market conditions* are historical, not the
   decisions. This is standard on-policy PPO training against real data, not backtesting: nothing
   about this phase evaluates or scores the model against history, and its output is never treated
   as "proof" the model is good — it only gets the model past pure-random initialization before
   real capital (even paper capital) is put behind its decisions.
2. **Continued live learning (unchanged from §2's original decision)**: the warm-started model is
   then the *same* model that keeps training from live paper-trading outcomes going forward — not
   a separate "final" model swapped in afterward. §2's core commitment (the deployed model's real
   training signal comes from live forward-test data, never from replayed history) is fully
   preserved; the replay phase only changes where the *first* few updates' gradient signal comes
   from, given a freshly-initialized network would otherwise spend a materially long stretch of
   scarce live paper-trading data at close to random behavior. Revisit only if evidence shows this
   warm-start biases the model toward historical patterns in a way live learning doesn't correct
   for.

Both phases pool **all active tokens** into the one global agent's training data (token identity
is an observation field, §15.1 — no per-token filtering or separate models). Concretely:
`train.py` gains a `--warm-start` mode that builds the replay env from Postgres (`candles` +
re-evaluating each token's assigned strategies via the same `strategy` package logic Go uses, so
the replayed observations match what `rlclient` actually sends in production) and runs standard
`model.learn()` against it; a live/continued-training mode (built once §15.9's end-to-end no-op
loop has accumulated real paper-trading history) consumes the *actual observation vectors logged
at decision time* (§15.3's schema — needs persisting, not yet done: extending
`paper_orders.features_json` or a new table, TBD at implementation) paired with their realized
outcomes. Either mode produces `models/ppo_global.zip`, loaded by `rl_service/serve/api.py` and
used to answer every token's `/predict` calls (the request's `inst_id` selects which
token-identity input to set, not which model to load — see §15.1 for why this is one shared policy
rather than per-token model files).

### 15.9 Implementation phasing (tracked in §14 going forward)

This section is design; §14's checklist is where actual implementation progress against it is
tracked. The first concrete slice: extend `strategy.Signal`/`domain.Observation`/`rlclient.Action`
for the new fields (§15.3-15.4, including the raw-price-context fields and token-identity one-hot
from this revision), then wire Phase A's 2 tokens end-to-end (signal → observation → `/predict`
with a still-untrained/no-op model → paper order → SL/TP-adjust path → per-token budget/reset
bookkeeping) before touching `train.py` — get the full loop running with a no-op model first
(matching the existing fail-safe pattern in `rl_service/serve/api.py`), same incremental-and-
verified approach used for every phase so far (§14).
