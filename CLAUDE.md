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
│   │   └── api/               # (planned) dashboard/reporting HTTP API, §11
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
├── panel/                      # (planned) frontend reporting/strategy-management dashboard, §11
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

A new `cmd/api` service (built on the same use-cases as the trading engine, per §10) exposing:

- **Training status:** whether training is running, elapsed time, and CPU/RAM/GPU usage. The
  Python side exposes these via `rl_service/metrics.py` (e.g. `psutil` for CPU/RAM, `pynvml` if a
  GPU is ever present) on a small endpoint that `cmd/api` polls or that's scraped via Prometheus.
- **Strategy CRUD:** list, view, edit, clone-to-create, enable/disable per token (§9).
- **Reports:** total orders/profit/loss with hour/day/week filters, and per-order detail
  (entry/SL/TP, timestamps, close reason) — the `paper_orders` table (§7) is designed around
  exactly this. Chart overlay of orders is a `panel/` (frontend) concern for later, not a backend
  blocker.

**Metrics (implemented): Prometheus + Grafana**, per explicit decision — not reinvented as a
custom dashboard, kept separate from the product-specific strategy/orders API. `go-engine`
services expose `/metrics` via `internal/metrics` (`promhttp`), scraped per `prometheus.yml`:
- `okxbot_strategy_signals_total{strategy,inst_id,side}` — every strategy evaluation, incl. holds.
- `okxbot_paper_orders_opened_total{strategy,inst_id,side}` — virtual trades opened.
- `okxbot_paper_orders_closed_total{inst_id,reason}` — closed trades by reason; `reason="sl"` /
  `reason="tp"` gives the SL-hit / TP-hit counts directly (rate()/increase() in Grafana for
  hour/day/week windows, matching the panel's reporting requirement in §11).
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

Phase 3 — dashboard:
- [ ] `cmd/api` reporting/strategy-management backend (§11)
- [ ] Training status + CPU/RAM/GPU metrics endpoint (`rl_service/metrics.py`)
- [ ] `panel/` frontend (framework TBD — see open question in chat)

Phase 4 — later/optional:
- [ ] User-authored strategy scripting layer (§9)
- [ ] Sentiment/news agents + signal aggregator (§9, §13)
- [ ] Evaluate migrating the event bus off Redis Streams if scale demands it (§12)

Update the checklist above as work progresses.
