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
details. One Phase 1 item remains open, tracked here since it's blocked on external state rather
than done:
- [ ] Repoint `rl_service/train.py` at the paper-trading trade log instead of historical replay
      (blocked on accumulating real paper-trading data first)

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

Update the checklist above as work progresses.
