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
│   │   ├── paper-trader/      # forward-test engine, §8; -backfill loads candle history, §17
│   │   └── api/               # dashboard/reporting HTTP API, §11
│   ├── internal/
│   │   ├── domain/            # core entities: Candle, Ticker, Position, Balance, Order,
│   │   │                        LeverageChange, Observation, Action — no framework/IO deps, §10
│   │   ├── usecase/           # application logic: Trader (live trading loop), PaperTrader
│   │   │   │                    (forward-test loop) — depend only on domain + port interfaces
│   │   │   └── conductor/     # SignalConductor: the RL signal lifecycle state machine — which
│   │   │                        decision to ask the model for, at what cadence, §15.12. Pure, no IO
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
  - `account_equity` — one row per mode (paper/demo/real): the shared balance every token trades
    against, its configured starting point, and reset_count/last_reset_at (§15.6/§15.7). Replaced
    the per-token `token_budgets` table in migration `000006`.
  - `account_equity_history` — every balance change: post-change equity, signed delta, and reason
    (`trade`|`reset`|`seed`), backing the panel's balance chart so an overnight drain-and-reset is
    reviewable after the fact (§15.7).
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
- **Decision timeframes vs. context timeframes (decided 2026-08-28).** These are deliberately
  separate settings, because they answer different questions:
  - `ingestion.bars` — every timeframe collected. Now `5m/15m/1H` (the decision set) **plus
    `4H`/`1D` for context only**.
  - `paper_trading.bars` — the timeframes strategies actually *decide* on, and therefore the RL
    model's training cadence: **`5m`, `15m`, `1H`**. Narrowing to a single "best" timeframe is a
    later call to make from real data, not up front.
  - A strategy assigned to 5m may still *read* 1H/4H/1D for trend confirmation — normal
    discretionary practice, and a strategy restricted to one window structurally cannot express it.
    Implemented as the **optional** `strategy.MultiTimeframeStrategy` interface (`EvaluateView(
    MarketView)`) rather than a change to `Strategy.Evaluate`'s signature, so all 14 built-ins keep
    working untouched and only strategies that want higher-timeframe context opt in.
    `strategy.EvaluateWith` dispatches to whichever the strategy implements, so engine call sites
    don't change when a strategy gains the capability. `MarketView.Higher(bar, minLen)` returns
    `ok=false` when a bar isn't collected or hasn't filled yet — that's the normal warm-up case, to
    be degraded through, never an error.
  - `PaperTrader.marketView` snapshots every maintained bar under one lock, so a multi-timeframe
    strategy sees bars consistent with each other and no lock is held across `Evaluate`.
  - **`cmd/strategy-optimizer` deliberately stays single-timeframe** (`Evaluate`, not
    `EvaluateWith`): a tuning run evaluates one kind on one bar (§16.3) and only maintains that
    bar's window. Handing a multi-timeframe strategy a view containing just its own bar would score
    it as a different strategy than the one production runs. Optimizing those needs the runner to
    maintain the extra bars first.
- **OKX bar-name casing is validated at startup.** OKX's channel names are case-sensitive
  (`candle1H`, not `candle1h`), and the ingestor subscribes to `"candle"+bar` literally — a
  mis-cased bar subscribes to a channel that pushes nothing, so that timeframe silently produces no
  candles while the pipeline looks perfectly healthy. `config.validateBarNames` rejects it at
  startup and names the correct casing, turning a silent data gap into an immediate failure.

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
persisted to Postgres. This replaces the earlier REST-polling version, which had a real
correctness bug (checking SL/TP only against candle-close prices could silently miss a price wick
that touched SL/TP and reverted within the same bar) in addition to rate-limit/delay concerns.

