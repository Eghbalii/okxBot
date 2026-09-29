# CLAUDE.md — okxBot Project Memory

This file is the persistent design/context document for this repository. Read it fully before
making changes. Keep it updated whenever architecture, endpoints, or conventions change.

## 1. Goal

An OKX **futures/perpetual-swap** trading bot where a **reinforcement-learning (RL) agent**
decides:

- when to open, adjust, or close a position
- what size and leverage to use
- implicit risk management (the reward function penalizes drawdown and liquidation risk)

The system is a **hybrid Go + Python** architecture:

- **Go** (`go-engine`): realtime market-data ingestion (WebSocket), order execution, account/risk
  management, and the low-latency trading loop. Go's concurrency primitives are a good fit for a
  persistent WebSocket connection with reconnect logic, and this is production trading
  infrastructure — it should be fast and predictable.
- **Python** (`rl-service`): the RL environment, training, and model-inference server. Python owns
  the ML stack (Gymnasium-style env shapes, Stable-Baselines3, PyTorch). Exposed to Go via a small
  FastAPI HTTP service (`POST /predict`) rather than embedding Python in Go.

Communication between the two is intentionally simple: **Kafka** for realtime market-data
streaming (Go publishes, consumers subscribe) and a synchronous **HTTP call** (`POST /predict`)
that the trading loop makes to get the agent's action.

## 2. Why RL, and library choices

- **Stable-Baselines3 (SB3)** — mature, well-documented PyTorch RL implementations.
- **SAC (Soft Actor-Critic)** is the algorithm in use. SAC is off-policy, which matters here: it
  learns from a replay buffer it can keep reusing, rather than discarding a fixed-size on-policy
  rollout after one gradient update. At the trade volume this project produces (tens of trades a
  day), an on-policy algorithm like PPO would take weeks to fill one training batch; SAC can update
  after every closed trade.
- **Action space**: the model decides, per lifecycle event, whether to open/skip a signal, whether
  to hold/adjust/close an open position, and what stop-loss/take-profit/size/leverage to use. See
  §6 for the full lifecycle design.
- **Training data source: live paper-trading (forward-test), plus an offline backtest for warm
  start.** The model's primary training signal comes from a **Paper Trading Engine** that runs
  strategies against the live market feed, opens virtual positions with full features (entry, SL,
  TP, size, leverage), and tracks them against the live price feed until close. The closed trade
  (decision + realized outcome) is what SAC learns from. A separate **backtest engine**
  (`go-engine/cmd/backtest`, `internal/backtest`) replays historical candles through the same
  strategy code and the same observation-building code to produce a large warm-start dataset —
  this exists specifically so a new deployment doesn't have to wait weeks for enough live trades to
  produce a non-random policy, while keeping the *live* forward-test loop as the ongoing source of
  truth. Live inference is a frozen-until-retrained policy served over HTTP, with an option to
  keep learning continuously from live outcomes (§7).

## 3. Repository layout

