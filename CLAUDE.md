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
├── docker-compose.yml         # kafka + redis + timescaledb + prometheus/grafana + loki/promtail
│                                 + go services + python services
├── loki-config.yml            # log aggregation storage/retention, §11.7
├── promtail-config.yml        # tails Docker json-file logs off disk -> Loki, §11.7
├── prometheus.yml             # metrics scrape config, §11.6
├── grafana/provisioning/      # auto-provisioned Prometheus + Loki datasources, §11.6/§11.7
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
│   │   ├── kafkastream/           # Kafka producer/consumer + instId dispatcher — the event bus,
│   │   │                           §12; implements port.MarketDataConsumer/Publisher
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

Three datastores, different jobs — this is not redundant, each is used for what it's good at:
- **Kafka** — the ephemeral real-time event bus (§12): ticks/candles/paper-order events in transit
  between the ingestor and consumers (paper-trader, strategy-optimizer, cmd/api's WebSocket
  bridge). Time-based retention (24h default, `KAFKA_LOG_RETENTION_HOURS`), not meant for
  long-term storage — this replaced an earlier Redis Streams implementation (see §12).
- **Redis** — now only `internal/optimizer.TrialStore`'s disposable, TTL'd trial state (§16.3):
  candidate strategy-parameter trials the optimizer runs, deliberately never persisted to Postgres.
  Unrelated to the event bus.
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
3. The engine subscribes to the live price feed (via the Kafka event bus, §12) and monitors the
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
  `ExchangeClient`, `internal/postgres` implements `Repository`, `internal/kafkastream` implements
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
  through a `cmd/api` endpoint, filterable to error-level lines — this remains the *live tail* path
  for the RL model-status panel specifically. **Revised 2026-08-27**: a lightweight log-shipping
  stack (Grafana Loki + Promtail, §11.6) was added on top of this for cross-service historical
  search/filtering — the original "journald/docker's own log store is enough" call didn't account
  for wanting to *query* (not just tail) errors across services after the fact. ELK was
  deliberately rejected in favor of Loki: Elasticsearch alone is the single heaviest component
  measured anywhere in this stack (§15.1), and Loki's label-only indexing (not full-text) is a
  better fit at this project's log volume while still slotting into the Grafana UI already in use.
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
  position opened, (b) closed by SL, (c) closed by TP. **Implemented as a real-time WebSocket
  push, not polling-only** (revised from the original "polls/subscribes" framing once Kafka
  landed as the event bus, §12): `cmd/paper-trader` publishes a lightweight
  `usecase.PaperOrderEvent{Type, OrderID, InstID}` to the `okx.paper-order-events` Kafka topic on
  every open/close; `cmd/api` consumes that topic (consumer group `"api-ws-bridge"`) and
  broadcasts each event to every connected panel client over `GET /api/ws`
  (`internal/api/ws.go`'s hand-rolled hub — no pub/sub library needed at this scale). The panel's
  `usePositionEvents` hook (`panel/src/hooks/`) triggers an immediate `GET /api/positions` refetch
  on each pushed event; `usePositionAlerts`' existing snapshot-diff logic then fires
  `new Audio(...).play()` + the browser Notification API off that fresher data — the diffing logic
  itself didn't need to change, only what triggers it. The original 5s poll (`usePolling`) still
  runs as a fallback/consistency check independent of the socket's connection state. No backend
  "notification service" beyond the hub above was needed.

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

### 11.7 Logs (implemented): Grafana Loki + Promtail

**Added 2026-08-27**, revising §11.2's original "no separate log-shipping stack for v1" call —
that call covered *live-tailing* a single service's logs (still true, §11.2 unchanged for that),
but didn't address wanting to *search/filter* errors across services after the fact, which is a
different, genuinely useful capability journald/`docker logs` alone don't give you. ELK was
considered and rejected: Elasticsearch alone is the single heaviest component measured anywhere in
this stack (§15.1's resource-math precedent), and Loki's label-only indexing (not full-text) fits
this project's actual log volume while reusing the Grafana UI already deployed for metrics — one
pane of glass for both, not two separate tools to learn.

- **`loki`** (`loki-config.yml`, filesystem storage on the `loki-data` volume — no object store,
  appropriate for one VPS not a multi-node deployment) and **`promtail`** (`promtail-config.yml`)
  added to `docker-compose.yml`. No application code changes were needed at all: every Go service
  already logs structured `logfmt` via `slog.NewTextHandler` (§6) to stdout, which Promtail's
  `logfmt` pipeline stage parses to promote `level` into a real, queryable Loki label
  (`{level="ERROR"}` in Grafana Explore/LogQL) — this is the concrete mechanism behind "save/query
  each service's errors separately." Non-Go containers (Python services, Postgres, Redis, Kafka)
  still have their raw log lines land in Loki, just without a parsed `level` label (the logfmt
  stage no-ops, not errors, on non-matching lines) — full-text search still works on those via
  LogQL's `|= "text"` filter.
- **File-based scraping, not Docker service discovery** — this was a real bug caught and fixed
  during live verification, not a design choice made up front. The first implementation used
  Promtail's `docker_sd_configs` (live-container discovery, refreshed every 5s), labeled with a
  `{container_name} -> service` relabel rule. Live-testing it against a real crash
  (`cmd/paper-trader` exiting ~2s after start on a bad `PUMP-USDT-SWAP` instrument ID already
  present in `config.yaml`, unrelated to this logging work) showed the crash's own error log
  **never made it into Loki at all** — the container had already exited and dropped out of
  `docker ps` before Promtail's discovery loop ever found it, silently losing exactly the kind of
  log this feature exists to capture. Fixed by switching `promtail-config.yml` to scrape Docker's
  own `json-file` log driver output directly
  (`/var/lib/docker/containers/*/*-json.log`, mounted read-only into the `promtail` service in
  `docker-compose.yml`) — those files persist on disk regardless of whether the container is still
  running, so a fast crash's logs are captured the same as a long-lived service's. Re-verified with
  a second, deliberately fresh crash of the same service: the error was queryable in Loki
  (`{container_id="...", level="ERROR"}`) within ~3 seconds.
  - **Tradeoff of this fix**: the file-based approach only labels by `container_id` (the 64-char
    hex id embedded in the log file's own path) since deriving a human-readable `service` label
    from a file glob needs a second Docker API lookup that would either duplicate ingestion (a
    second scrape job) or add a dependency this scrape config deliberately doesn't have. Resolve
    an id to a name with `docker inspect <container_id> --format '{{.Name}}'`, or skip the lookup
    and just filter on `filename` (which embeds the same id) or full-text search
    (`|= "paper-trader"` works fine against most log lines even without a label for it).
- **Grafana datasource auto-provisioning**: `grafana/provisioning/datasources/datasources.yml`
  registers both Prometheus and Loki on Grafana startup — no manual "Add data source" step after a
  fresh `docker compose up`, matching the "foundations first, minimal manual setup" pattern used
  elsewhere in this project.
- **Retention**: 14 days (`loki-config.yml`'s `retention_period: 336h`) — bounded so disk usage on
  a small VPS doesn't grow unbounded; adjust directly in that file if a longer/shorter window is
  needed later.
- No dashboard/Explore-view saved searches are pre-built yet (mirrors §11.6's own "dashboards
  aren't built yet, add them once there's real data" framing) — querying today means using
  Grafana's Explore tab against the Loki datasource directly.

## 12. Event-driven design

Real-money live trading benefits from reacting to events (fills, price ticks, risk breaches)
rather than polling. The ingestion layer is event-driven (OKX WS push → Kafka topic), and internal
components (paper-trading engine, strategy-optimizer, the panel's WebSocket bridge) are written as
handlers subscribing to that bus. Code against the `port.MarketDataConsumer`/`MarketDataPublisher`
interfaces (§10) so the transport itself stays swappable behind those two methods.

**Kafka, not Redis Streams** (revised 2026-08-27 — this project started on Redis Streams, which
worked fine at this scale; the migration to Kafka was a deliberate choice for a public/portfolio
repository, not a response to any Redis Streams limitation actually hit in practice. If this
weren't a resume piece, Redis Streams would still be the pragmatic choice for a project this
size — that tradeoff is worth stating plainly rather than pretending Kafka was required):
- `internal/kafkastream` (`github.com/segmentio/kafka-go` — pure Go, no cgo/librdkafka, simpler
  Docker builds than `confluent-kafka-go`) implements `Publisher`/`Consumer` against
  `port.MarketDataConsumer`/`MarketDataPublisher`, same shape as the Redis Streams implementation
  it replaced. `Publish(ctx, key, event)` takes an explicit partition key (Redis Streams never
  needed one) — every publisher keys by `instId`, so one instrument's events stay strictly
  ordered within their own partition.
- **Topics**: `okx.tickers` (all instruments, keyed by instId), `okx.candles.<bar>` (one topic per
  timeframe, e.g. `okx.candles.1m`, also keyed by instId), `okx.paper-order-events` (paper-order
  open/close notifications for the panel's WebSocket bridge, §11.4). Auto-created on first publish
  (`AllowAutoTopicCreation`) — a deliberate v1 simplicity choice, not a scale-tested default.
  Retention is time-based (`KAFKA_LOG_RETENTION_HOURS`, 24h default in `docker-compose.yml`),
  replacing Redis Streams' count-based `MAXLEN ~100_000` cap.
- **Consumer groups map directly onto the old Redis Streams group names** (`"paper-trader"`,
  `"strategy-optimizer"`, plus a new `"api-ws-bridge"` for the WebSocket bridge, §11.4) — each
  reads the same topics independently, same as before.
- **Partitioning vs. the old per-instrument consumer identity**: Redis Streams' `XREADGROUP` let
  each service run one consumer *identity* per instrument (`ConsumerName = instId`) within one
  shared group, each instance filtering the stream down to its own instId in the handler. Kafka
  consumer groups instead auto-assign whole *partitions* to readers in the group — there's no
  per-instrument consumer identity to keep. So each service now runs **one shared reader per
  topic**, and `internal/kafkastream.Dispatcher` fans out that reader's messages to
  per-instrument handlers by decoding just the `instId` field and routing in-process
  (`Dispatcher.Register`/`ForInstrument`) — the actual `handleTick`/`handleCandle` logic in
  `usecase.PaperTrader` and `cmd/strategy-optimizer` didn't need to change, only the outer
  consumer-wiring loop in each `main.go` collapsed from N consumers to 1 dispatcher per topic.
- **Ack/commit semantics preserved as-is**: `Consumer.Run` commits a message's offset after
  `handler` returns regardless of whether it errored — matching Redis Streams' old unconditional
  `XAck`-after-handler behavior (no DLQ/retry either before or after this migration; a bad message
  is not retried forever, `handler` is responsible for its own logging).
- **Redis didn't go away** — `internal/optimizer.TrialStore`'s disposable trial-parameter state
  (§16.3) stays on Redis, since that's a KV/TTL use case Kafka doesn't fit, not part of the event
  bus this section describes.
- **Deployment**: single-broker Kraft-mode Kafka (`apache/kafka` image, no separate Zookeeper) in
  `docker-compose.yml` — appropriate for a portfolio project's scale, not a production multi-broker
  cluster. One deployment gotcha worth documenting: `KAFKA_LISTENERS` must use bare-colon binds
  (`PLAINTEXT://:9092`), not an explicit `0.0.0.0` host — the image's config tool rejects
  `0.0.0.0` there with "advertised.listeners cannot use the nonroutable meta-address," even though
  only `KAFKA_ADVERTISED_LISTENERS` is what actually gets advertised to clients (live-verified
  against a real running broker while building this). Separately, live-verified end-to-end
  publish/consume against a fresh broker: the very first publish to a brand-new topic can fail
  once with "Unknown Topic Or Partition" (a real kafka-go timing quirk — the client's write races
  the broker's own auto-created-topic metadata propagation) before succeeding on the next publish;
  this is a one-time, self-resolving startup blip, not a persistent problem, and is already covered
  by every publisher's existing "log a warning and continue" pattern (no publish call in this
  codebase treats a publish failure as fatal).

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
each, publishing every event to Kafka (§12 — originally Redis Streams, migrated later). The Paper
Trading Engine consumes both via consumer groups (`internal/kafkastream.Consumer`/`Dispatcher`):
every **tick** triggers an immediate SL/TP check against open virtual orders (no missed intra-bar
wicks, no polling delay), and every **finalized candle** triggers strategy re-evaluation and is
persisted to Postgres. A REST call (`GetCandles`) is used exactly once at startup, to seed the
initial in-memory candle window — not on an ongoing poll loop. This replaces the earlier
REST-polling version, which had a real correctness bug (checking SL/TP only against candle-close
prices could silently miss a price wick that touched SL/TP and reverted within the same bar) in
addition to rate-limit/delay concerns.

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
- [x] Migrated the event bus from Redis Streams to Kafka (§12) — a deliberate portfolio-value
      choice (public GitHub repo), not a response to a scale limit actually hit. Added a real-time
      WebSocket bridge (`GET /api/ws`, `internal/api/ws.go`) off the new `okx.paper-order-events`
      topic at the same time, replacing the panel's polling-only open/SL/TP alerting (§11.4) with
      a push while keeping the 5s poll as a fallback.

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
- [x] Audit MidPrice's live-tick freshness across the SL/TP-adjust and open-order-decision paths
      (explicit user requirement, 2026-08-27) — **found genuinely stale, not just re-verified**:
      `adjustOpenOrdersWithRL` was only ever called from `handleCandle` with `c.Close` (the
      finalized candle's close), never from `handleTick`, so the RL model's SL/TP-adjust decision
      could reason about a price up to one full bar interval stale (e.g. up to 15 min on a 15m bar)
      while `monitorOpenOrders` (the actual SL/TP-touch execution check) was correctly checking the
      live tick every tick the whole time — the model just never got to react to what execution
      already saw. Fixed: `adjustOpenOrdersWithRL` now runs from `handleTick`
      (`internal/usecase/papertrade.go`), throttled to `RLAdjustInterval` (2s, a fixed wall-clock
      throttle chosen over tick-count throttling so the effective call rate stays predictable
      regardless of OKX's own tick rate) via `shouldRunRLAdjust`'s mutex-guarded
      check-and-claim — bounds `rl_service` inference load without reintroducing candle-close-scale
      staleness. `evaluateStrategies`' new-order-open path deliberately stays candle-close-driven
      (§9: strategies are candle-driven by design; only the in-trade adjust decision needed the
      live-tick fix). Also renamed `MidPrice` → `LastPrice` (`domain.Observation`/`obs.py`'s
      `mid_price` → `last_price`, both sides kept in sync) since "midpoint" was never accurate —
      it's always been OKX's tickers-channel `last` trade price. 2 new tests
      (`TestHandleTick_TriggersRLAdjustOnLiveTickPrice`, `TestHandleTick_RLAdjustThrottled`) cover
      the behavior the old test suite had zero coverage for (the existing
      `TestAdjustOpenOrdersWithRL_*` tests call the adjust function directly, bypassing
      `handleTick` entirely, so they never exercised cadence).
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

Phase 6 — strategy parameter optimizer (§16, implemented):
- [x] `cmd/strategy-optimizer`: a long-lived Go service (not a one-shot CLI — see §16.7's revision
      of §16.4), full trial lifecycle against real `strategy.Strategy` values via `strategy.
      Factories`/`WithParams` (no reimplementation), Redis-backed disposable trial state
      (`internal/optimizer.TrialStore`/`OpenTrial` — never `paper_orders`), tick-driven SL/TP-touch
      win/loss judgment reusing `usecase.SLTPTouchReason` (extracted from `papertrade.go`'s
      `closeReason` into a shared exported helper so PaperTrader and the optimizer judge trials by
      the exact same domain logic, §16.3). Pure trial-scoring/candidate-selection logic
      (`CandidateResult`, `EligibleCandidates`, `BestCandidate`, `ShouldPersist`) lives in
      `internal/optimizer/scoring.go`, unit-tested without Redis; `internal/optimizer/runner.go`'s
      `Run` type owns one time-boxed run's lifecycle (`EnsureCandidates`/`EvaluateCandle`/
      `CheckTick`/`Finalize`); `cmd/strategy-optimizer/main.go` wires per-instrument tick/candle
      consumers, the scheduler, and `POST /optimize`/`GET /status`.
- [x] Python/Optuna candidate-proposal sidecar (`optimizer-service/`, its own FastAPI app, no
      shared code with `rl-service` — it has no torch/CUDA concern at all, §16.2) + the Go<->Optuna
      `/suggest`/`/report` call boundary (`internal/optimizer.SidecarClient`), using Optuna's
      `ask`/`tell` API (not `optimize()`) since Go drives the trial loop, keyed by opaque per-
      candidate `trial_id`s (§16.6).
- [x] Winning-candidate persistence as durable sub-strategy rows + assignments, reusing
      `CreateStrategy`/`CreateAssignment` (§16.3 step 5, §11.3), plus a new `strategy_param_changes`
      table (migration `000005`) + `Repository.RecordParamChange`/`ListParamChanges` logging every
      change (optimizer- and manual-panel-sourced alike) for the Strategies page's new price-chart
      marker overlay (§16.7).
- [x] `rsi_sma_fuzzy` fuzzy-confidence sub-strategy variant (§16.5): trapezoidal membership
      functions replace RSISMA's hard threshold cliff (RSI=29.99 vs 30.01 no longer flips the
      decision discontinuously) — `oversoldMembership`/`overboughtMembership` ramp linearly across
      a configurable transition zone (`OversoldMin`/`OversoldMax`, `OverboughtMin`/`OverboughtMax`),
      with a `MinMembership` floor so the strategy doesn't emit noise-level signals on every candle.
      Registered in `Factories` as `"rsi_sma_fuzzy"`, a separate `strategy.Strategy` kind from
      `RSISMA` (not a modification — origins stay independently comparable, §11.3). 6 tests verify
      the membership ramp's exact values, that adjacent RSI steps never jump discontinuously (the
      actual "no cliff" property), buy-signal construction, the min-membership hold gate,
      `WithParams` repairing a degenerate/inverted zone, and factory registration.

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

**Measured, not estimated** (2026-08-27, on a 2015 MacBook i5/8GB via Colima, `rl-service`'s actual
Docker image, CPU-only PyTorch — see the Dockerfile note below): idle `rl-service` container ~180MB
RAM; a real PPO warm-start-shaped run (2 pooled tokens, `n_steps=2048`, `batch_size=64`) sustained
~430MB RAM and ~1 CPU core at 260-700 fps, completing 20,480 timesteps in 78s. Memory stays flat
regardless of `total_timesteps` (SB3's rollout buffer size is fixed by `n_steps`, not total
steps) — so `WarmStartConfig`'s default 50,000 timesteps is ~70-125s, not a long-running job, and
this fits comfortably even on hardware well below the original 8-core/16GB planning assumption.
Widening the observation (more strategies/timeframes/tokens, §15.2's Phase B) grows the policy
network slightly but the rollout-buffer-dominated memory profile above is the right order of
magnitude to plan around, not a hard ceiling that's already been hit.

**PyTorch's default wheel pulls in the full CUDA/nvidia-\* GPU toolkit** (several GB) even with no
GPU present — `rl-service/Dockerfile` installs the CPU-only build explicitly
(`--index-url https://download.pytorch.org/whl/cpu`) before `requirements.txt`, which is what kept
the image at 2.72GB instead of noticeably larger. This isn't optional on resource-constrained
hardware: the CUDA wheel's download/build was what originally exhausted a low-disk dev machine's
space mid-build.

**Full-stack measurement, not just rl-service in isolation** (2026-08-27, same machine, real
`docker compose` build of every service, `configs/config.yaml`'s actual 10-token roster, genuinely
connected to live OKX public WS — not a synthetic test):
- **Idle full stack** (ingestor + paper-trader + api + rl-service + redis + timescaledb, all 10
  tokens/6 timeframes actually streaming live OKX data, paper-trader actively opening real paper
  orders): **~290MB RAM combined, ~25-30% of one CPU core.** The Go services are the cheapest part
  by far — ingestor and paper-trader combined use under 25MB RAM each even ingesting 10 tokens ×
  6 timeframes of live WS data; Postgres is ~80MB, Redis ~15-40MB depending on stream backlog.
- **Adding Prometheus + Grafana** (`docker-compose.yml`'s monitoring stack, CLAUDE.md §11.6):
  +~260MB (Grafana ~235MB, Prometheus ~25-30MB) — brings the idle full stack (minus `trader`/
  `panel`, which need real OKX keys/aren't part of the trading-critical path) to **~585MB RAM,
  ~30% of one core.**
- **Peak, with a real 50,000-timestep warm-start-shaped training run (10 pooled tokens, matching
  §15.2 Phase B scale) running concurrently with the full live stack above**: `rl-service` briefly
  spiked to **~470% CPU** (PyTorch's internal BLAS/thread pool uses multiple cores during a
  training update step, not just the ~1 core rollout collection uses) and ~450MB RAM; every other
  service's usage was unaffected by the concurrent training load. The training run itself completed
  in **~207s (~3.5 minutes)** at ~246 fps — slower than the 2-token run above (260-700 fps) since
  10 tokens' pooled experience means more data per rollout, but still not a long-running job.
  Nothing else in the stack degraded or fell behind during this — ingestion kept up with live OKX
  data throughout.
- **Bottom line**: the whole trading-critical stack (everything except monitoring) fits in under
  1GB RAM and under half a CPU core at idle, even at full Phase B scale (10 tokens). A training run
  temporarily wants close to a full core (or more, briefly) but doesn't meaningfully compete with
  the trading-critical services for RAM. A budget 2 vCPU / 2GB RAM VPS has comfortable headroom for
  everything in this project at once, monitoring included — the original 8-core/16GB planning
  figure was a conservative upper bound, not a real requirement.

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
- **Live last-traded price** (`LastPrice`, renamed from `MidPrice` — see §15.9's audit item, which
  found this field was actually being fed a stale candle-close price on the SL/TP-adjust path
  despite this section's original "already correctly wired" note, and fixed it to the live tick):
  always present, always the current tick price — required for SL/TP-adjust decisions and PnL math
  regardless of any of the above.

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

## 16. Strategy parameter optimizer (independent of the RL model)

Decided 2026-08-27, after an extended design discussion. This is a **separate, independent
service** for tuning each built-in strategy's own numeric parameters (RSI period, MA lengths,
thresholds, etc. — see `strategy.ParamSpec`, §9) per token, using real market data — deliberately
decoupled from the RL agent (§15) so the two systems can never contaminate each other's signal.

### 16.1 Why this is separate from the RL agent, and why the RL agent doesn't do this itself

Two independent problems got conflated during early design and needed to be pulled apart:

- **What the RL agent (§15) does**: given a strategy's already-decided signal (side, confidence,
  SL%, TP%), learn how much to trust it, how to size/leverage the resulting position, and whether
  to adjust SL/TP in-trade. The RL agent never sees or touches a strategy's *internal* parameters
  (RSI period, MA length) — it only ever sees the strategy's *output*.
- **What this optimizer does**: given a strategy's *internal* parameters, find values that make its
  raw signal quality better (before the RL agent ever weighs in) — measured strictly by whether
  price touched the strategy's own suggested SL or TP, nothing else.

Explicit product decision on why these must stay separate (raised directly by the user): if the
RL agent's own trading decisions (position sizing, early closes, SL/TP adjustments) were allowed
to influence which strategy parameters get judged "good," two real problems follow — (a) a
good signal could be made to look bad by an unrelated RL sizing/timing decision layered on top of
it, and (b) the optimizer would be reasoning about a moving target, since the RL agent is itself
still learning. Keeping the optimizer's win/loss judgment strictly to "did price touch this
signal's own SL or TP" — never realized PnL, never anything the RL agent touched — removes RL as a
confound entirely. This is also why feeding raw, unprocessed indicator values (RSI number, MA
crossover event, etc.) directly into the RL network was rejected as the *general* signal-generation
approach (§15.3's design still keeps strategies as one input among several, not replaced) — a
crossover is a discrete event, not a continuous value, and averaging together indicators computed
over different lookback windows (e.g. RSI-14 and RSI-21) as separate raw inputs is not
meaningful without something to reconcile them; a raw-indicator-in, decision-out network would
have to rediscover technical analysis from scratch, which is a much harder learning problem than
weighting pre-computed opinions given this project's trade-volume constraints (§2). Fuzzy-logic
preprocessing (turning heterogeneous indicators into comparable, continuous membership degrees
before any model sees them) was discussed as a real technique for exactly this reconciliation
problem and is worth a dedicated sub-strategy experiment (§16.6) — but is not a prerequisite for
this optimizer, whose job is narrower (tune existing parameters, not reconcile heterogeneous raw
inputs into a new kind of signal).

### 16.2 Architecture: Go does the trials, Python only proposes candidates

**Engineering principle driving this split**: never duplicate domain logic across languages if it
can be avoided; only reach across language boundaries for a genuinely generic, off-the-shelf tool.
The 12 built-in strategies' logic is this project's actual domain knowledge and lives in
`internal/strategy/*.go` — reimplementing it in Python (the same trap warm-start's design
deliberately avoided, §15.8) would create two copies that drift the moment one is edited and the
other is forgotten. Bayesian optimization itself (which candidate parameter values to try next,
given prior trial results) is a generic, domain-independent algorithm with no mature off-the-shelf
Go library — Python's Optuna is mature and well-tested. So:

- **`cmd/strategy-optimizer`** (new Go service): runs potentially hundreds of parallel parameter
  trials per (token, base-strategy) pair, using the real `strategy.Strategy` interface directly —
  no reimplementation, no drift risk. Owns trial lifecycle, signal evaluation, and SL/TP-touch
  win/loss judgment.
- **A small Python/Optuna sidecar** (new `rl-service`-adjacent service or an addition to
  `rl_service`, TBD at implementation — likely its own lightweight FastAPI app given it has no
  ML-model-loading concerns `rl_service` does): given a trial's parameter ranges (from
  `strategy.ParamSpec`) and prior trials' results (win/loss), returns the next candidate parameter
  set to try. Stateless from Go's perspective per call — Go owns trial bookkeeping, Optuna only
  proposes.

### 16.3 Trial mechanics

For a given (token, base-strategy) pair, e.g. (XAU-USD-SWAP, `rsi_sma`):

1. `cmd/strategy-optimizer` asks Optuna for N candidate parameter sets (bulk-seeded at start, or
   one at a time as trials complete — implementation detail, not a design commitment yet).
2. For each candidate, it constructs a live `strategy.Strategy` via `WithParams` (already
   implemented, §9) and evaluates it against the real live candle window on every relevant candle
   close, exactly like `PaperTrader.evaluateStrategies` does — same strategy code, same real market
   data, but **the resulting "trial position" is never written to `paper_orders`**. It is tracked
   as lightweight, TTL'd state in Redis (candidate params, entry price, SL/TP, opened-at) —
   deliberately not a durable Postgres row, since these are disposable experiments, not real trades
   or even real paper trades.
3. On every real-time price tick (the same `okx.tickers` Kafka topic `PaperTrader`/`cmd/trader`
   already consume, CLAUDE.md §12), every open trial for that token is checked for SL/TP touch
   against the live tick price — not candle-close price, for the same correctness reason
   `PaperTrader`'s tick-driven SL/TP check exists (§14: candle-close-only checking can silently
   miss a wick that touched SL/TP and reverted within the bar).
4. When a trial's SL or TP is touched, it's recorded as a win or loss (touched TP = win, touched SL
   = loss) and reported back to Optuna as that trial's outcome, closing the loop for the next
   candidate suggestion.
5. After enough trials accumulate (a minimum count per candidate, not a single trial — noisy market
   events, e.g. a sudden large order or a social-media-driven price spike hitting one otherwise-good
   parameter set's SL, must not by themselves condemn it; exact minimum-trial-count and
   how-long-to-run-per-round are tuning parameters to set at implementation, not fixed here) and
   Optuna's search has converged on one or more promising parameter sets, the best candidate(s) are
   persisted as new durable sub-strategy rows (`port.Repository.CreateStrategy`, `ClonedFrom` set
   to the base strategy's origin row, `Config` set to the winning params — CLAUDE.md §11.3's
   existing parent/override model, unchanged) and assigned to that token/timeframe
   (`CreateAssignment`) — entering the real signal-generation pipeline `PaperTrader` runs, same as
   any manually-created sub-strategy today.

### 16.4 What this phase is for, and what's still undecided

Explicit scope, per the user: this optimizer is for the **initial training/bootstrap phase** —
getting from "default strategy parameters" to "parameters tuned against real recent market
behavior per token" before the RL agent (§15) starts training in earnest against realistic signal
quality, not a fixed foundational strategy that never improves after. **Not yet decided**: what
happens after the system moves into the main live-trading phase — whether/how often re-optimization
runs again, whether it runs continuously in the background, or whether it's a manual/periodic
operator action. Do not build an automatic recurring re-optimization loop without an explicit
decision on this — leave it as a manually-triggered `cmd/strategy-optimizer` run for now.

### 16.5 A fuzzy-logic sub-strategy variant (parallel track, independent of the optimizer above)

Separately from the optimizer itself, the user asked for a **fuzzy-logic version of at least one
existing strategy** as a new sub-strategy option — e.g. a `rsi_sma`-derived variant whose
`Confidence` is computed as a smooth, continuous membership degree (how strongly is RSI in the
"oversold" region, as a 0-1 degree, not a hard threshold crossing) rather than the sharp
threshold-crossing logic today's strategies use. This is a real, independent technique (not a
replacement for the optimizer, not a replacement for the RL agent) that directly addresses the
"nothing in real markets is a hard 0/1 boolean" observation raised during design — smoothing a
single strategy's confidence calculation is a self-contained, low-risk place to try it, distinct
from the larger (and explicitly deferred) question of using fuzzy logic to reconcile heterogeneous
raw indicators as a preprocessing layer ahead of the RL agent (§16.1's Neuro-Fuzzy note). Implement
as a new `strategy.Strategy` kind (e.g. `rsi_sma_fuzzy`) alongside the existing 12, not a
modification to `rsi_sma` itself — origins must stay locked and comparable (§11.3).

### 16.6 Open implementation questions (resolve at implementation time, not here)

- Exact minimum-trials-per-candidate and per-round time/trial budget before Optuna's suggestions
  are trusted enough to persist (§16.3 step 5).
- Where the Python/Optuna sidecar physically lives (new service vs. an addition to `rl_service`)
  and its API shape.
- Whether `cmd/strategy-optimizer` runs against every configured token/base-strategy pair by
  default or requires an explicit operator-triggered list (likely the latter, to bound resource use
  — see §15.1's resource-math precedent for why an unbounded "everything at once" default is the
  wrong instinct).

### 16.7 Implementation decisions (resolving §16.4/§16.6, made during this build)

These decisions were made while actually building §16 and **revise §16.4's original
"manually-triggered only" framing** — recorded here rather than silently changing §16.4's prose,
since the reasoning for the change is worth keeping alongside the original framing (same pattern
as §15.1's "rejected/superseded" note).

- **`cmd/strategy-optimizer` is a long-lived service, not a one-shot CLI.** It exposes
  `POST /optimize {inst_id, kind}` (start one run right now) and `GET /status?run_id=` (poll
  progress — candidates tried, best score so far, persisted or inconclusive), and it also runs a
  **built-in scheduler**: on a configurable interval (`optimizer.schedule_interval`, a plain Go
  duration string, e.g. `"24h"` — not hardcoded daily) it fires one time-boxed run per configured
  target, **sequentially** (never all targets in parallel), matching §15.1/§16.6's precedent
  against unbounded "everything at once" resource use. Targets default to
  `Trading.InstIDs × optimizer.default_kinds` but are fully overridable via `optimizer.targets`.
- **Time-boxing is now the primary stopping rule, not trial count** — this revises §16.3 step 5's
  original "minimum trial count" framing to "time-boxed, with a minimum-trades-per-candidate
  *eligibility floor* within that time box." Each run gets a wall-clock budget
  (`optimizer.run_duration`, e.g. `"4h"`); within it, candidates are continuously requested from
  the sidecar (refilled up to `optimizer.candidate_batch_size` as trials complete) and evaluated
  against live market data. `optimizer.min_trades_per_candidate` (e.g. 15) still gates which
  candidates are even eligible to win at the time box's close — the noise-rejection reasoning from
  the original §16.3 step 5 is unchanged, it's just no longer the loop's own exit condition.
- **Run-end scoring/persistence** (one of §16.6's "resolve at implementation time" items, now
  resolved): pick the highest-win-rate eligible candidate, ties broken by trade count (more
  evidence wins). If a clean baseline exists — the strategy currently assigned to that
  inst_id+bar, with its own closed-trade win rate via `StrategyStatsFor` — the winner must beat it
  by at least `optimizer.min_improvement_pct` percentage points (e.g. 5pp) to persist. If no clean
  baseline exists (fresh token/kind pair, or the current assignment has no closed trades yet), the
  winner must instead clear an absolute floor, `optimizer.min_win_rate_pct_floor` (e.g. 50%). A run
  that produces no eligible or qualifying candidate is recorded as inconclusive and persists
  nothing — this is an expected, non-error outcome, not a failure.
- **Sidecar is a genuinely separate, minimal service** (`optimizer-service/`): its own FastAPI app
  and `requirements.txt` (`fastapi`, `uvicorn`, `optuna`, `pydantic` — no `torch`/`psycopg2`, unlike
  `rl-service`), because it has none of `rl-service`'s model-loading or CPU-only-torch-wheel
  concerns (§15.1's Dockerfile note doesn't apply here at all). `POST /suggest` uses Optuna's
  `ask()` per candidate (returning the trial's own `trial.number` as an opaque `trial_id` Go must
  echo back); `POST /report` looks that trial back up and calls `study.tell()`. Studies
  (`optuna.create_study(direction="maximize")`, one per `"{inst_id}:{kind}"` study id) live only in
  the sidecar process's memory — a restart starts fresh studies, which is fine per the original
  "disposable trial state" framing (only a *winning* candidate is ever durable, and that
  persistence happens Go-side, not in the sidecar).
- **Parameter-change timeline + chart** (not originally scoped in §16, added because the
  optimizer needed some visible record of what it changed and when): a new `strategy_param_changes`
  table (migration `000005_strategy_param_changes`) logs every strategy config change —
  `source='optimizer'` when `cmd/strategy-optimizer` persists a winning candidate,
  `source='manual'` when an operator edits a sub-strategy's params via the existing
  `PUT /api/strategies/:id` panel flow (both call the same new
  `Repository.RecordParamChange`). `GET /api/candles` (a new, minimal read of the existing
  `candles` hypertable — no new candle storage) and `GET /api/strategies/:id/param-changes` back a
  new chart on the Strategies page (`panel/src/components/ParamChangeChart.tsx`): a plain inline
  SVG price line (no charting library added, keeping with §14 Phase 3's hand-rolled-CSS/minimal-
  deps convention) with a vertical marker line at each parameter-change timestamp; hovering a
  marker shows a tooltip diffing old vs. new param values (only the keys that actually changed).