**Candle windows are seeded from Postgres at startup, never from the exchange** (2026-08-29,
`PaperTrader.seedCandlesFromRepo`). The original REST seed (`GetCandles` once per instrument+bar at
boot) was removed in `f1af152` because at 10 instruments × 3 bars it fired ~30 concurrent
`/market/candles` requests that OKX rejected — reported confusingly as "instrument not found"
(51001) on a different instrument each run, and surviving retry/jitter, pool tuning, a semaphore,
and two HTTP/1.1 workarounds. Removing it was right, but the accompanying reasoning ("history fills
in within the first few closed candles") only held for the shortest bar: a 1H window needs ~2 days
of live feed to reach 50 candles. The observable result was that after every restart only
short-window strategies on 5m could fire — 12 of 14 strategies returned `Hold` and every open
position came from the one strategy needing the fewest candles. The fix reads the same candles the
ingestor has already written to the `candles` table, so it makes no exchange call and that failure
mode cannot recur. `paper_trading.candle_limit` raised 100 → 300 so long-window strategies fit.

**`PaperTrader` consumes every `ingestion.bars` timeframe, not just the decision bars.** It is the
only writer of the `candles` table, so a bar nobody consumes is never persisted: the ingestor
correctly subscribed to `candle4H`/`candle1D` and published them to Kafka, where the messages
expired unread, leaving the context timeframes (§9) permanently empty in the database while every
log looked healthy. Consuming a bar does not make it a decision bar — `evaluateStrategies` filters
assignments by `a.Bar != bar`, so a context-only timeframe maintains its window and persists its
candles without ever triggering a trade.

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
- [x] **Widened the action space to the full §15.4 action (2026-08-28)** — until this landed, both
      envs' `action_space` was still the original `Box(2,)` `[target_exposure, leverage_frac]`, so
      `/predict` returned **hardcoded** neutral values for `strategy_weights` (uniform) and
      `sl_adjust_pct`/`tp_adjust_pct` (always 0.0) no matter what the model said. That made the
      whole shadow-fork A/B mechanic (§15.4) dead code: `adjustOpenOrdersWithRL` skips on a zero
      adjustment, so a trained model could never fork anything and the comparison page would have
      stayed empty with no error anywhere. Now: `ACTION_DIM = 4 + MAX_STRATEGY_SLOTS` (12), decoded
      by one shared `obs.py:decode_action` used by both `/predict` and `ReplayEnv`; a model with a
      mismatched action width is refused with a 503 (and `/health` gained `action_compatible`)
      instead of silently serving stubs. `ACTION_SCHEMA_VERSION`/`domain.ActionSchemaVersion` track
      the action layout separately from the observation's `schema_version`, since a too-narrow
      action space is a different failure from a mis-shaped observation. `ReplayEnv` now also
      tracks simulated SL/TP, closes on a bar high/low touch, applies Go's tighten-only ratchet, and
      charges the §15.5 churn penalty — without those, the new adjustment outputs would have had no
      gradient signal at all. Verified end-to-end against a real trained model: `/predict` returns
      model-derived nonzero `sl_adjust_pct`/`tp_adjust_pct` and differentiated `strategy_weights`;
      an old 2-dim model gets the 503. 8 new decode tests + 4 new replay-env tests (19 Python tests
      total), all 85 Go tests still pass.
- [x] RL-driven order sizing at open (`usecase.rlSizing`, `paper_trading.rl_sizing`, off by
      default) — previously every paper order was opened at a hardcoded 1x leverage and the fixed
      `notional_usd`, so the `paper_orders` log contained zero leverage/exposure variance for the
      continued-live-learning phase below to learn sizing from. Magnitude-only by design: direction
      stays with the strategy layer (§9/§16.1). `RLSLTPAdjust` is now an explicit `PaperTrader`
      field rather than being implied by `Model != nil`, so the two RL passes stay independent as
      documented.
- [x] Fixed the live/demo path's train-serve skew (`usecase.Trader`, `cmd/trader`): its observation
      sent no `ActiveTokens` at all (an all-zero token-identity one-hot — a token the global model
      has never seen) and reported **total account equity** as `TokenEquityUSD` where every
      paper-trading observation reports that token's own sub-budget. Both now come from
      `Trading.InstIDs`/`PaperTrading.TokenBudgetUSD`, the same source the paper-trader uses, with a
      total-equity fallback when no per-token budget is configured. No practical effect while the
      model is a no-op, which is exactly why it was worth fixing before the demo run rather than
      after (§15.6, and the open demo-trading item in Phase 2).
- [x] **Shared $100 account replacing per-token sub-budgets (2026-08-28, §15.6's revision)** — the
      RL agent now decides what fraction of real account equity each position uses, bounded by
      `account.max_position_pct` (25%) and `account.max_total_exposure_pct` (60%) rather than by
      pre-split per-token buckets. Migration `000006` replaces `token_budgets` with `account_equity`
      (one row per mode) + `account_equity_history`; observation schema bumped to **v4**
      (`AccountEquityUSD`/`AccountInitialUSD`/`OpenExposureUSD` replace the token-budget fields, fed
      to the model as ratios). `ReplayEnv` sizes against the shared balance under the same caps and
      **carries the balance across token boundaries** — it used to reset equity per token, which
      under a shared account would have taught the agent that losses are wiped clean at each
      boundary. `token_budgets` used `DOUBLE PRECISION` against §7's NUMERIC rule; the new tables
      don't repeat that. Verified against a real TimescaleDB (migration applies, drain→reset,
      real-mode carve-out, history ordering, exact NUMERIC round-trip of 100.07), and the endpoints
      verified through the real router.
- [x] Equity timeline for all three modes (explicit requirement: "I want to see it happen when I'm
      not online") — every balance change writes an `account_equity_history` row with its reason
      (`trade`/`reset`/`seed`), written in the **same transaction** as the balance update so the
      chart can never be missing the drop that drained the account. `GET /api/account` +
      `GET /api/account/history`; `cmd/trader` records demo/real by observing the exchange's
      reported equity each poll, best-effort so a database problem can't interrupt live trading.
      **Panel chart still to build** — the backend data is in place, the frontend view isn't (the
      panel is the one part explicitly out of scope for now).
- [x] **Decision timeframes fixed at 5m/15m/1H, with higher-timeframe context available
      (2026-08-28)** — `ingestion.bars` collects `5m/15m/1H` plus `4H`/`1D` for context;
      `paper_trading.bars` (the decision/training cadence) is the three. Added the optional
      `strategy.MultiTimeframeStrategy` interface + `MarketView`/`EvaluateWith` so a strategy
      assigned to 5m can consult 1H/4H for trend confirmation — an optional capability interface
      rather than a signature change, so all 14 built-ins are untouched. `PaperTrader.marketView`
      snapshots every bar under one lock. `cmd/strategy-optimizer` deliberately keeps the
      single-timeframe path (it only maintains one bar; see §9). Replaced the tick-path's
      `Bars[0]` decision-context selection — array order that would silently change meaning if the
      config list were reordered — with `paper_trading.rl_decision_bar`, defaulting to the
      *shortest* configured bar (freshest read for an in-trade adjustment). Added OKX bar-name
      casing validation at startup: `1h` instead of `1H` used to subscribe to a channel that pushes
      nothing, producing a silent data gap on a pipeline that looks healthy. Warm-start stays
      single-bar by design (now `15m`, was `1m`) — interleaving timeframes in one replay sequence
      makes "one step" mean different elapsed times, so the reward signal would be inconsistent
      across steps. 12 new tests.
- [x] Paper → demo → real progression guardrails (§15.6): mode derived from `okx.simulated` so it
      can never disagree with the credentials in use, plus `trading.allow_real_money` (default
      false) which `cmd/trader` refuses to start without against non-demo keys.
- [x] **Audited all 14 strategies for entry_px / sl_px / tp_px (2026-08-28, §15.11)** — see §16.8
      for the full result. Nothing was deleted: 7 strategies computed genuinely structural levels
      and were discarding them (converting to a percentage, then having ResolveLevels reconstruct an
      approximation), and the other 7 have no structural level in their logic at all, where a
      percentage is the honest output — inventing one would give the model a level no strategy
      chose, which is worse than none. The audit surfaced four separate pre-existing bugs, one of
      which had made a registered strategy permanently silent.
- [x] **Continuous learning on SAC (§15.11)** — `rl-service` now keeps learning from live outcomes
      instead of serving frozen weights between periodic retrains, which PPO structurally could not
      do at this trade volume. `rl_service/learner.py` holds each decision as *pending* keyed by
      order id and pairs it with the realized PnL that arrives on the terminal (`closed_*`) call
      hours later — a decision cannot be scored when it is made, so pairing by id rather than by
      arrival order is what keeps a winning trade's reward from training a losing trade's decision.
      Reward is PnL normalized by account size, for the same reason the observation feeds ratios.
      Snapshots write weights AND replay buffer together (config `snapshot_every`), and startup
      restores both — weights alone would come back having forgotten every experience collected.
      `learning_enabled` is off by default and must stay off for real money.
      Caught a silent-failure bug while testing: SB3 sets up its logger inside `learn()`, which this
      service never calls, so `train()` raised on its first metric write and the error handler
      swallowed it — the service would have looked healthy while never learning anything. 9 new
      tests (45 Python total), verified end to end through the real API with learning enabled.

- [x] **The Signal Conductor (§15.12, 2026-08-28)** — the piece that made every other §15 mechanism
      actually reachable. `PaperTrader` had been sending `CategoryUpdate` as a hardcoded default and
      never emitting a terminal call, so `rl_service/learner.py` — which holds each decision pending
      by order id and pairs it with the realized PnL arriving hours later — received **zero rewards
      in production**: the model was asked questions but never told how any answer turned out.
      `internal/usecase/conductor` holds the pure state machine (category selection, PnL-delta
      update cadence, signal carry-forward per (instId, bar), SL/TP placement clamps) and
      `internal/usecase/lifecycle.go` the IO half. Every close now routes through one
      `closeOrder`, so no path can complete a close while skipping the reward call; a manual close
      deliberately emits nothing, since attributing an operator's action to the policy would train
      on a decision it never made. Early close (`rl_early`, migration `000008`) is opt-in — it is
      the one action that destroys the counterfactual. Also fixed a latent double-call: the open
      path used to `Predict` twice for one decision (once for buy/sell, once for sizing), which
      could size an order against one answer while taking its levels from another. 25 new tests
      (161 Go total), plus end-to-end verification against a live learning-enabled `rl_service` and
      the migration applied to a real TimescaleDB.

**NEXT UP — start here in a new session.** Ordered:

- [x] **(1) Signal Conductor in Go — §15.12.** Done 2026-08-28. `internal/usecase/conductor` (pure
      state machine) + `internal/usecase/lifecycle.go` (PaperTrader's IO half): category selection,
      PnL-threshold update cadence, terminal reward calls, early close (`rl_early`, migration
      `000008`), signal carry-forward, and the SL/TP placement clamps. Verified against a live
      `rl_service` with learning enabled — `completed_trades` and real SAC gradient steps, where
      the counter would previously have stayed at zero forever. 25 new tests.
- [x] **(2) Reward penalties in `ReplayEnv` — §15.13.** Done 2026-08-28. Drawdown (against a new
      `peak_equity` high-water mark that survives token boundaries) and liquidation-proximity
      (the term that makes leverage itself cost something) now both charge against reward, and are
      reported in `info` so a falling curve can be attributed to risk vs. bad entries. Caught a
      latent bug in the process: `step` re-stamped `self.leverage` every step, so a position opened
      at 100x silently read as 1x while held. 10 new tests.
- [x] **(3) Strategy audit for `entry_px`/`sl_px`/`tp_px`** — done 2026-08-28, full result in
      §16.8. 7 of 14 now emit the structural levels they were computing and discarding; the other 7
      legitimately have none. Turned up four pre-existing bugs, including `pmax` being unable to
      emit a signal at all and EMA's precision growing without bound.
- [x] **(3b) Candle backfill (2026-08-28, §17)** — warm-start rolls out against real candle
      history from Postgres, and a fresh database has none. Waiting days for the live ingestor to
      accumulate it was never necessary: OKX serves it directly. `cmd/paper-trader -backfill` and
      `POST /api/candles/backfill`, paced and idempotent. This turned step (4) from a multi-day
      wait into a ~90 second job.
- [x] **(4) It is running (2026-08-29)** — but not by the route this item described. Warm-start was
      dropped by explicit product decision, which made the untrained policy skip every signal and
      produced a structural deadlock (no opens → no closes → no reward → weights never change; see
      §16.9). Resolved by running with `serve.learning_enabled: true` and `rl_sltp_adjust: true`
      while `rl_sizing` stays **off**: strategies open positions, and the model manages them through
      the update path, so real rewards flow from real closed trades. `rl_early_close` stays off.
      Enabling `rl_sizing` — handing the model the open decision — is the next step once it has a
      track record. Getting here surfaced four silent-failure bugs and two ordering bugs, all
      documented in §16.9.
- [ ] **(5) Hand the model the open decision**: turn on `paper_trading.rl_sizing` once enough closed
      trades exist that the policy is no longer random (SAC's `learning_starts` is 100, so gradient
      steps do not begin before that). Watch `okxbot_model_open_decisions_total`'s `skip` vs `open`
      split — a policy still skipping everything is not ready.

Deferred, in rough priority: per-token reward breakdown in training logs (§15.5 — the detection
mechanism for "good on average, bad for one token", needed before expanding past 2 tokens); the
panel's equity chart (backend done, frontend not); `rl_service/metrics.py` (§11.2); and the OKX
demo-trading dry run, which is the one Phase 2 item that can only be done on the server.

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

### 15.1 Core decision: one single global agent, not one per token

(The algorithm was PPO when this was written; §15.11 changed it to SAC. The one-global-agent
decision below is unaffected by that — it is about pooling experience, not about the algorithm.)

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
  the exact `instId` exists on OKX SWAP before wiring it in), ≤5 of the 14 registered strategies
  (`strategy.Factories`) (pick the
  ones with the cleanest/most orthogonal signals — e.g. avoid shipping two near-duplicate
  moving-average-cross variants both in the initial 5), the 3 decision timeframes
  (**`5m`, `15m`, `1H`** — OKX casing) folded into
  the one global agent's observation, token identity as an explicit input field (one-hot over
  active tokens is enough at this scale — a learned embedding is unnecessary complexity for 2-10
  tokens). One model: `models/ppo_global.zip`. Strategies may additionally *read* `4H`/`1D` for
  context without those becoming decision timeframes (§9). Narrowing to a single best-performing
  timeframe is an explicit **later** decision, to be made from real per-timeframe results rather
  than guessed now.