```
okxBot/
├── CLAUDE.md                  # this file
├── README.md                  # quickstart + infra/resource requirements
├── docker-compose.yml         # kafka + redis + timescaledb + prometheus/grafana + loki/promtail
│                                 + go services + python services
├── loki-config.yml            # log aggregation storage/retention
├── promtail-config.yml        # tails Docker json-file logs off disk -> Loki
├── prometheus.yml             # metrics scrape config
├── grafana/provisioning/      # auto-provisioned Prometheus + Loki datasources
├── go-engine/                 # Go module: data ingestion + order execution + risk + API
│   ├── cmd/
│   │   ├── ingestor/          # connects an exchange's public WS, publishes ticks/candles to Kafka
│   │   ├── trader/            # live trading loop: strategy signals -> RL model -> real orders
│   │   ├── paper-trader/      # forward-test engine — the RL training-data source
│   │   ├── backtest/          # offline replay engine: strategies + observation builders over
│   │   │                        historical candles, producing a warm-start training dataset
│   │   ├── strategy-optimizer/# tunes strategy parameters against live market data (independent
│   │   │                        of the RL agent — see §10)
│   │   ├── okx-gateway/       # the one process holding real exchange credentials; every other
│   │   │                        service reaches an exchange through this (exchange-parameterized:
│   │   │                        the same binary serves OKX or MEXC)
│   │   └── api/               # dashboard/reporting HTTP API
│   ├── internal/
│   │   ├── domain/            # core entities: Candle, Ticker, Position, Balance, Order,
│   │   │                        Observation, Action — no framework/IO deps
│   │   ├── usecase/           # application logic: PaperTrader, BotTrader, ManualTrader — depend
│   │   │                        only on domain + port interfaces
│   │   │   └── conductor/     # SignalConductor — the RL signal-lifecycle state machine: which
│   │   │                        decision to ask the model for, at what cadence. Pure, no IO.
│   │   ├── port/               # interfaces use-cases depend on: Repository, ExchangeClient,
│   │   │                        ModelClient, MarketDataConsumer/Publisher, SymbolResolver
│   │   ├── config/             # env/yaml config loading
│   │   ├── okx/                 # OKX adapter (implements port.ExchangeClient)
│   │   │   ├── rest/            # signed REST client: orders, leverage, positions, balance, algo
│   │   │   │                     orders (exchange-side stop-loss/take-profit)
│   │   │   └── ws/              # public + private websocket clients (reconnect, heartbeat)
│   │   ├── mexc/                # MEXC adapter — a second exchange satisfying the same
│   │   │                          port.ExchangeClient interface, proof the ports-and-adapters
│   │   │                          design isn't OKX-shaped (see §11)
│   │   ├── gateway/              # the exchange-agnostic gateway service logic: per-consumer rate
│   │   │                          limiting, retry/backoff, metrics — knows nothing about OKX
│   │   │                          specifically
│   │   ├── gatewayclient/        # HTTP client other services use to talk to okx-gateway
│   │   ├── postgres/            # TimescaleDB/Postgres adapter (implements port.Repository)
│   │   ├── kafkastream/           # Kafka producer/consumer + instrument dispatcher (the event
│   │   │                           bus); implements port.MarketDataConsumer/Publisher
│   │   ├── strategy/              # strategy registry + indicator library (60+ built-in kinds)
│   │   ├── backtest/              # the offline replay engine's core logic
│   │   ├── optimizer/             # the strategy-parameter-optimizer's trial lifecycle
│   │   ├── risk/                  # hard risk limits (circuit breakers) independent of the RL model
│   │   └── rlclient/               # HTTP client for the Python inference API
│   └── configs/config.example.yaml
├── rl-service/                 # Python: RL environment, training, inference
│   ├── requirements.txt
│   ├── rl_service/
│   │   ├── obs.py               # observation/action encoding shared by inference and training
│   │   ├── reward.py            # the risk-adjusted reward function
│   │   ├── warmstart.py         # consumes the backtest's dataset to pretrain a fresh model
│   │   ├── learner.py           # continuous online-learning loop for live paper trading
│   │   └── serve/                # FastAPI inference app
│   └── configs/config.example.yaml
├── optimizer-service/          # Python: minimal FastAPI + Optuna sidecar proposing candidate
│                                  strategy-parameter sets — no ML model, no torch
├── panel/                      # React + TypeScript + Vite frontend: positions/strategies/
│                                  model-status/resources/account/trade pages
└── data/                       # gitignored local data cache (candles, model artifacts)
```

## 4. Exchange API notes

- The exchange REST/WS boundary is abstracted behind `port.ExchangeClient` and
  `port.SymbolResolver` — nothing in `internal/usecase` imports an exchange adapter package
  directly (enforced by a static test that parses imports at the AST level).