- **Phase B:** expand token count toward the full ~10-token roster and/or the full 14-strategy
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
- **Account/position tail**: current exposure, current leverage, unrealized PnL %, plus the shared
  account's equity ratio (balance ÷ starting balance) and its already-committed exposure ratio
  (§15.6, revised 2026-08-28 — this replaced the earlier per-token sub-budget framing). Fed as
  ratios rather than raw dollars so the policy doesn't go out-of-distribution when the account size
  is reconfigured. Per-token reward attribution (§15.5) is unaffected by capital being pooled.
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

**Implemented layout (2026-08-28)** — the policy emits one fixed-width vector, `ACTION_DIM = 4 +
MAX_STRATEGY_SLOTS` (12 today), decoded by the single shared `rl_service/obs.py:decode_action`
that both `/predict` and the replay env go through so the two can never drift:
`[target_exposure, leverage_frac, sl_adjust_pct, tp_adjust_pct, w_0 … w_{MAX_STRATEGY_SLOTS-1}]`.
PPO needs a fixed action shape but the number of assigned strategies varies per request, so the
weight slots are read **positionally** against the request's strategy signals (assignment order, as
`buildObservation` appends them) and only slots backed by a real signal are returned. Raising
`MAX_STRATEGY_SLOTS` (mirrored as `domain.MaxStrategySlots`) changes the action width and
invalidates existing models — bump `ACTION_SCHEMA_VERSION`/`domain.ActionSchemaVersion` with it.
`sl_adjust_pct`/`tp_adjust_pct` are emitted in `[-1, 1]` and scaled by `MAX_SLTP_ADJUST_PCT` (0.02,
mirroring `usecase.MaxSLTPAdjustPct`) so the policy's output range maps onto exactly the bounded
adjustment Go's ratchet accepts, rather than spending most of its range on values Go clips away.

A model whose action width doesn't match is refused with a **503** (and `/health` reports
`action_compatible: false`) rather than served — before this landed, `/predict` returned hardcoded
neutral values for `strategy_weights`/`sl_adjust_pct`/`tp_adjust_pct`, which looked exactly like a
working model that never wanted to adjust anything, and silently made the shadow-fork mechanic
below dead code.

1. **`strategy_weights`**: one continuous value per assigned strategy (clamped to [0,1] and
   normalized to sum to 1 across the strategies present in the request) — how much the agent trusts
   each strategy's current signal *right now*, combined with each strategy's `Confidence` to produce
   a single effective directional signal. This is what "combine the signal of multiple strategies"
   (per your original ask) resolves to concretely: a learned weighting, not a fixed voting rule. An
   all-zero output falls back to uniform, since a dict of zeros is indistinguishable downstream from
   a bug.
2. **`target_exposure`** (unchanged, [-1, 1]): resulting position as a fraction of *this token's*
   max allowed notional, sign = side.
2b. **Direction stays with the strategy layer.** `target_exposure`'s *magnitude* sizes the order;
   its sign is not used to flip an order's side out from under the strategy that generated the
   signal (§9/§16.1: strategies decide *when* there's a tradeable signal, the agent decides *how
   much*). A model that disagrees with a signal expresses that by sizing toward zero.
   `usecase.rlSizing` implements this, gated behind `paper_trading.rl_sizing` (off by default,
   independent of `rl_sltp_adjust`); while it's off, every paper order is opened at the fixed
   `notional_usd` and 1x, so the `paper_orders` log carries **no leverage or exposure variance** —
   which is precisely the training signal §15.8's continued-live-learning phase needs in order to
   learn sizing at all. Turn it on once a model with the current action schema is loaded.
3. **`leverage_frac`** (unchanged, [0, 1]): mapped to `[1x, MAX_LEVERAGE]` — your target range is
   10x-100x. `EnvConfig.max_leverage`, `okx_futures_env.py`'s own default, and
   `config.example.yaml`'s `risk.max_leverage` are all 100 as of this revision (they were 5 when
   this section was first written). §5's hard risk caps (independent of RL, Go-side,
   non-overridable) are what actually bound worst-case loss — the RL agent proposing up to 100x is
   safe only because those caps clamp it, never trust the agent's own leverage choice as the safety
   boundary.
4. **`sl_adjust_pct`, `tp_adjust_pct`** (new, continuous, both allowed negative/positive within a
   clamped range of ±2% per decision step): in-trade adjustments to the *open* position's SL/TP.
   Cadence: this section originally proposed starting at candle close and tightening to tick
   cadence later; §15.9's freshness audit found candle-close cadence let the model reason about a
   price up to a full bar stale, so it now runs from the **tick stream**, throttled to
   `RLAdjustInterval` (2s). This is what "trail SL
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
     adjustOpenOrdersWithRL` (called from `handleTick` on the live tick stream, throttled to
     `RLAdjustInterval`, gated on `PaperTrading.RLSLTPAdjust`) is the call site. A fork is
     **tracking-only**: it is explicitly excluded from token
     budget/reward accounting (§15.6/§15.7) — only its baseline parent's realized PnL counts
     toward the token's real running budget, so one signal never draws down the budget twice.

### 15.5 Reward shaping

Extends the existing `okx_futures_env.py` shaping (realized PnL − fee/funding − drawdown penalty −
liquidation-proximity penalty, §2) with:
- A small penalty on `sl_adjust_pct`/`tp_adjust_pct` churn so the agent doesn't learn to twitch the
  SL every tick for free — every adjustment should earn its keep in realized outcome, not be free
  to try. **Implemented** in `replay_env.py` as `SLTP_CHURN_PENALTY` (0.002), charged against the
  magnitude of each step's proposed adjustment. Scaled against the raw `[-1, 1]` outputs rather than
  the post-`MAX_SLTP_ADJUST_PCT` fractions, so the penalty doesn't quietly shrink if that bound is
  widened later. For the adjustment to have any learnable consequence at all, the replay env also
  tracks simulated SL/TP prices per position and closes on a **bar high/low touch** (not just the
  close — the same intra-bar-wick correctness reason `PaperTrader` checks SL/TP on ticks, §14), and
  mirrors Go's `RatchetSLTP` tighten-only clamp so the policy only sees reward consequences for
  adjustments production would actually accept.
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

**Revised 2026-08-28 — one shared account the agent sizes against, replacing fixed per-token
sub-budgets.** The original decision is kept below rather than deleted, since the concern that
motivated it is real and still constrains the design.

**Current design**: one account (`account.initial_usd`, default **$100**) that every token trades
against. The RL agent's `target_exposure` decides what fraction of *current account equity* goes
into each position — this is the "imagine we have $100 and the model decides how much to use"
requirement, and it's what makes `target_exposure` a real capital decision rather than a multiplier
on a config constant. Sizing is implemented in `usecase.rlSizing`, gated behind
`paper_trading.rl_sizing`.

Two **hard Go-side caps** bound whatever the model proposes — the same "never trust the model as
the safety boundary" pattern as §15.4's ratchet and §5's risk manager:
- `account.max_position_pct` (default 0.25) — no single position exceeds this fraction of equity.
  At the 100x leverage ceiling (§15.4) an uncapped position is an account-ending event, and §5's
  risk manager only guards the *live* path, not paper.
- `account.max_total_exposure_pct` (default 0.60) — the summed notional of all open baseline
  positions across every token. The per-position cap alone would still permit four simultaneous
  25% positions committing the whole account. A new position is trimmed to the remaining headroom,
  or declined outright when there is none. Shadow forks (§15.4) are excluded from this sum — they
  track their baseline parent rather than committing separate capital.

The observation carries `AccountEquityUSD`/`AccountInitialUSD`/`OpenExposureUSD` (schema v4), fed
to the model as **ratios rather than raw dollars** (`rl_service/obs.py:observation_tail`): the
decision is scale-free, and a policy trained on raw balances would go out-of-distribution the
moment the account size is reconfigured. `ReplayEnv` applies the same caps and the same
equity-relative sizing, so the policy never learns to request sizes production would clamp away.

**Mode progression: paper → demo → real** (decided 2026-08-28). Training starts in **paper** mode
and stays there until there's a genuinely good win rate to point at; then optionally OKX **demo**
(real order placement, sandboxed — `x-simulated-trading: 1`, §4) as a dress rehearsal that exercises
the actual execution path; then **real** money. Each mode keeps its own `account_equity` row and
equity timeline (§15.7), so switching doesn't blend the histories and the paper track record stays
readable after moving on. Two guardrails make the progression hard to take by accident:
- `cmd/trader`'s mode is **derived** from `okx.simulated`, never configured separately — a
  standalone `mode:` setting could disagree with the credentials in use, which would record real
  losses against the demo account's books *and* make them eligible for the paper/demo auto-reset.
  Tying them together makes that combination unrepresentable.
- `trading.allow_real_money` must be explicitly `true` before `cmd/trader` will start against
  non-demo credentials; it refuses and exits otherwise. Reaching real trading by merely *forgetting*
  to set `OKX_SIMULATED_TRADING` would make an unset env var the only thing between a sandbox and
  real capital.

Making mode (and the other config above) switchable from the panel is the intended end state, but
the panel is deliberately behind the backend for now — the settings are config-driven until the
frontend work happens, and `allow_real_money` should stay a config/deploy-level decision rather than
a button, even once the rest becomes panel-editable.

**Why the original per-token design was replaced**: the concern below — that a *learned*
reallocation decision could starve a temporarily-losing token of the trades it needs to keep
contributing training data — is about the agent moving capital *between* tokens as a strategic
choice. That is still not built and still not wanted. What changed is the recognition that fixed
$10 buckets don't avoid that problem so much as sidestep the actual product requirement: a real
account is one pool, and "how much do I commit here" is the decision worth learning. The starvation
risk is now bounded structurally by the caps above rather than by pre-splitting the account, and
per-token reward attribution (§15.5) is unaffected — that was always independent of whether capital
is pooled.

**Superseded (kept for the reasoning)** — *Fixed, not learned, in Phase A*: config-level per-token
notional ($10/token against a <$50 total budget), not a decision the RL agent makes. Rejected
letting even the (now single, shared) agent dynamically reallocate capital across tokens for Phase A
specifically because it would make the per-token reward signal (§15.5) depend on a
capital-allocation decision made from the same pooled policy update — moving capital away from a
temporarily-losing-but-still-learning token would starve it of the very trades it needs to keep
contributing to the shared policy's training data, compounding rather than isolating a bad early
streak.

### 15.7 Handling the account going to zero/negative ("give it another chance")

Per the original ask, now applied at the **account** level rather than per token (§15.6's
revision): a drained balance must not permanently bench the bot — it gets reset and another shot,
since a losing streak early in training is expected noise, not necessarily evidence the
strategy/token mix is bad. Concretely:
- Track equity as its own durable state (`account_equity`, migration `000006`, one row per mode),
  not recomputed by summing `paper_orders` on every check, so "the account is at zero" is a fact
  the system can act on directly.
  - The recent-performance tail in the observation (§15.3) plus `AccountEquityUSD` (§15.6) is what
    lets the *agent itself* learn to size down approaching zero, rather than needing a hardcoded
    halt.
- **Reset condition** (a hard Go-side rule, not RL-decided — this is a bookkeeping/risk-adjacent
  action, same "don't trust the model to have learned this" reasoning as §15.4's ratchet
  constraint): when the running balance hits zero/negative, top it back up to
  `account.initial_usd` and record the reset. `reset_count` is the signal worth watching — an
  account resetting daily is itself evidence something is wrong, and surfacing that is the whole
  reason it's counted rather than silently papering over it.
- **Equity timeline** (explicit product requirement, 2026-08-28 — "I want to see it happen when I'm
  not online"): every balance change writes a durable `account_equity_history` row carrying the
  post-change balance, the signed delta, and *why* (`trade` / `reset` / `seed`), for **all three
  modes**. A drain-and-reset that happens overnight is therefore reviewable afterward in the
  panel's chart instead of existing only as a log line nobody was watching. The balance update and
  its history row are written in **one transaction** — a chart missing the very drop that drained
  the account would be actively misleading, which is exactly what this exists to prevent. Served by
  `GET /api/account` and `GET /api/account/history?mode=&since=&limit=`; `cmd/trader` records the
  demo/real timeline by observing the exchange's reported equity each poll (best-effort — a
  database problem must never interrupt a live trading loop).
- This is paper/demo-mode behavior. Real-money trading must NOT auto-reset a drained balance — that's
  real capital, and running out is a stop condition requiring a human decision, not an automatic
  top-up. **Now enforced in code, not just documented**: `Repository.ApplyRealizedPnL` skips the
  reset entirely when `mode == "real"`, leaving the balance at/below zero and reporting
  `reset=false`, so the paper-mode reset logic cannot leak into the real path by accident. Covered
  by a test asserting a real account drained to -50 stays at -50. `cmd/trader` picks its mode from
  `okx.simulated` (demo when simulated, real otherwise), so the carve-out follows the credentials
  in use rather than a separately-configured flag that could disagree with them.

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

### 15.10 Event-driven signal lifecycle (decided 2026-08-28) — supersedes §15.3/§15.4's shapes

**The bug that forced this redesign**: `strategy_signals` were built in Go, sent over the wire, and
parsed by Pydantic — but `rl_service/obs.py:observation_features` never put them into the model's
input vector. The model was being asked to emit `strategy_weights` over strategies whose opinions
it could not see. `recent_trades` (§15.3) was dropped the same way. So the observation was carrying
data the policy never received, and the strategy-weight output was noise by construction. §15.3's
field list stands as intent; this section replaces how signals actually reach the model.

**Core idea**: the model isn't polled with a generic world-snapshot — it's driven by a **signal
lifecycle**, where the signal's *category* tells it which decision it is being asked to make. A Go
**controller** (an extension of `PaperTrader`, not a new service) owns that lifecycle per token.

**Categories** replace `buy`/`sell`/`hold` + a separate optimize action:

| Category | When | Model decides |
|---|---|---|
| `buy` / `sell` | a strategy fires, no open position on this token | open or skip; size, leverage, SL/TP |
| `update` | position open: another signal fires, or PnL moved past a threshold | adjust SL/TP, close early, or nothing |
| `closed_tp` / `closed_sl` / `closed_early` | position closed | nothing — this event **is** the reward |

`hold` disappears: a strategy with no opinion simply produces no signal. The close event carrying
realized PnL is what makes this episodic RL rather than a polling loop with a bolted-on reward —
the terminal signal trains every decision that preceded it.

**On the close signal, do NOT zero entry/SL/TP/size.** The outcome has to stay attached to the
decision that produced it, or the model cannot learn which SL placement caused which result.

**One signal per call, deliberately** (revising §15.4's fixed `strategy_weights` slots): each
strategy's signal is its own `/predict` call, carrying its own strategy identity, timeframe, and
its own proposed SL/TP. This removes the fixed-slot ceiling entirely — the roster can grow to 20
strategies or shrink to 8 with no action-space change and no retraining, which a slot-per-strategy
design could never do. `strategy_weights` is **dropped**: with each strategy's live win-rate and
trade-count fed as *input*, the model can learn to discount weak strategies without emitting a
trust score, and strategy selection is better answered from realized win rate anyway.
- Cost of this choice: the model cannot see two strategies agreeing *within one call*, so
  confluence would be invisible. Mitigated by a small fixed-width **market-context block** in every
  observation summarizing the other currently-live signals (how many long/short, mean confidence,
  time since the most recent other signal) — no per-strategy identity, so it stays independent of
  roster size.

**Update cadence: PnL-delta triggered, not time-triggered.** A fixed interval sends updates when
nothing has happened and misses fast moves. Instead an `update` fires when unrealized PnL has moved
at least `rl_update_pnl_threshold_pct` (~1%) since the last one, **or** a time ceiling elapses
(so a position grinding sideways is still observed — funding accrues and setups decay, and "time
passed" is itself information), **or** a strategy fires (never filtered — a real opinion always
reaches the model immediately). This self-adapts: near-silent in a range, dense during a move. It
also keeps credit assignment tractable — order 10 meaningful steps per trade instead of thousands
of near-identical ones.

**Shadow forks become the training signal for optimization** (extending §15.4's A/B mechanic from
an operator-facing comparison to a learning one): baseline runs its original SL/TP, the fork runs
the adjustment, and **outcome(fork) − outcome(baseline)** is a directly-attributable reward for the
adjustment decision specifically — not blended with entry quality the way whole-trade PnL is. This
is what makes in-trade optimization learnable rather than a long-delayed credit-assignment slog.
Consequence for the controller: forks are open positions too, so they generate their own `update`
signals, and fork-of-a-fork must be bounded or it grows without limit.

**Live PnL is computed in Go** from entry price, live tick, and size (as `unrealizedPnLPct`
already does) — not read from Redis, which holds only the optimizer's disposable trial state (§7).

**Reward** (§15.5 unchanged in intent): `ReplayEnv` rewards fee-adjusted PnL minus SL/TP churn,
minus drawdown and liquidation-proximity penalties. Those last two landed in §15.13 — until then the
reward was PnL-only, which with 100x leverage available selected for maximum position size while
§15.6's caps bound it with a hard limit, i.e. the objective and the clamp disagreed. They now agree.

### 15.11 SAC + continuous learning, and the final observation/action shape (decided 2026-08-28)

Revises §2's PPO choice and §15.10's field list, after working through what "the model decides how
much to risk" actually requires. §15.10's lifecycle (categories, close-event-as-reward, one signal
per call) is unchanged — this settles the algorithm and the exact fields.

**PPO → SAC.** §2 chose PPO for stability and noted SAC as a later A/B candidate. The requirement
that forced the change: **the model must keep learning in production, not serve frozen weights
between periodic retrains.** PPO is on-policy — it learns only from actions its *current* policy
just took, collects a batch (~2048 steps), updates, and discards it. At tens of trades/day that
batch takes weeks to fill, so PPO structurally cannot learn continuously here.

SAC is off-policy: a **replay buffer** keeps every experience and reuses it, so it learns from a
trickle rather than a batch and can update after each closed trade. Measured on this project's
actual dimensions (94-in/7-out): ~462k params, **1.88 MB** per weights snapshot, and **~79 MB** RAM
for a 100k-entry buffer (SB3's 1M default is sized for Atari and would waste ~788 MB for capacity
this project will never fill). Fits the 4 GB VPS with room to spare.

- **Freezing stays available**: a SAC model saves and loads exactly like PPO, so `learning_enabled`
  simply turns updates off. This maps onto §15.6's paper → demo → real progression — learn
  continuously in paper, freeze a reviewed snapshot before real capital is involved.
- **Snapshots must include the replay buffer**, not just weights: a restart would otherwise discard
  every experience collected. Weights and buffer are saved separately by SB3.
- **The service becomes stateful.** Today `rl-service` loads a file and answers; a restart loses
  nothing. With continuous learning the buffer and updated weights live in memory, which is a new
  operational requirement.
- **One endpoint, always `/predict`.** Terminal (`closed_*`) calls return an action the caller
  discards — the service uses them to compute reward and update. There is no separate `/train`
  route, and no queue: one request, one response, ~1 ms.
- **Honest limit**: switching algorithms does not fix data scarcity. RL typically wants hundreds of
  thousands of experiences; this produces tens of trades/day. SAC is markedly more sample-efficient
  than PPO, but expect slow learning regardless. §15.4's shadow forks help by yielding two outcomes
  per signal plus a clean baseline-vs-adjusted difference.

**Observation (v6).** Revised from §15.10 after review — several fields were carrying no weight:
- **Signal**: `present`, `side`, `confidence`, **`entry_px`**, **`sl_px`**, **`tp_px`**,
  `win_rate`, `log(trade_count)`, plus `kind` and `bar` one-hots.
  - SL/TP are **prices, not percentages**. A strategy derives a level from chart structure (below a
    swing low, at a fair-value gap); expressing it as a percentage discards exactly the structural
    information that made it a level. Same for the model's own SL/TP output.
  - `entry_px` is new — strategies propose where to enter, which nothing carried before.
  - `win_rate`/`trade_count` are kept deliberately: they are what replaced the `strategy_weights`
    output, and the only way one shared policy can learn that a strategy works on one token and
    not another. Trade count is log-compressed because 100% of 2 trades and 60% of 200 are very
    different evidence.
- **Position** (merged; §15.10 had this split across two overlapping blocks): `position_open`,
  `side`, `leverage`, `size_ratio`, `is_fork`, `age_seconds`, `unrealized_pnl_pct`,
  **`pnl_max`/`pnl_min`**, `dist_to_sl`, `dist_to_tp`. Entry/SL/TP are not repeated here — they
  are already in the signal.
  - `age_seconds` distinguishes "+30% in 10 minutes" from "−5% after 4 hours", which current PnL
    alone cannot.
  - **`pnl_max`/`pnl_min`** (`pnl_min` is negative-ranged) record how far a position travelled in
    each direction, not just where it sits now. A trade that reached 90% of its target and gave it
    all back is a completely different lesson from one that drifted sideways, and without this the
    model cannot tell them apart. Genuinely strong training signal for the SL/TP-adjust decision.
- **Per timeframe**: the **live forming candle's OHLC** plus `ema_5/10/20`, `volatility_5/10/20`,
  `volume_ratio_20`, `rsi_14`, the recent-returns window and distance to swing high/low.
  - OKX pushes the forming candle on the same WS channel (`confirm=0`), so live OHLC needs no extra
    REST call and no rate-limit exposure. On a 1H bar the last *closed* candle can be 59 minutes
    stale, which is exactly the freshness problem §15.9's audit found on the SL/TP path.
  - `ret_1`/`log_ret_1` dropped (redundant with the returns window); `sma_*` → `ema_*`; `vol_*`
    renamed to `volatility_*` and `volume_ratio_20` — both were called `vol`, which read as one
    concept when they are two (return dispersion vs. traded volume).
- **Dropped**: `market_context` (confluence summary — low value once each call carries one signal),
  `recent_trades` (20 of 94 inputs for a weak signal), and the signal's own `age_seconds`.

**One-hot vocabularies are over-provisioned so the roster can grow without retraining**: 24 strategy
kinds (14 used), **16 timeframes** (3 used), 16 tokens (2 used). Spare slots cost a few zeros;
crossing a ceiling costs a retrain. **Appending is safe, reordering is not** — inserting in the
middle silently reassigns every later slot's meaning for an already-trained model.

**Action (v4).** One `action` field, five values, named to match the input categories so the same
word means the same thing on both sides (`adjust` was renamed `update` for this reason):

| Valid on | `action` | Meaning |
|---|---|---|
| `buy`/`sell` | `open` / `skip` | take the trade, or decline it |
| `update` | `none` / `update` / `close` | leave it, move SL/TP, or close now |
| `closed_*` | — | ignored; the call exists to deliver reward |

Plus `sl_px`, `tp_px` (**prices**, set by the model, not just adjusted), `size_pct`,
`leverage_frac`, and `order_id` echoed back so the controller can pair a response to its position.
The model always emits every value; the controller accepts only those meaningful for the category.

**Direction and entry price stay with the strategy** (§16.1): strategies answer *where and which
way*, the model answers *how much risk*. The model shapes the trade — stops, targets, size,
leverage — based on regime (and later news/on-chain), and may `skip` a signal entirely, but never
flips its side.

**Go-side clamps, config-driven** (the §15.4 ratchet pattern — the model is never the safety
boundary): min/max SL distance, a minimum TP:SL ratio, and size/leverage ceilings. Early in
training the policy is effectively random, and one absurd SL would otherwise destroy a position.

### 15.12 The Signal Conductor (implemented 2026-08-28)

§15.10/§15.11 settled *what* the lifecycle is; this section is *how* it is built. Before it,
`PaperTrader` sent `Category = CategoryUpdate` as a hardcoded default and never emitted a terminal
call, so the continuous-learning path in `rl_service/learner.py` received **no rewards at all** in
production — the model could be asked questions but was never told how any answer turned out. This
was the blocker for everything else in §15.

**Name**: `SignalConductor` (`internal/usecase/conductor`), deliberately not "controller". It does
not decide direction — strategies do (§9/§16.1) — and it does not decide size — the model does. It
decides *who plays when*: which lifecycle question to ask, at what cadence, and it guarantees the
terminal call that carries the reward actually happens. Coordination without authorship.

**Where it lives**: `internal/usecase/conductor` holds the pure state machine (category selection,
update cadence, signal carry-forward, SL/TP clamps) with no IO at all; `usecase.PaperTrader` owns
the repository and model calls and delegates the decisions to it (`internal/usecase/lifecycle.go`).
The split keeps the cadence and category rules unit-testable without a database or a running
rl-service, and keeps `papertrade.go` from absorbing another few hundred lines. `PaperTrader` builds
its conductor lazily from its own `RL*` fields, so a struct-literal construction — every test and
every `cmd/` wiring — needs no separate initialization step.

**Category selection** (`conductor.OpenCategory`/`TerminalCategory`, replacing the hardcoded
default in `buildObservation`, which now stands only as a fallback that claims nothing):

| Condition | Category | Then |
|---|---|---|
| strategy fires, no open baseline position for this token | `buy`/`sell` from the signal's side | model returns `open`/`skip`; on `open`, apply its `SizePct`/`LeverageFrac`/`SLPx`/`TPPx` |
| strategy fires, position already open | `update` | model returns `none`/`update`/`close` |
| PnL moved ≥ threshold since last update, position open | `update`, `Signal = nil` | same |
| time ceiling elapsed, position open | `update`, `Signal = nil` | same |
| order closes (SL/TP touch, or model `close`) | `closed_sl`/`closed_tp`/`closed_early` | fire-and-forget; the response is discarded |

`Signal = nil` on a price-driven update is deliberate: `signal_block`'s `present` flag is what tells
the model no strategy spoke, and inventing a stale signal there would be a lie it learns from.

**Update cadence** (`Conductor.ShouldUpdate`, config `paper_trading.rl_update_*`). Fire an `update`
when **any** holds:
- unrealized PnL moved ≥ `paper_trading.rl_update_pnl_threshold_pct` (default ~1%) since the last
  update for that order — self-adapting: near-silent in a range, dense during a real move, and far
  better for credit assignment than thousands of near-identical steps;
- `paper_trading.rl_update_max_interval` elapsed (default ~15m) — a position grinding sideways must
  still be observed, since funding accrues and setups decay, and "time passed" is itself information;
- a strategy fires — never filtered; a real opinion always reaches the model immediately.

Per-order state (last-update PnL and timestamp) lives in memory on the conductor, keyed by order id
and dropped when the order closes so it cannot grow past the set of open positions. Losing it on
restart is harmless: the next tick simply triggers one update, which is the correct behavior for a
process that has just come back and does not know how the position moved while it was gone.
`ShouldUpdate` claims the slot as it answers (it advances the baseline when it returns true), the
same check-and-claim pattern as `shouldRunRLAdjust`, so concurrent ticks cannot double-fire.

**Terminal calls are the reward delivery path.** Every close now goes through one
`PaperTrader.closeOrder` — SL touch, TP touch, and model-driven early close alike — so no caller
can complete a close while skipping the terminal call. It builds an observation with the terminal
category and `PositionState.RealizedPnLUSD` set and POSTs it. Entry/SL/TP are deliberately NOT
zeroed: the outcome has to stay attached to the decision that produced it (§15.10). Ordering
matters — the call happens *after* the order is durably closed, because a model or network problem
must never leave a position open in the database that the price feed has already resolved. A manual
or timeout close deliberately emits nothing: that is an operator's action, and reporting it would
attribute a human decision to the policy. Best-effort otherwise, logged at warn, since a failure
means that trade trains nothing.

**Early close** (`action == "close"`): implemented, gated behind `paper_trading.rl_early_close`,
**off by default**. It closes at market and records `close_reason = 'rl_early'` (migration
`000008`) so early-closed trades stay distinguishable from operator action *and* comparable against
trades that ran to SL/TP — the same evidence-gathering logic as the shadow forks. It stays opt-in
because it is the one lifecycle action that destroys the counterfactual: an early-closed trade can
never show what it would have done.

**Signal carry-forward**: the last signal per (inst_id, bar) is retained by the conductor and
re-attached to price-driven `update` calls, since a higher-timeframe opinion stays meaningful
between its candles and dropping it at candle close would hide it from every update in between.
Retention is per (instId, bar) — a 1H signal must not leak onto a 5m decision, or one token's onto
another's. Note §15.11 dropped the signal's `age_seconds`, so staleness reaches the model only
through `PositionState.AgeSeconds`; if carry-forward proves to need explicit staleness, that is an
observation-schema change, not a conductor change.

**Forks generate their own updates** (§15.4): they are open positions and the model manages them
too. But a fork is never itself forked — `applyAdjustment` returns early for `Variant ==
"rl_adjusted"` — otherwise each adjustment spawns a new branch and the tree grows without bound,
every leaf drawing model calls forever.

**Go-side clamps** (`conductor.Clamps`, config `paper_trading.rl_clamps`, the `RatchetSLTP` pattern
— the model is never the safety boundary): minimum and maximum SL distance and a minimum TP:SL
ratio, applied to the levels the model sets **on an open**. This is a different question from the
ratchet, which governs how levels may *move* later and says nothing about initial placement. Early
in training the policy is effectively random: a stop 0.001% from entry stops out on noise before
the trade can do anything, one 40% away turns a bounded loss into an account event at high leverage,
and a target nearer than the stop is negative-expectancy by construction no matter how good the
entry. Order matters within the clamp — the stop is clamped first and the ratio is checked against
the *clamped* stop, since validating against a rejected stop would let it silently justify a target
that no longer matches the risk actually being taken. A level on the wrong side of entry is dropped
rather than mirrored: guessing what an incoherent output meant would invent a decision the model
never made, and the strategy's own level is the better fallback.

**One model call per open decision.** `sizeFromAction` was split out of `rlSizing` so the open path
reuses the same sizing and capping rules without issuing a second `Predict` for one decision — two
calls would not only waste inference, they could return different answers and leave the order sized
against one while its levels came from the other.

**Verified end-to-end, not just unit-tested** (2026-08-28): the full lifecycle was driven through
the real `rlclient` → `rl_service` HTTP path against a live service with `learning_enabled: true`.
Four trades produced `completed_trades: 4`, `buffer_size: 4`, `pending: 0` (every decision paired
with its outcome, none orphaned), `last_reward: 0.04` (matching `realized_pnl / account_initial`),
and `updates: 3` — real SAC gradient steps. Before this change that counter would have stayed at
zero forever. Migration `000008` was also applied against a real TimescaleDB, confirming the
regenerated CHECK constraint accepts `rl_early` and still rejects an invalid reason (a dropped-but-
not-recreated constraint would have silently allowed any string), and that the down migration
relabels existing `rl_early` rows rather than failing on them.

### 15.13 Reward penalties in ReplayEnv (implemented 2026-08-28)

`ReplayEnv.step` computed `reward = (realized_pnl - fee_cost) / initial_equity` minus the SL/TP
churn penalty, and **nothing else**. The drawdown and liquidation-proximity penalties §15.5 and §2
describe existed only in the legacy `okx_futures_env.py`, which is off by default and not what
anything trains against.

Why it mattered more than it looked: with leverage up to 100x, maximum expected PnL comes from
maximum position size, so a PnL-only reward actively taught over-leveraging while §15.6's 25%/60%
caps held that back with a hard clamp — the objective and the clamp pulling in opposite directions.
Done before any serious training run, deliberately: early learning against the wrong objective has
to be unlearned later, and at this project's data volumes that is expensive.

- **Drawdown** (`DRAWDOWN_PENALTY_WEIGHT`, `_drawdown_pct`): charges for distance below the
  account's high-water mark. This is what makes a round-trip cost something — reward is otherwise
  computed per step from realized PnL, so running the account to 2x and giving it all back collects
  the gains on the way up and pays the losses on the way down, netting to ~0 and reading as no worse
  than never having traded. Measured: that round-trip now scores **−0.625** instead of ~0.
  `peak_equity` is new state (it was not tracked at all) and is deliberately **not** reset at a
  token boundary — with one shared account (§15.6), clearing the high-water mark on crossing to the
  next instrument would teach the same "losses don't follow me" lesson the equity carry exists to
  prevent.
- **Liquidation proximity** (`LIQ_PENALTY_WEIGHT`, `_liquidation_penalty`): zero when flat or when
  estimated distance to liquidation is ≥ `LIQ_BUFFER_FLOOR_PCT` (10%), ramping linearly to 1.0 as
  that distance closes. This is the term that makes **leverage itself** expensive: the drawdown term
  only charges for losses already taken, so without it the agent could hold maximum leverage
  indefinitely at no cost right up until it blew up. Measured curve: free to ~10x, then −0.015 at
  10x, −0.165 at 20x, −0.285/step at 100x. Strong enough to discourage, not an outright ban — a
  genuinely good high-leverage trade can still pay for it, which is the intent.
- Both terms are surfaced in `step`'s `info` dict. A reward falling because of risk and one falling
  because of bad entries need completely different fixes, and an aggregate reward curve cannot tell
  them apart.

**Weights are a starting point, not tuned values.** `0.5`/`0.3` are carried over from the legacy env
and were never validated against this env's reward scale (PnL normalized by account size). Revisit
them against real training curves.

**A real bug surfaced while testing this**, unrelated to the penalties but hidden by their absence:
`step` assigned `self.leverage = next_leverage` on *every* step, so a position opened at 100x
silently became 1x as soon as the policy stopped asking for leverage (a `none` action carries
`leverage_frac=0`). The recorded leverage described the last action rather than the trade being
held — which is why the liquidation penalty read 0 for a 100x position and the first version of the
leverage test failed. Leverage is now stamped only when exposure is actually established, and reset
to 1x when flat. Nothing before this could have noticed: no test read leverage on a held position,
and no reward term depended on it.

10 new tests (54 Python total), including the property §15.13 named outright — an identical trade at
higher leverage earns strictly less reward — plus monotonicity across 1x→100x, and a training run
that stays finite with the penalties active. Mutation-checked: removing the two penalty lines fails
three of them.

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
The built-in strategies' logic (14 kinds registered in `strategy.Factories` today) is this
project's actual domain knowledge and lives in
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
as a new `strategy.Strategy` kind (e.g. `rsi_sma_fuzzy`) alongside the existing kinds, not a
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

### 16.8 Strategy audit: structural price levels (2026-08-28)

§15.11 made a strategy's proposed `entry_px`/`sl_px`/`tp_px` part of the observation, because a
level derived from chart structure (a stop below a swing low, a target at a fair-value gap) carries
information a percentage cannot. This audit checked all 14 registered kinds against that.

**Result: nothing was deleted.** All 14 were emitting percentages only, but the split is not
"good vs. bad strategies" — it is whether the logic actually *computes* a structural level:

| strategy | what it was discarding |
|---|---|
| `double_top_bottom` | `p2` (the pattern's own extreme) as the stop, the Fib projection as the target |
| `dual_ma_atr` | swing low − ATR×mult as the stop |
| `pmax` | its own ATR trailing band — the strategy's literal definition of "I am wrong here" |
| `trend_confluence` | an ATR-scaled distance, round-tripped through a percentage |
| `pivot_reversal` | the armed pivot it breaks out *through* → entry |
| `seasonal_atr_short` | `sellLevel` (SMA + ATR×mult) → entry |
| `weekly_dip_buy` | `dipLevel` off the week's open → entry |

The other seven — `rsi_sma`, `rsi_sma_fuzzy`, `sma_cross_fixed_exit`, `ema_cross_trailing`,
`stepped_trailing`, `stoch_cross`, `grid_like` — are pure oscillators/crossovers with no structural
level in their logic (for `sma_cross_fixed_exit`, fixed exits *are* the strategy). They keep
percentages deliberately: inventing a level would hand the model one no strategy chose, and a wrong
level misleads it more than no level does. The same reasoning applies *within* `pivot_reversal`,
which gained an entry but kept percentage stops — only one pivot is armed at a time there, so the
opposite one is frequently stale and would sometimes place the stop at an unrelated earlier swing.

Percentages are still emitted alongside the prices, so anything reading `SLPct`/`TPPct` is
unaffected; `ResolveLevels` leaves an explicitly-set level alone, so the price is what reaches the
model. Note one real behavior change: because `ResolveLevels` derives percentage levels from
`EntryPx`, the strategies that now report a structural entry measure their risk from *that* price
rather than from a candle close that may have overshot it.

**Four pre-existing bugs surfaced**, none related to the level work itself — they were simply
invisible while only percentages were emitted:

1. **`pmax` could never emit a signal at all.** It compared the current bar's moving average against
   a band derived from that *same* average (`ma > ma + multiplier*atr`) — impossible for any
   positive ATR, so the trend never left its initial `+1`. The strategy was registered in
   `Factories`, assignable from the panel, and silently returning `Hold` forever. The Pine source
   (`strategy_PMax Explorer`, lines 82-90) compares against the PREVIOUS bar's *ratcheted* band; the
   port had dropped both the ratchet and the `Prev`. Nothing caught it because no test asserted the
   strategy ever fires.
2. **`double_top_bottom`'s target was inverted for double bottoms.** The Pine source keeps `height`
   signed (`avg(y2,y4) - y3`), so `_t = y3 - height*fib/100` projects down from a top and up from a
   bottom with one formula. The port took `.Abs()` of height, so the target always projected
   downward. `.Abs()` on the resulting *distance* hid it: the magnitude was right, and the wrong
   direction only became visible once the level itself was reported. Fixed by restoring the source's
   signed-height convention rather than branching per direction — the branch produced correct
   numbers but diverged structurally from the source, which is what made the bug possible.
3. **`EMA`'s precision grew without bound.** Alpha is a repeating decimal for most periods (2/11 at
   period 10) and the recurrence multiplies the running value by `(1-alpha)` every bar;
   `decimal.Decimal` is arbitrary-precision, so nothing truncated it. Measured 498 digits at 40
   candles, 1,139 at 80, 4,659 at 300 — growing linearly with the candle window forever.
   `PaperTrader` maintains long windows and several strategies use EMA, so these values were
   reaching `NUMERIC` columns and every model observation. Now rounded to `emaScale` (12dp) *per
   step* — rounding the accumulator is what stops the growth, since the previous value feeds the
   next multiplication. Flat at ~15 digits at any window size. `emaScale` is deliberately NOT a
   `ParamSpec`: it is a numerical guard, not a trading parameter, and exposing it would add a
   meaningless dimension to the optimizer's search.
4. **`WithParams` leaked accumulated state in 7 of 14 strategies.** The natural Go implementation is
   `cp := *s`, which copies evaluation state (previous MA values, trend direction, armed pivots,
   trailing bands, the grid's anchor price) along with the configuration. A strategy warmed up on
   hundreds of candles would hand that state to a differently-configured variant, which then
   evaluates its first bar mid-trend using numbers computed with *different parameters* — e.g.
   comparing an MA-20 `prevShortMA` against an MA-50 `shortMANow`, a meaningless comparison that
   manufactures a crossover that never happened. `grid_like` was the worst case: it carries
   `baseline`, the price the grid is anchored to. `double_top_bottom` was subtler still — its
   `pivots` slice was *shared* by a shallow copy, so two variants could overwrite each other's log.
   - **Not broken in production today**: `internal/optimizer/runner.go`'s `buildStrategy` calls this
     on a fresh `factory()` instance. But nothing enforced that, and a leak here would score tuning
     candidates against contaminated state — silently meaningless results, not a visible failure.
   - Each stateful strategy now implements a private `resetState()` **beside its state fields** and
     calls it from `WithParams`. The field list lives next to the fields it resets, so adding a
     field means updating the reset in the same place rather than remembering a zeroing line at the
     bottom of `WithParams`. `Strategy.WithParams`' doc comment now states the requirement.
   - `TestWithParams_DoesNotCarryAccumulatedState` enforces it across **every** kind in `Factories`
     by warming an instance on 400 candles and asserting a copy made from it behaves identically to
     one made from a fresh instance. That test is what found `grid_like`, which a by-eye field-name
     scan had missed.

15 new tests (185 Go total). The `pmax`, EMA, and state-isolation fixes are mutation-checked:
reverting each one fails the test written for it.

### 16.9 Bringing the RL loop live: four bugs the rollout exposed (2026-08-29)

Turning on `learning_enabled` → `rl_sizing` → `rl_sltp_adjust` for the first time surfaced four
defects, none of which any test or metric would have caught, because each one failed *silently*.
Recorded together because they share a theme: **a path that declines to act leaves no trace unless
something is written to make it visible.**

1. **`paper-trader` had no `RL_SERVICE_URL`.** `docker-compose.yml` set it for `trader` and `api`
   but not the one service that actually calls the model, so `rlclient` fell back to its
   `http://localhost:8000` default — inside that container, its own loopback. Every model call
   failed with connection-refused. Invisible until the flags were switched on, because before that
   the model call path had never executed in production.