- **OKX** is the primary, currently-live exchange. Key characteristics that shaped the adapter:
  - REST/WS split by region: `www.okx.com`/`ws.okx.com` reject requests from some regions with a
    misleading "API key doesn't exist" error; the EEA-hosted deployment uses `my.okx.com` /
    `wseea.okx.com` instead. This applies to both the REST base URL and the authenticated private
    WebSocket, independently — check both if this recurs.
  - Real-money trading on this account uses OKX's newer USD/USDC-settled "X-Perp" perpetual
    product, whose instrument id embeds a rolling expiry date that OKX periodically renews (e.g.
    `BTC-USD_UM_XPERP-<date>`). This is exactly why symbol resolution is a pluggable
    `port.SymbolResolver`: OKX needs a small, periodically-refreshed table; a second exchange
    (MEXC) needs none, since its perpetual symbols (`BTC_USDT`) are stable.
  - Exchange-side stop-loss/take-profit is placed as a single OCO-style "algo order" carrying both
    trigger prices — not two separate orders, since OKX cancels the other leg automatically when
    one triggers, and two independent orders would leave the losing side resting after the winner
    fires.
  - Demo/simulated trading uses the same hosts with `x-simulated-trading: 1` on REST. **The
    simulated environment does not carry every real-money product** — confirmed live that OKX's
    X-Perp instruments used for real trading are unavailable in demo, which is a hard constraint on
    what can be dry-run there.
  - Auth headers: `OK-ACCESS-KEY`, `OK-ACCESS-SIGN`, `OK-ACCESS-TIMESTAMP`, `OK-ACCESS-PASSPHRASE`.
    Signature = `base64(HMAC_SHA256(secret, timestamp + method + requestPath + body))`.
- **MEXC** is a second, fully-implemented adapter (`internal/mexc`) used today for a parallel
  paper-trading comparison, not for real-money trading. It differs from OKX in ways the adapter
  hides completely from the rest of the codebase: no passphrase, a numeric (not string) response
  code, column-array kline payloads instead of row-arrays, second-granularity kline timestamps
  where OKX uses milliseconds, no explicit "this candle is closed" flag (closure is inferred from a
  newer bar arriving for the same symbol/interval), and stop-loss/take-profit attached to a
  position rather than existing as a separate order with its own id.
- Real credentials live **only** in `okx-gateway`'s own environment/config, never checked into git,
  never loaded by any other process. See §11 for the gateway design and why this centralization
  matters.

## 5. Risk management (independent of the RL model)

The Go `internal/risk` package enforces hard limits the RL agent cannot override:

- Max leverage cap, independent of whatever leverage the RL model proposes — the model's output is
  clamped to this before it ever reaches an order.
- Max position notional / max % of account equity per position, and a max total exposure ratio
  across all open positions.