2. **`decode_action` could answer the wrong question.** The action head's argmax ran over all five
   actions regardless of category, so a `buy`/`sell` call could return `none` — an update-category
   action, not an open/skip decision. `openDecision` then had no answer to act on and fell through
   to fixed sizing. Fixed with `LEGAL_ACTIONS_BY_CATEGORY`, masking the argmax to the categories in
   §15.11's table; the mask still picks the highest *legal* logit, so the policy's preference
   between `open` and `skip` survives.

3. **Every fallback was unobservable.** `sizeFromAction` returns `ok=false` on a zero `size_pct` —
   which an untrained policy emits constantly — and did so with no log line and no metric. Combined
   with (2), the result was that the model was consulted on all 522 signals, answered unusably every
   time, and `okxbot_model_open_decisions_total` stayed empty, which read as "the model was never
   called." It was called every time. `ModelOpenDecisionsTotal` now records `open`/`skip`/
   `unusable`/`unsized`/`error`, so every reason a decision did not reach an order is countable.

4. **The model was serving an 83-dim artifact while the code produced 89.** `to_vector` pads or
   truncates to whatever the loaded model expects, so at 83 the feature budget was exactly zero and
   every observation collapsed to the fixed tail — the policy never saw price or signal data at all,
   which is why it returned an identical `skip` for every input. Root cause was operational: the
   corrected model was regenerated while `rl-service` was running, and its snapshot hook wrote the
   old in-memory weights back over the new file. Regenerating with the service stopped fixed it.
   The check that matters is `SAC.load(...).observation_space.shape[0]`, not `/health`'s
   `model_loaded: true` — the latter is true for a wrongly-shaped model too.

**Two ordering bugs in the open path, found the same day:**

- **Opposing positions on one token.** `evaluateStrategies` had no check for an already-open
  position, so every assigned strategy opened independently: `grid_like` (sell) and
  `weekly_dip_buy` (buy) both opened on TRUMP-USDT-SWAP/5m. §15.12 says a `buy`/`sell` decision
  exists only when the token is flat — otherwise a firing signal is an `update`. Guarded by
  `hasOpenBaseline` (forks excluded, §15.4), and the newly-opened order is appended to the local
  `open` slice so the *next* strategy in the same pass sees it.
- **The same race across timeframes.** Each bar has its own consumer goroutine, so two bars closing
  in the same instant both read an empty book — observed as orders 70 (15m) and 71 (5m) on
  ENA-USDT-SWAP, 13ms apart. The per-call guard could not see a concurrent call. `openMu` now
  serializes the whole read-then-open sequence. The regression test releases both goroutines from a
  shared channel and runs under `-race`; mutation-checked by removing the lock (fails with `got 2`).

**A position opened with no stop-loss at all** (order 80, TRUMP-USDT-SWAP, sell, `stoch_cross`) —
the most serious defect of the day, because it is unbounded downside rather than a missed
opportunity. Four independent gaps had to line up:

- `stoch_cross` emits `TPPct` and never sets `SLPct`. That is deliberate for that strategy (it
  exits on the opposite crossover), and it is the only one of the 14 that does it.