- A hard cap on realized loss as a fraction of margin, applied at multiple independent layers (the
  opening decision, every in-trade adjustment, and the strategy-optimizer's own trial evaluation) —
  deliberately redundant, since a single enforcement point that gets bypassed once is a single
  point of failure.
- A minimum reward:risk ratio ceiling on take-profit placement, so a strategy's raw stop distance
  can never be paired with an unreachable target.
- Distance-to-liquidation floor: reject any action that would push the estimated liquidation price
  within a configured margin of the current mark price, cross-checked against the exchange's own
  reported liquidation price where available (never let a cross-check *loosen* a safety margin,
  only tighten it).
- A process-wide halt (`risk.Manager.Halt`) that stops new real-money opens on conditions like a
  margin-mode mismatch or an unexplained position appearing on the exchange that this system has no
  local record of. The halt is re-derivable from the exchange's own state plus this system's
  database (not just an in-memory flag), which is what lets an operator safely clear a halt once
  the underlying condition is confirmed resolved.

This is deliberately implemented in Go, not Python: a bug or bad output in the RL service must
never be able to bypass hard safety limits.

## 6. Signal lifecycle and the Signal Conductor

The RL agent doesn't poll a generic world snapshot — it's driven by an event-based **signal
lifecycle**, where the signal's category tells it which decision it's being asked to make:

| Category | When | Model decides |
|---|---|---|
| `buy` / `sell` | a strategy fires, no open position on this token | open or skip; size, leverage, SL/TP |
| `update` | a position is open and another signal fires, or unrealized PnL moves past a threshold, or a time ceiling elapses | hold, adjust SL/TP, or close early |
| `closed_*` | the position closes (SL touch, TP touch, early close, timeout) | nothing — this event *is* the reward |

A Go component, the **Signal Conductor** (`internal/usecase/conductor`), owns this lifecycle per
instrument: it decides which category applies and at what cadence, and guarantees the terminal
call that delivers the reward always actually happens — every close routes through one code path,
so no caller can complete a close while skipping the model's terminal call. The Conductor is pure
logic with no IO; the surrounding `PaperTrader`/`BotTrader` types own the repository and network
calls and delegate the decision to it.

Update cadence is **event-driven, not fixed-interval**: an update fires when unrealized PnL moves
past a threshold, when a time ceiling elapses (so a position sitting flat is still periodically
observed), or immediately whenever a strategy produces a fresh opinion — never filtered.

Stop-loss/take-profit adjustments are a **one-way ratchet on the stop** (can only move to reduce
risk, never loosen or undo a prior tightening) with the take-profit side allowed to move more
freely in either direction, subject to the same reward:risk ceiling as the open path and a hard
rule that a target can never cross the position's own entry price (which would turn a "take
profit" into a realized loss).

## 7. RL observation, action, and training design

- **One shared policy** trained on pooled experience across every active instrument, with token
  identity encoded as a compact "token profile" (typical volatility, volume rank, price, recent
  change) rather than a one-hot over a specific token id. This scales to a growing/changing
  instrument roster without a fixed ceiling and without reassigning slot meanings whenever the
  roster changes — a one-hot approach hit exactly that problem once the token list grew past its
  original slot count.
- **Observation** is a fixed-width vector covering: token profile, account/risk state, lifecycle
  category, per-strategy live track record (win rate, trade count, typical reward:risk), the
  current signal's own scalars, open-position state (if any), the token's own recent price/candle
  context with several derived indicators, and a reference block built from a fixed reference
  instrument (BTC) so the model has some visibility into the broader market, not only the token
  it's deciding on. The exact width is versioned and validated end-to-end: a builder that can't
  produce a complete, correctly-shaped observation causes the caller to skip the model call rather
  than send a malformed or padded vector — there is no silent truncation or padding at any layer.
- **Action** is a small fixed-width vector, split into an "open" head (open/skip) and a "manage"
  head (none/update/close), each read only in the category it applies to — separating these matters
  because training a head on data it never causally influenced (e.g. training the close output on
  every buy/sell call, when it was never read for those) gives it no corrective signal and lets it
  drift to its output bound.
- **Reward** is a single risk-adjusted function shared between live serving and training: realized
  PnL minus fees, normalized by the risk actually taken (not by position size — two trades with the
  same dollar gain but very different stop distances are not equally good), minus a leverage
  penalty, a churn penalty on frequent SL/TP adjustments, and a drawdown penalty so a full
  round-trip (up then back down) doesn't net to a near-zero reward the way a naive per-step PnL sum
  would.
- **Continuous learning**: the model can keep learning from live paper-trading outcomes between
  training runs (an off-policy algorithm like SAC supports this naturally via its replay buffer),
  with a hard requirement that model weights and the replay buffer are always saved and restored
  together — restoring weights alone would come back having forgotten every experience that shaped
  them.
- **Warm start**: `cmd/backtest` replays historical candles through the *same* strategy
  implementations and the *same* Go observation-building code the live path uses, simulating each
  trade to its SL/TP outcome, and emits a JSONL dataset `rl_service/warmstart.py` consumes to
  pretrain a fresh model before it ever sees live data — breaking the structural deadlock where a
  freshly-initialized, effectively-random policy skips every signal, so nothing ever opens, closes,
  or produces a reward to learn from.

## 8. Strategy engine

- A **Strategy** is a pluggable signal generator: given a window of candles and indicator values
  for one instrument, it produces a signal (buy/sell/hold) with a suggested entry/stop-loss/
  take-profit and a confidence score. All built-in strategies implement one small interface, so
  hand-written and future user-authored strategies look identical to the engine.
- **60+ built-in strategies** span classic technical indicators (moving averages, RSI, MACD,
  Bollinger Bands, Keltner Channels, stochastic oscillators), ICT/smart-money concepts (order
  blocks, fair value gaps, liquidity sweeps), classic price-action patterns (engulfing candles,
  inside-bar breakouts, pivot reversals), and a few structural additions: a regime filter wrapper,
  a confluence combiner that only fires when several member strategies agree within a short window,
  a BTC-divergence strategy (an altcoin diverging from BTC's own move, in either the "follow" or
  "fade" direction), a session-momentum strategy keyed on fixed time-of-day windows, and a `coin_flip`
  null-strategy baseline used purely to measure whether any other strategy's edge is statistically
  distinguishable from chance.
- Strategies decide **when** there's a tradeable setup and its **direction**; the RL agent decides
  **how much** (size, leverage) and can adjust or close the resulting position. The agent never
  flips a strategy's chosen side.
- A strategy that computes a genuinely structural price level (a swing low, a fair-value-gap edge,
  an ATR-scaled trailing band) reports that level directly rather than only a percentage distance —
  a percentage discards exactly the structural information that made the level meaningful. A
  strategy with no structural level in its own logic reports a percentage honestly rather than
  inventing a level nothing in its logic actually chose.
- **Per-instrument, per-timeframe assignment** is a database row, not a code change: the same
  strategy "kind" can run as several independently-tuned variants on different instrument/timeframe
  pairs simultaneously, all descending from a locked, never-edited "origin" row that keeps the
  built-in default always available as a reference/reset target.
- **`WithParams`** (producing a differently-configured copy of a stateful strategy) must reset all
  accumulated evaluation state, not just configuration — a naive shallow copy would let a
  differently-tuned variant inherit a stateful strategy's mid-trend internals and manufacture a
  crossover event that never happened. A dedicated test warms every registered kind on a long
  candle window and asserts a copy behaves identically to a fresh instance.

## 9. Strategy parameter optimizer

A **separate, deliberately independent** service (`cmd/strategy-optimizer` +
`optimizer-service`) that tunes a strategy's own numeric parameters (indicator periods,
thresholds, ATR multipliers) against live market data, decoupled from the RL agent so the two
systems can never contaminate each other's signal.