- `ResolveLevels` derives `SLPx` from `SLPct`. Given nothing to derive from, it leaves the level at
  zero — it converts, it does not validate.
- `buildPaperOrder` writes `slPx` as nil and raises no objection.
- `conductor.Clamps` — the layer whose entire job is where levels may be placed — has exactly one
  call site, inside `openDecision`, which returns at its first line when `rl_sizing` is off. With
  the model out of the open path (the §16.9 rollout choice), **nothing validated any order at all**.

The last point is the real lesson: a safety check reachable only through the feature flag of an
optional subsystem is not a safety check. Validation now runs in `evaluateStrategies` on every
open, after the model's decision so it sees the levels the order will actually carry, and
`Clamps.EnsureStop` fills a missing stop at `MaxSLDistPct` rather than rejecting the signal —
rejecting would silently disable every target-only strategy, while a widest-bound stop keeps the
strategy's own exit intact and makes the loss finite. An order that still has no stop after that is
refused outright and logged at error.

**One baseline, many forks** — 49 forks across only 12 baselines, one parent (HYPE-USDT-SWAP order
51) carrying 8. `applyAdjustment` refused to fork a fork (`o.Variant == "rl_adjusted"`) but never
asked whether the baseline it *was* forking already had one, so every accepted adjustment branched
again. This compounds rather than merely duplicating: each fork is itself an open position drawing
its own update calls, so more forks produce more adjustments produce more forks. It also destroys
what §15.4's mechanic exists for — a same-entry A/B needs one control against one adjusted variant,
and with eight there is no longer a single "the adjusted trade" to compare against.

Fixed by allowing at most one fork per baseline: a further adjustment now EDITS the existing fork
(`updateFork`) rather than branching. The ratchet is evaluated against the **fork's** current
levels, not the baseline's — the fork is what carries the adjusted stop, so measuring from the
baseline would let an already-tightened level be re-proposed at its original distance, quietly
undoing the ratchet's only-tighten guarantee.

**A take-profit that ratcheted past entry, closing trades at a loss under `close_reason='tp'`**
(orders 100 and 110, `stoch_cross`, short, entry 2.649, original TP 2.62251). `ratchetTP` allows
the target to move toward the current price and guarded that with `proposed.LessThan(price)` — but
`price` is the LIVE price, not entry. Once price moved against the short to ~2.68, the region
"between the old TP and current price" included everything past entry, so the target ratcheted up
to 2.676 and both trades closed as take-profits with a realized PnL of −1.02.

The invariant the guard was missing: a target that crosses entry is not a target, because touching
it realizes a loss. `ratchetTP` now takes `entry` and bounds the move by it on both sides. The long
case was not reproducible in production but is guarded and tested identically — the asymmetry was
in the guard's reasoning, not in the market.

Worth noting the strategy was NOT at fault here, which is where suspicion naturally falls: the
signal carried a correct `tp_px` of 2.62251 (below entry for a short), and both `ResolveLevels` and
`buildPaperOrder` reproduced it correctly. Only the in-trade ratchet inverted it, and only on
positions the model had adjusted.

**Deadlock worth knowing about.** A freshly-initialized SAC policy skips essentially every signal,
and with no opened trade there is no closed trade, no reward, and therefore no weight update — the
policy stays random forever. Warm-start (§15.8) exists precisely to avoid this; skipping it makes
the deadlock structural, not a bug. Three ways out were considered: a bootstrap override that
forces opens until N trades have closed, a short warm-start run, or leaving `rl_sizing` off so
strategies open positions and the model only manages them via the update path. The third was chosen
— it needs no new code, starts real reward flowing immediately, and matches the roadmap's
one-thing-at-a-time rollout order.

## 17. Candle backfill (implemented 2026-08-28)

Warm-start training (§15.8) rolls out against real candle history read from Postgres's `candles`
hypertable, and a fresh database has none. The original plan was to run the live ingestor for
several days to accumulate it — unnecessary, since OKX serves that history directly. This turned
the first warm-start from a multi-day wait into a ~90 second job.

- **`GET /api/v5/market/history-candles`**, not the `/market/candles` the ingestor already used.
  The latter only serves the most recent window (a few hundred bars) and cannot page backwards, so
  it can seed a live candle window but cannot build history. `rest.Client.GetHistoryCandles` pages
  backwards from a cursor; note OKX's parameter is confusingly named `after` (meaning "older than",
  from the cursor's perspective), so the Go signature says `before` instead.
- **`usecase.Backfill`** owns the orchestration. It depends on a new narrow
  `port.HistoryCandleFetcher` rather than the full `ExchangeClient` — a data-loading job must not
  be able to place an order, and the type system should enforce that rather than the implementation
  being careful.
- **Sized in CANDLES per timeframe, not days** (a deliberate revision of the day-based depth this
  was first specified with). `ReplayEnv` advances one candle per training step, so candle count is
  what training consumes: "30 days" is ~30 rows on a 1D bar and ~8,600 on 5m. A day-based depth
  starves exactly the timeframes that need the most history — at ~96 15m candles per token, a
  50,000-timestep warm-start would loop one day of price action ~260 times, which is memorization,
  not initialization. `DefaultTargetCandles` is 1,500: ~5 days of 5m, ~15 days of 15m, ~2 months of
  1H, ~4 years of 1D.
- **Paced and idempotent.** `DefaultPageDelay` (150ms) stays well inside OKX's rate limit rather
  than racing it — the backfill shares an IP with the live trading path, so being throttled here
  would also throttle order placement. `SaveCandle` already upserts on `(inst_id, bar, ts)`, so an
  interrupted run is resumed by simply issuing it again.
- **Partial failure is expected, not fatal.** One delisted instrument or one timeframe with less
  history than requested reports its error in its own `BackfillResult` while every other pair
  completes. A stall guard exits if the cursor stops advancing, so a misbehaving endpoint can't
  produce an infinite request loop against a rate-limited API.
- **Two triggers**: `cmd/paper-trader -backfill [-backfill-candles N]` (runs and exits without
  starting the trading loop) and `POST /api/candles/backfill` on `cmd/api` (empty body = the
  configured instruments and bars). The flag lives on `cmd/paper-trader` rather than in a new
  binary because that process already has the exchange client, repository, and instrument/bar
  config wired up. The endpoint runs synchronously: it is paced and can take minutes, but a
  fire-and-forget job would need its own status endpoint and progress store to answer the only
  question anyone asks afterward ("did it work?").

**Live-verified against real OKX and a real TimescaleDB**, not just unit-tested: 9,000 candles
across 2 instruments × 3 timeframes in ~90s; re-running the full backfill left the row count
unchanged at exactly 1,800 (idempotency against real Postgres, not just the fake); 0 of 1,800 rows
had incoherent OHLC; a nonexistent instrument returned HTTP 200 with its error scoped to its own
result. Coverage matched the projection — 1H reached ~2 months back, 1D ~3 months at 100 candles.

**A test fake was fixed rather than the test weakened**: `fakeRepository.SaveCandle` appended where
the real Postgres implementation upserts, so the idempotency test failed against a fake that could
not model the behavior being asserted. A fake that diverges from its real counterpart quietly
weakens every test that uses it, so the fake now upserts on the same key.