Why separate: the RL agent's own decisions (sizing, early closes, in-trade SL/TP adjustment) are
themselves still being learned, so letting them influence which strategy parameters get judged
"good" would mean judging a strategy's raw quality against a moving target. The optimizer's
win/loss judgment is strictly "did price touch this signal's own stop-loss or take-profit" —
never realized PnL, never anything the RL agent touched.

- **Go owns the trial lifecycle**: it constructs live `strategy.Strategy` values with candidate
  parameters (no reimplementation of strategy logic in Python) and evaluates them against the real
  tick/candle feed, tracking trial state as disposable, TTL'd Redis entries rather than durable
  database rows.
- **Python (`optimizer-service`) only proposes candidates**: a minimal FastAPI + Optuna sidecar
  with no ML model and no PyTorch dependency, using Optuna's ask/tell API since Go drives the trial
  loop rather than Optuna's own `optimize()`.
- A winning candidate is persisted as a new sub-strategy row and assignment, entering the same
  real trading pipeline as any manually-tuned strategy. A candidate's generation number reflects
  the lineage's full history (a real `MAX(generation)` aggregate), not just its immediate parent —
  so retrying after a rejected attempt correctly advances rather than colliding on the same number.
- Auto-promotion of a new candidate is gated on the *currently active* candidate having
  accumulated a minimum number of real closed live trades first — a backtest pass alone says
  nothing about whether the running strategy is actually worse; only live data can answer that.

## 10. Event-driven design

The ingestion layer is event-driven (exchange WebSocket → Kafka topic), and every downstream
consumer (paper-trading engine, strategy optimizer, the panel's WebSocket bridge) is a handler
subscribing to that bus, coded against `port.MarketDataConsumer`/`MarketDataPublisher` so the
transport stays swappable.

- **Kafka** (single-broker Kraft-mode, no separate Zookeeper) is the event bus: `<exchange>.tickers`,
  `<exchange>.candles.<bar>` (one topic per timeframe), `<exchange>.orderbook`, and
  `okx.paper-order-events` (position open/close notifications for the panel's live push).
- **Redis** is used only for the strategy-optimizer's disposable trial state — a TTL'd
  key-value use case Kafka doesn't fit, unrelated to the event bus.
- Consumer groups are scoped per exchange/instance (e.g. `paper-trader-<exchange>`) so two
  independent paper-trading instances (say, one against OKX and one against MEXC for comparison)
  never rebalance against each other's Kafka group — a real degradation this project has hit before
  and fixed by scoping the group name.
- Kafka consumer goroutines retry transient fetch/commit errors with exponential backoff rather
  than exiting on the first error — a consumer that silently stops (with the surrounding process
  otherwise looking completely healthy) is worse than one that keeps retrying forever, since there
  is no error a live trading pipeline should give up on quietly.

## 11. The exchange gateway (multi-exchange, credential-isolated)

Every other service calls a gateway service over internal HTTP instead of constructing its own
exchange REST client. `internal/gateway` (rate limiting, retry/backoff, metrics) carries no
exchange-specific code at all — it's built from a small set of interfaces and a pluggable retry
predicate, with an exchange's own quirks (auth scheme, response envelope, retryable-error codes)
living entirely inside that exchange's adapter package. Its HTTP routes are exchange-agnostic
(`/ticker`, `/order`, not an OKX-specific path shape).

**Each exchange runs its own gateway instance**, not one shared process — a real credential and
rate-limit boundary should never be shared across exchanges. Both instances are built from the same
generic binary, parameterized at startup by which exchange adapter to load, so adding an exchange
in production is "start another instance pointed at a new adapter," not "write a new service."

Why centralize rather than give each service its own rate limiter: several independent processes
each enforcing their own local rate limit cannot see each other's request volume, so the actual
aggregate hitting the exchange is unbounded by any single service's limiter. Each gateway instance
enforces per-consumer, per-endpoint-class token buckets with strict priority for the live trading
consumer over background consumers (backtesting, paper trading, strategy discovery), retries
transient exchange errors with exponential backoff, and reports request volume by
consumer/endpoint/outcome as a metric — the one place that can answer "who is spending how much of
this exchange's rate-limit budget."

This design was validated by adding a second, fully independent exchange adapter (MEXC) that
satisfies the same `port.ExchangeClient` interface with zero changes to the gateway logic, the risk
manager, or any use-case code — proof that the ports-and-adapters boundary is a real abstraction and
not accidentally shaped around OKX's specifics.

**Known naming leftover**: the gateway binary's directory (`cmd/okx-gateway`), its Dockerfile
(`Dockerfile.okx-gateway`), and its config env var (`OKX_GATEWAY_URL`) all predate the
multi-exchange design and still carry an OKX-specific name, even though every instance — including
the MEXC one — runs from that same binary/Dockerfile and reads that same env var name. This is a
cosmetic cleanup item (rename to `cmd/gateway`, `Dockerfile.gateway`, `GATEWAY_URL`), not a design
constraint — `internal/gateway`, the package that actually matters for the multi-exchange claim,
already carries no exchange suffix.

## 12. Clean architecture (ports and adapters)

`go-engine` is organized ports-and-adapters style specifically so an exchange, a database, or the
event-bus transport can be swapped without touching business logic:

- `internal/domain` — plain entities (Candle, Ticker, Position, Balance, OrderRequest, Observation,
  Action), no framework/IO imports.
- `internal/usecase` — application logic: `PaperTrader` (the forward-test loop), `BotTrader` (the
  live automated trading loop), `ManualTrader` (a discretionary, operator-driven trading path) —
  depending only on `domain` and `port` interfaces. This is where the real business rules live and
  what's unit-tested without a real exchange or database.
- `internal/port` — interfaces the use-cases depend on: `ExchangeClient`, `Repository`,
  `MarketDataConsumer`/`MarketDataPublisher`, `ModelClient`, `SymbolResolver`.
- Adapters implement those ports and convert their own wire/storage formats to and from `domain`
  types at the boundary: `internal/okx` and `internal/mexc` implement `ExchangeClient`,
  `internal/postgres` implements `Repository`, `internal/kafkastream` implements
  `MarketDataConsumer`/`Publisher`, `internal/rlclient` implements `ModelClient`.
- A static test parses every file under `internal/usecase` at the AST level and fails the build if
  a use-case imports an exchange adapter package directly — the layering rule is enforced, not just
  documented.

## 13. Dashboard / reporting panel

A React + TypeScript + Vite frontend (`panel/`) and a Go HTTP API (`cmd/api`, built on the same
use-cases as the trading engine) expose:

- **Home** — balances per exchange, paper/bot trading service status, a sortable/filterable market
  table (price, 24h change, range, volume, a composite discovery score) with per-token icons and
  exchange marks, opening into a chart modal on click.
- **Positions** — one page per trading mode (paper / bot / manual), each with its own stats box
  (open count, equity, PnL over several windows), a config box (pause/stop, direction/timeframe/
  strategy/token controls), and a live-updating table with a chart-and-adjust view per position.
- **Strategies** — the strategy-optimizer's candidates by status (active, rejected-but-was-trading),
  with sortable backtest-vs-live columns and a parameter-change history per candidate.
- **Model status** — the RL service's health (loaded/not, observation/action compatibility),
  process liveness/uptime/restart history via the host's process supervisor, and a log tail.
- **Resources** — service health (including a real-trading halt with enough detail to safely clear
  it once the underlying condition is confirmed resolved) and a link through to Grafana for
  CPU/RAM/GPU history.
- **Account** — total assets across exchanges and per-mode (paper/bot/manual) trading-cap controls.
- **Trade** — a manual, discretionary futures trading page (chart + orderbook + order ticket) that
  bypasses strategy/RL automation entirely for direct operator-driven orders, sharing the exchange
  adapter layer and the same real-order tables as automated trading.

Network model: the panel and every backend port are reachable only over an OpenVPN tunnel, never
exposed on the open internet — there is no authentication layer in front of the panel today, so
network isolation is the only gate. This is an explicit, revisitable trade-off, not a permanent
design decision: application-level authentication (OAuth2, Google sign-in, and 2FA are the leading
candidates) is planned to sit in front of the panel itself, so access no longer depends solely on
network reachability. Until that lands, network isolation must not be relaxed.

A real-time WebSocket bridge (`GET /api/ws`) pushes position open/close events from Kafka straight
to connected panel clients, backing sound/notification alerts and instant list refreshes, with a
slower interval-based poll kept running underneath as a fallback/consistency check independent of
the socket's own connection state.

## 14. Data persistence

Two datastores, each used for a distinct role:

- **Kafka** — the ephemeral real-time event bus (§10): ticks, candles, and position-event traffic
  in transit between the ingestor and every consumer. Time-based retention, not meant for long-term
  storage.
- **TimescaleDB** (Postgres + the Timescale extension) — durable storage: candles (a hypertable,
  keyed by exchange/instrument/timeframe/timestamp), paper and bot orders with their full feature
  snapshot at decision time, strategy definitions and per-instrument/timeframe assignments, account
  equity and its full history (every balance change recorded with its reason, in the same
  transaction as the balance update itself), and the strategy-optimizer's candidate lineage.
- All monetary/price columns use an arbitrary-precision decimal type end to end (Go's
  `decimal.Decimal`, Postgres `NUMERIC`) — `float64` cannot represent most decimal fractions
  exactly, which matters over repeated price/PnL arithmetic. Any *recurrence* that feeds a
  decimal value back into its own next computation (an exponential moving average, a running
  position size derived from account equity) must round at each step regardless — arbitrary
  precision without rounding grows the value's digit count without bound, which has caused real
  incidents in this project (a stalled paper-trading engine when a size column's precision grew
  past the database's own numeric limit).
- A schema change that alters an existing column's *shape* (not just adds a new one) should ship in
  the same deploy as the code that depends on the new shape — deploying them separately can crash
  the depending service in the gap between the two.

## 15. Conventions

- Go: standard `gofmt`, errors wrapped with `%w`, structured logging (`log/slog`).
- Python: type hints throughout, Pydantic models for API request/response schemas.
- Commit style: one focused commit per logical step. Conventional commit prefixes (`feat:`,
  `chore:`, `docs:`, `fix:`).
- Secrets: `.env` and `configs/config.yaml` (real, non-example) are gitignored. Only
  `*.example.yaml` / `.env.example` are committed. Real exchange API credentials must never be
  committed in any form, in any file, at any point — including in a config backup, a shell script,
  or a commit message.
- Always develop and test against an exchange's simulated/demo trading mode before real money is
  involved. Enabling real trading is a configuration change gated behind an explicit flag, never a
  default.
- Never validate at a boundary that can't actually receive bad input; do validate thoroughly at the
  boundary between an untrusted/external source (an exchange response, a live model output) and
  anything that places real capital at risk.
- Prefer clamping an out-of-range value to a safe bound over rejecting it outright, when the
  quantity in question is something an operator or an early-stage model is expected to occasionally
  get wrong (e.g. a stop-loss distance) — rejection can silently disable an entire code path (a
  strategy whose stop-loss the model widened past the allowed range simply never opens), while a
  clamp keeps the underlying decision intact with its risk bounded.

## 16. Testing

- Go: unit tests throughout `internal/`, hand-rolled fakes for `port.Repository`/`ExchangeClient`
  rather than a mocking framework, so tests read as plain Go. A handful of integration-style tests
  require a real local Postgres/Redis and skip cleanly when neither is reachable.
- A regression test for a found bug is only trustworthy once verified to actually fail against the
  reverted fix — a test that passes whether or not the bug is present proves nothing about the bug
  it was written for.
- Python: `pytest`, with property-style tests (e.g. "the same trade at higher leverage must score
  strictly less reward") preferred over pinning exact numeric outputs, since the reward function's
  calibration is expected to be revisited.

## 17. Roadmap / open items

- **Application-level authentication for the panel** (OAuth2, Google sign-in, and 2FA are the
  leading candidates) so access no longer depends solely on network isolation (§13).
- **Rename the gateway binary/Dockerfile/env-var off their legacy OKX-specific names** (§11) —
  `cmd/okx-gateway` → `cmd/gateway`, `Dockerfile.okx-gateway` → `Dockerfile.gateway`,
  `OKX_GATEWAY_URL` → `GATEWAY_URL` — a naming cleanup with no behavior change, deferred only
  because it touches every service's deploy config at once.
- Real-money trading is implemented (order placement, exchange-side stop-loss/take-profit,
  reconciliation against the exchange's own reported state, a manual discretionary trading path)
  but is deliberately gated behind explicit configuration flags and is not the default state of a
  fresh deployment.
- A private-exchange-WebSocket push for lower-latency position/balance reconciliation exists for
  OKX; MEXC currently relies on periodic REST polling only, which is a legitimate baseline rather
  than a gap that blocks anything.
- Per-token reward/PnL breakdown in training logs (not just an aggregate curve) is the mechanism
  for catching a policy that looks good on average while quietly doing badly on one specific
  instrument — worth having before the traded-instrument roster grows much further.
- A user-authored strategy scripting layer (beyond cloning and re-parameterizing a built-in kind)
  is a natural next step but a substantial feature on its own; a constrained JSON/DSL of indicator
  conditions is the likely starting shape rather than an embedded general-purpose scripting
  language.
- Sentiment/news-based signal sources are designed to slot into the same strategy-registry pattern
  used for every existing signal source, requiring no architecture change when added.

## 18. License

**Business Source License 1.1** (source-available, not OSI open-source). Anyone may run, modify,
and use the code for any non-commercial purpose, including running their own trading instance.
Offering the code (or a modified version of it) as a commercial product or service to a third
party requires a separate commercial license from the copyright holder. Each released version
converts automatically to Apache 2.0 four years after its publication date. See `LICENSE` in the
repository root for the full text and the exact Additional Use Grant.
