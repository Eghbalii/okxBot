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

**IMPORTANT — real OKX API credentials (added 2026-09-04).** This project now has real,
live-money OKX API keys (§27) for testing order placement/cancellation against the real account.
These must **NEVER** be committed to git, ever, in any form — not in `config.yaml`, not in a
`.env` file staged by accident, not in a shell script, not in a commit message, not in this file.
They live **only on the server**, in the server's own gitignored `.env`/`config.yaml` (per §27.1's
design, ultimately only inside `cmd/okx-gateway`'s environment once that migration is complete —
today, before that migration finishes for every service, wherever a given service's real config
currently holds them on the server).

**Never run any service, script, or test with the real API key on the local machine — full stop.**
This is not just "don't commit it" — the key must never even be typed, pasted, exported, or
loaded into a process on this laptop, not in an untracked file, not in a shell env var, not in a
one-off test script, regardless of gitignore status. All order-placement/cancellation testing
against the real account happens by SSHing to the server (`ssh okx`) and running there. Before any
`git add`/commit that touches config or env-shaped files, check the diff for a stray key/secret/
passphrase, per this project's own standing git-safety practice.

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

**Implemented 2026-08-30, after the model above was found to be aspirational rather than real.**
Until this date every service port was published on `0.0.0.0` with no firewall of any kind: Redis
(no password) and TimescaleDB (`okxbot:okxbot`) were reachable from the open internet, alongside
Grafana on its default `admin`/`admin`. Multiple hosts were actively probing the box, and one
scan drove Grafana to 1.68GB RSS and triggered a **global OOM kill** that took the host down
(§16.10). No compromise was found — Redis was empty with a default `dir`/`dbfilename`, no rogue
cron, no unexpected `authorized_keys` — but nothing had prevented one.
- **OpenVPN** on `10.8.0.0/24`, split-tunnel (`--no-route-internet`): only server traffic crosses
  the tunnel, so a client's general browsing is unaffected. Services are reached at the gateway
  address, e.g. Grafana `http://10.8.0.1:3000`, panel `:8080`, `cmd/api` `:8090`.
- **Ports still bind `0.0.0.0` deliberately**, which looks wrong and is not. VPN clients arrive on
  `tun0`, so a `127.0.0.1` bind makes every service unreachable over the tunnel; binding `tun0`'s
  address directly makes Docker fail to start whenever it precedes OpenVPN (there is no ordering
  dependency between the two units). The gate is iptables instead.
- **The DROP must live in `DOCKER-USER`**, not `INPUT`. Docker's own DNAT/FORWARD rules bypass
  `INPUT` entirely, so an `INPUT`-only rule — or a plain `ufw` rule, the obvious first instinct —
  looks correct and does nothing. A second chain (`OKXBOT_LOCK`, hooked into `INPUT`) covers the
  `docker-proxy` userspace path, which does traverse `INPUT`. Persisted via `iptables-persistent`.
- **Two follow-on bugs surfaced when a client actually connected (2026-08-30)**, neither visible
  from the server side — every service answered correctly on `10.8.0.1` locally the whole time:
  1. **The installer's own `OPENVPN_INSTALL_FORWARD` chain REJECTs all forwarded VPN traffic.**
     Installing with `--no-route-internet` makes it assume clients need nothing beyond the server
     itself, so VPN clients got `Connection refused` on every containerized service (traffic to a
     published port is *forwarded* to the container, not delivered locally). Fixed with an ACCEPT
     for `10.8.0.0/24 -> 172.16.0.0/12` **before** that REJECT, and the same line patched into
     `/etc/iptables/add-openvpn-rules.sh`, which recreates the chain from scratch on every boot and
     would otherwise silently re-break access after a reboot.
  2. **MTU blackhole when OpenVPN runs inside another VPN.** A client on Surfshark (IKEv2,
     `ipsec0` MTU 1280) completed TCP handshakes but any large response vanished: the panel (455B)
     loaded instantly while Grafana's `/login` (59KB) hung forever. This reads as a server hang and
     is not one — small packets pass, large ones are dropped with no error anywhere. Fixed server-
     side with `tun-mtu 1400` + `mssfix 1100` in `server.conf`, which clamps the MSS for every
     client rather than needing a per-client edit. Diagnose it with `ping -s 1400` vs `-s 500`
     against the gateway: 100% loss on the former and 0% on the latter is the signature.
- **Verification matters here more than usual**, because every convenient test is misleading:
  probing the public IP *from the server* routes locally and never crosses `DOCKER-USER`; a
  third-party port-checker reported SSH closed while the SSH session running the check was live.
  The rule was finally confirmed by probing from a dedicated network namespace — the connection
  was refused and the DROP counter incremented on that exact packet.

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
  `docker-compose.yml`, `restart: unless-stopped` + `docker inspect` for restart count/start time
  — note this policy was *assumed* by this section but absent from `docker-compose.yml` until
  2026-08-30, so for months any transient failure was permanent downtime, §16.10)
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
  order), signal count, win rate, realized PnL. Exposed via `cmd/api` as a computed view/query,
  not a separately maintained table.
  - **A win is a closed trade with positive realized PnL, not `close_reason='tp'`** (corrected
    2026-08-30). The original `'tp'` vs `'sl'` ratio silently stopped being meaningful once the
    RL ratchet (§15.4) began trailing stops into profit: a stop-loss touch then *realizes a gain*.
    On real data 101 of 152 `'sl'` closes were profitable, so `stepped_trailing` displayed a 0%
    win rate beside +$36 of realized PnL. The PnL was never wrong — it always summed gains and
    losses together — but reading the two side by side is what exposed the win rate. Under the
    corrected definition that strategy reads 71% (24/34), and every strategy's rate now agrees
    with the sign of its PnL. Note this makes `optimizer.currentBaseline`'s baseline PnL-derived
    while a trial's own score stays strictly SL/TP-touch based (§16.1); see the note in
    `internal/optimizer/runner.go` before re-enabling scheduled optimization runs.
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
      `/api/v5/public/instruments` lookup yet — fixed for `RealTrader`/`Trader` in §33.3, found
      load-bearing against a real account whose tradeable instrument's contract shape is nowhere
      near 1:1).
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
      nothing, producing a silent data gap on a pipeline that looks healthy. 12 new tests.
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
- [x] **(3b) Candle backfill (2026-08-28, §17)** — a fresh database has no candle history, and
      waiting days for the live ingestor to accumulate it was never necessary: OKX serves it
      directly. `cmd/paper-trader -backfill` and `POST /api/candles/backfill`, paced and
      idempotent.
- [x] **(4) It is running (2026-08-29)** — starting the model fully untrained (no pretraining
      phase, see §15.8) means the untrained policy skips every signal, which produces a structural
      deadlock (no opens → no closes → no reward → weights never change; see §16.9). Resolved by
      running with `serve.learning_enabled: true` and `rl_sltp_adjust: true`
      while `rl_sizing` stays **off**: strategies open positions, and the model manages them through
      the update path, so real rewards flow from real closed trades. `rl_early_close` stays off.
      Enabling `rl_sizing` — handing the model the open decision — is the next step once it has a
      track record. Getting here surfaced four silent-failure bugs and two ordering bugs, all
      documented in §16.9.
- [ ] **(5) Hand the model the open decision**: turn on `paper_trading.rl_sizing` once enough closed
      trades exist that the policy is no longer random (SAC's `learning_starts` is 100, so gradient
      steps do not begin before that). Watch `okxbot_model_open_decisions_total`'s `skip` vs `open`
      split — a policy still skipping everything is not ready.
      - **First live attempt, 2026-08-30/31, reverted**: `rl_sizing` was turned on directly in the
        server's deployed config around 17:00 UTC on 2026-08-30 (ahead of this checklist item, and
        with only 39 of the learner's own `completed_trades` — well short of SAC's
        `learning_starts=100` — even though total closed `paper_orders` was already 195; those are
        different counts, see below). Within ~20 minutes the policy converged to answering `skip`
        on 100% of buy/sell calls across all 10 instruments (2,488 `skip` vs 0 `open` in
        `okxbot_model_open_decisions_total` over the following ~14h) — exactly the failure mode
        this item warns about. rl-service itself never crashed or restarted and strategies kept
        firing normally (2,541 signals/24h) the whole time, so the symptom ("no new positions")
        showed up nowhere except this one metric — worth checking first next time, before assuming
        a pipeline outage. Reverted `rl_sizing` to `false` on the server and restarted
        `paper-trader`; positions resumed opening immediately via the fixed-sizing fallback path
        (`openDecision` returns `ok=false, falls through` on `unusable`/`unsized`, but returns
        `ok=true, Skip=true` — no fallback — on a genuine `skip` answer). Re-attempt only once
        `completed_trades` (not total `paper_orders`) has cleared 100 by a comfortable margin, and
        watch the skip/open split closely in the first hour after re-enabling.
      - **Fixed-sizing defaults changed 2026-08-31 (explicit operator decision), independent of the
        rl_sizing question above**: `paper_trading.notional_usd` 100 → 10 and
        `defaultPaperLeverage` (`internal/usecase/papertrade.go`) 1x → 20x — the deployed service is
        meant to run small-notional/high-leverage, so the fixed-sizing path (used whenever
        `rl_sizing` is off, or falls back on an unusable/unsized model answer) should exercise that
        combination rather than a profile the service will never actually use. A separate idea —
        randomizing notional/leverage per order to manufacture sizing variance for training — was
        considered and rejected: a value chosen by Go, independent of the observation and outside
        the model's own action, adds noise to the reward signal without building an action-reward
        relationship SAC can learn from (only the model's *own* action varying, via its exploration
        noise once `rl_sizing` is on, produces a learnable signal). §15.6's design docs' own
        `[1x, MAX_LEVERAGE]` language and the RL agent's `leverage_frac` range are unaffected — this
        only changes what a fixed/fallback order looks like.
- [x] **Timeout force-close for stale positions (§15.14, 2026-08-30)** — positions were sitting open
      a long time with barely-moving PnL, tying up an instrument's one-open-position slot (§16.9).
      `paper_trading.rl_max_open_duration` (default 6h) force-closes them, `close_reason='timeout'`,
      reported to the model as `closed_early` rather than a new one-hot category (would need an
      observation-schema bump on both Go and `rl_service` — deferred, see §15.14's reasoning). 6 new
      tests.

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
      before Phase B expands token count.
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
tradeoff for Phase A — not an oversight. Revisit toward per-token once (a) enough tokens are live
that pooled-vs-per-token data volume no longer favors pooling, or (b) evidence shows the global
agent's per-token performance (tracked individually, not just in aggregate — see §15.5) is
diverging in a way that hurts a specific token.

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
RAM, fitting comfortably on hardware well below the original 8-core/16GB planning assumption.
Widening the observation (more strategies/timeframes/tokens, §15.2's Phase B) grows the policy
network slightly but is not expected to change this order of magnitude.

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
- **Peak, with a gradient-update-heavy training workload running concurrently with the full live
  stack above**: `rl-service` briefly spiked to **~470% CPU** (PyTorch's internal BLAS/thread pool
  uses multiple cores during a training update step, not just the ~1 core rollout collection uses)
  and ~450MB RAM; every other service's usage was unaffected by the concurrent training load.
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

### 15.8 Training loop shape: continued live learning only

**Decided (revised 2026-08-29): no warm-start/replay pretraining phase.** The model trains
exclusively from live paper-trading outcomes, from a fresh/random initialization, per §2's
original commitment that the deployed model's real training signal comes from live forward-test
data, never from replayed history. A replay-environment pretraining phase (`rl_service/env/`,
`train.py --warm-start`) was designed and built, then dropped after a real run against the
backfilled BTC/ETH candle history diverged (reward and critic loss blew up to nonsensical
magnitudes, `~1e14`/`~1e27`, partway through a 50,000-timestep run) — root-caused to training
against a dataset with almost no real strategy-signal history logged yet, which is exactly the
thin/signal-less condition the replay phase would always start from at this project's actual data
volumes. Rather than build tooling to manufacture synthetic signals for a replay dataset just to
make pretraining viable, the simpler and more faithful choice is to skip it: an untrained model is
a safe, well-defined starting state (the serving path's existing no-op fallback for
`model_loaded: false`), and real training begins the moment real paper-trading trades close.

The model trains from the *actual observation vectors logged at decision time* (§15.3's schema —
`paper_orders.features_json`) paired with their realized outcomes, exactly as continued live
learning was always meant to work — see §15.11's continuous-learning design and §15.12's Signal
Conductor for how a decision and its outcome are actually paired end to end. It pools **all active
tokens** into the one global agent's training data (token identity is an observation field, §15.1
— no per-token filtering or separate models).

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

### 15.14 Timeout force-close for stale positions (2026-08-30)

Added after the operator noticed some paper positions sitting open a long time with barely-moving
PnL — tying up an instrument's one-open-position slot (§16.9) without the position itself going
anywhere. `internal/usecase.PaperTrader.MaxOpenDuration` (config `paper_trading.rl_max_open_duration`,
default **6h**) force-closes any open position, RL-adjusted or not, once it has run past that limit,
`close_reason='timeout'`. Checked in `monitorOpenOrders` on every tick, only when the genuine SL/TP
touch check for that tick found nothing — a real touch always wins over a timeout landing on the
same tick, never the reverse. `'timeout'` was already a valid `close_reason` CHECK value since
migration `000001` but had never actually been emitted by any code path; no migration was needed.

**Whether the model was already told about this kind of close — the operator's own question:**
No — before this, a timed-out position had no `close_reason` case at all, so
`conductor.TerminalCategory` fell through to its default `""` and the trade delivered **zero
reward**, the same silent gap §15.12 found and fixed for `manual` closes. That gap is now closed,
but not by adding a fourth terminal category. The three the model has known since §15.10/§15.11 are
`closed_tp`, `closed_sl`, `closed_early` — a genuinely new `closed_timeout` category was considered
and explicitly declined for now: `SIGNAL_CATEGORIES`' one-hot is a fixed-width vocabulary shared
byte-for-byte between Go and `rl_service/obs.py`, so a new entry means bumping
`OBSERVATION_SCHEMA_VERSION` (6 → 7) on both sides simultaneously — until that ships, every
`/predict` call 422s on the version mismatch (the model stops being consulted at all), and even
once it ships the *already-trained* model's replay buffer still holds only the old, narrower
vectors — `to_vector` pads/truncates to whatever width the loaded model expects (the exact
mechanism §16.9 found), so nothing actually learns the new signal until enough fresh experience
accumulates at the new width. Given that cost against a housekeeping close that doesn't yet need
its own gradient signal, `close_reason='timeout'` instead reuses `closed_early`'s category
(`conductor.TerminalCategory` maps both `CloseReasonRLEarly` and the new `CloseReasonTimeout` to
`domain.CategoryClosedEarly`) — the model is told "this was a decision-driven exit," which is true
of both, and the trade trains something instead of nothing. The distinction still matters to a
*human* reading `paper_orders`: `close_reason` itself stays `'timeout'` vs `'rl_early'`, so the two
are never confused when reviewing trade history, even though the model sees one category for both.
Revisit toward a real fourth category once there's a concrete reason the model needs to tell them
apart (e.g. evidence it should behave differently after a timeout than after choosing to exit).

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
`internal/strategy/*.go` — reimplementing it in Python would create two copies that drift the
moment one is edited and the other is forgotten. Bayesian optimization itself (which candidate
parameter values to try next,
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
policy stays random forever. This deadlock is structural, not a bug, given §15.8's decision to
train from a fresh/random initialization with no pretraining phase. Two ways out were considered: a
bootstrap override that forces opens until N trades have closed, or leaving `rl_sizing` off so
strategies open positions and the model only manages them via the update path. The second was
chosen — it needs no new code, starts real reward flowing immediately, and matches the roadmap's
one-thing-at-a-time rollout order.

## 17. Candle backfill (implemented 2026-08-28)

A fresh database has no candle history in Postgres's `candles` hypertable. The original plan was
to run the live ingestor for several days to accumulate it — unnecessary, since OKX serves that
history directly. This turned what would have been a multi-day wait into a ~90 second job.

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
  was first specified with). A day-based depth starves exactly the timeframes that need the most
  history: "30 days" is ~30 rows on a 1D bar and ~8,600 on 5m. `DefaultTargetCandles` is 1,500:
  ~5 days of 5m, ~15 days of 15m, ~2 months of 1H, ~4 years of 1D.
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

### 16.10 The host OOM, and the exposure it uncovered (2026-08-30)

Reported as "Grafana is down." Grafana was the symptom; it was neither the cause nor the most
serious finding.

**What happened, in order.** `cmd/paper-trader` died at 07:11 on a Kafka DNS lookup timeout. With
no `restart:` policy on any service it stayed dead, so paper trading and RL reward collection
stopped silently — the panel and every other service still looked healthy. At 07:22 the kernel's
global OOM killer took Grafana, which had grown to **1.68GB RSS**. The trigger was in Grafana's own
logs: a sustained vulnerability scan from the open internet (`/.aws/credentials.copy`, `/info.php`,
`/config.json.bak`, `/serverless.yml.orig`), 595 requests from one host, with responses taking
minutes. paper-trader's Kafka timeout 11 minutes earlier was almost certainly the same memory
pressure.

**The real finding.** Every published port was on `0.0.0.0` with no firewall — including Redis with
no password and TimescaleDB with `okxbot:okxbot`, both fully reachable from the internet on a host
demonstrably being probed. Checked for compromise and found none (empty Redis with default
`dir`/`dbfilename` — the RDB-write backdoor path — no rogue cron, only the owner's SSH key, no
miner). The window was luck. Fixed per §11's network-model note above: OpenVPN plus an iptables
lockdown in `DOCKER-USER`.

**Three things this incident teaches that are worth keeping:**

1. **A documented policy is not an implemented one.** §11.2 described `restart: unless-stopped` as
   though it were in place, and §11 described VPN-only access as the network model. Neither existed
   in `docker-compose.yml`. Both read as settled decisions for months. When a design doc asserts an
   operational property, that property needs a check that would fail if it were absent — prose
   cannot be that check.
2. **Memory limits are a trading-safety control, not a monitoring nicety.** Nothing bounded any
   container, so a *monitoring-tier* service under an *external* scan was able to kill the box the
   trading loop runs on. Grafana, Kafka (whose 1G default heap left a 3.9GB host almost no
   headroom) and rl-service are now capped. Grafana's cap was raised 512m → 768m after measuring
   ~440MB idle: a limit that sits at 87% before any load just trades a host OOM for a service crash
   loop, which is not an improvement.
3. **`restart: unless-stopped` is wrong for a service that cannot possibly start.** `cmd/trader`
   exits immediately without OKX credentials, which is the normal state until the demo run (§14
   Phase 2). Restarting it forever burns CPU and floods the logs; it gets `on-failure:5`, which
   still recovers from genuine crashes once credentials exist. Applying one policy uniformly to
   every service would have quietly created a permanent crash loop.

**On verifying a firewall.** Every convenient test here was actively misleading, and each one
initially suggested a *wrong* conclusion. Probing the public IP *from the server* routes locally and
never traverses `DOCKER-USER`, so it reported all ports open after they were closed. A third-party
port-checker reported SSH closed while the SSH session issuing the request was live — which also
invalidated its "closed" verdicts for the other ports. A zero DROP counter is ambiguous between "the
rule works" and "traffic bypasses the rule." What finally settled it was probing from a dedicated
network namespace: the connection was refused *and* the DROP counter incremented on that exact
packet. When a check's failure mode is silent, prefer the test that produces a positive signal.

### 16.11 Double-reporting a trial outcome to the sidecar (found and fixed 2026-08-31)

Found while investigating a "Bad Gateway" report on the panel — a coincidence, not related: the
gateway issue was nginx caching a redeployed `api` container's old IP (§11's own operational-rule
addendum), but checking the surrounding logs surfaced a real, unrelated bug in the same window.
`optimizer-service`'s log showed `ValueError: Cannot tell a COMPLETE trial.` from
`optuna.study.tell()`, twice in 24h, both from `cmd/strategy-optimizer`'s own IP for the same
`trialId`.

**Root cause**: `internal/optimizer.Run.CheckTick` (called from the tick-Kafka-consumer goroutine
on every price tick) read a trial as open via `store.OpenForInst`, computed the SL/TP touch, then
called `store.Close` followed by `sidecar.Report` — three separate steps with no lock spanning all
of them, and `TrialStore.Close` itself unconditionally issued `DEL` with no signal for whether it
was the first caller to remove that key. Two ticks landing close together (or a tick racing the
30-second `awaitRun`/candle-close path touching the same run's state) could both observe the same
trial as still open before either had closed it, both compute the identical win/loss outcome, and
both call `sidecar.Report` for the same trial — Optuna correctly rejects `tell()` on a trial number
already marked `COMPLETE` by the first call, which is what surfaced as the 500.

**Impact was cosmetic, not correctness-affecting**: the first `Report` for a given trial always
succeeded and recorded the real outcome; the second was pure duplicate noise — no trial's score was
ever double-counted in `r.candidates` (that update happens only when `Close` reports it was the
actual closer, per the fix below) and no run's final candidate selection was affected. Root-caused
by checking which service's IP the sidecar's failing request came from (`172.18.0.13`, matching
`strategy-optimizer`, not the newer `strategy-tester` container also sharing this sidecar per
§18.2) before concluding anything about the new code — the timing coincided with the strategy-
tester optimizer loop's own deploy that same day, and confirming the actual caller ruled that out
immediately rather than chasing the wrong service's logs.

**Fix**: `TrialStore.Close` now returns `(closed bool, err error)`, using Redis `DEL`'s own reply
(count of keys actually removed) as an atomic per-key signal of whether THIS call was the one that
closed the trial. `Run.CheckTick` only updates `r.candidates`' win/loss tally and calls
`sidecar.Report` when `closed == true`; a `false` means another caller already did both, so this
one silently no-ops. 1 new test (`TestTrialStore_CloseIsIdempotent`, asserting the second `Close`
on an already-closed trial reports `closed=false`), 37 optimizer tests total. The trial-store tests
are Redis-integration tests that skip cleanly with no local Redis reachable (existing pattern,
`redisAvailable`) — verify this fix against the server's real Redis after deploying, not just the
skip-clean local run.

## 18. Independent strategy-validation service (`cmd/strategy-tester`, 2026-08-30)

A standalone paper-trading copy, requested after noticing production's single-open-position-per-
token rule (§16.9) let the fastest timeframe (5m) monopolize every token's slot — 15m/1H signals
were firing but never got a chance to open, which made it hard to tell whether a strategy was
actually bad or simply starved of the slot. This service exists to answer "does this strategy's
raw signal work" with zero interference from the RL agent's sizing/SL-TP-adjust/update mechanic,
the shared-account caps, or any other production concern — deliberately the same separation of
concerns as the strategy-optimizer's §16.1 ("keep the RL agent out of judging signal quality").

- **Storage is entirely separate** (migration `000010`): `tester_strategy_versions` and
  `tester_orders`, sharing no table, foreign key, or naming with `strategies`/`paper_orders`. A
  `tester_config` singleton row holds the panel-editable overrides (bar/notional/leverage).
- **Every registered `strategy.Factories` kind (14) trades every configured instrument** (defaults
  to the same roster as `trading.inst_ids` — the operator's explicit "don't limit to one or two
  tokens, use exactly the same tokens" instruction) at one fixed timeframe (`tester.bar`, default
  `5m` — chosen from the operator's own observation that signals/fills concentrate there in
  practice, confirmed by checking `paper_orders`: all 10 tokens' open-position slots were filled by
  5m orders, 15m/1H essentially starved). One open position per instrument across every kind,
  serialized the same way as `PaperTrader.openMu` (§16.9's exact race, guarded against here too).
- **No RL, no update mechanic, no sizing decision**: fixed `notional_usd`/`leverage` (default
  $10/10x) for every position. A signal with no stop-loss is skipped outright, same non-negotiable
  rule as production (§16.9) — this service has no clamp/`EnsureStop` fallback to fill one in.
- **Judged by realized PnL sign**, not `close_reason` — same correction just applied to
  `port.Repository.StrategyStatsFor` (§11.3). This service has no in-trade adjustment today so
  `sl`↔loss and `tp`↔win will coincide almost always in practice, but keeping one win/loss
  definition across both services means a reader never has to remember which page uses which rule.
- **Versioning is manual, not automatic**: the operator edits a kind's params from the panel, which
  inserts kind's next version number (`grid_like_v2`, etc.) rather than overwriting — the prior
  version is disabled but its historical stats stay visible, and `parent_version_id` lets the panel
  diff exactly what changed. There is no auto-versioning from a model or optimizer; this is
  deliberately a separate, simpler mechanism from `strategy_param_changes` (§16.7).
- **Config changes need a restart to take effect** (a new bar means a new Kafka subscription and a
  fresh candle-window reseed) — `POST /restart` on `cmd/strategy-tester` simply calls `os.Exit(0)`
  and relies on Docker's `restart: unless-stopped` policy to relaunch it reading the config it just
  saved. This is the ONLY viable mechanism: `cmd/api`'s own `dockerStatus`/`LogTail` already shell
  out to a `docker` binary that turned out not to be installed in its own image (`Dockerfile.api`
  never added the Docker CLI) — found while building this, a pre-existing gap left as-is per the
  operator's own "leave it for now" call. Self-exit + a container-scoped restart policy is what
  guarantees restarting the tester can never affect any other service, with no shared command path
  that could accidentally target one.
- **Panel tab is stats-only, no positions list** (operator's explicit ask): one row per version
  grouped by kind, showing signal count, open count, TP/SL close counts, win rate, and rounded
  realized PnL, plus a click-through modal comparing a version's params against its parent's.
- **Proxied through `cmd/api`** (`GET/POST/PUT /api/tester/*`) rather than exposed directly, same
  access-control posture as every other panel data source (§11) — the panel never talks to
  `cmd/strategy-tester`'s port itself, and that port is still firewalled VPN-only regardless.
- **A pre-existing migration race surfaced during first deploy, left unfixed by explicit choice**:
  `internal/postgres.Repository.Migrate`'s check-then-act loop (check if a migration filename is
  already recorded, then apply it) has no lock, so two services starting simultaneously against a
  fresh database can both see a new migration as unapplied and both attempt it — the loser's
  transaction fails on a duplicate-key constraint and the process exits, which its restart policy
  immediately recovers from (observed: `cmd/api` restarted once, `RestartCount=1`, and came up
  clean because `cmd/strategy-tester` had already finished applying migration `000010`). This can
  recur on any future migration where two migrating services first start together — a `pg_advisory_lock`
  around the whole loop would close it, deferred since self-recovery already works.

### 18.1 Removing the one-position-per-instrument limit, and its own timeout (2026-08-30)

Two follow-on changes, made after the operator observed the panel looking "frozen" — real, but
not a bug: every one of the 10 instruments' single open-position slot (inherited from production's
own rule, §16.9) was occupied, so no new signal from any of the other 13 kinds on that token could
open until the occupying one closed, sometimes hours later. Checking Kafka consumer lag (0 on both
topics), CPU usage, and the live `/api/tester/stats` response all confirmed the service was healthy
and simply blocked by its own design, not stuck.

- **The one-position-per-instrument limit is REMOVED for this service specifically** (explicit
  operator instruction — "برای استراتژی تستر نیازی نیست محدودیت یک پوزیشن برای هر توکن داشته
  باشیم"). `evaluateVersions` now opens a position for every version that fires on a candle close,
  not just the first one, gated only by **one open position per (instrument, version)** — a kind
  cannot stack a second position on the same token before its first resolves (that would double-
  count one setup as two independent trials), but every other kind is free to trade that same
  token simultaneously. Immediate effect measured live: `ZEC-USDT-SWAP` alone went from 1 to 5
  concurrent positions across different kinds in one candle close, and several kinds that had
  produced zero signals in over an hour — `pmax` among them — started firing the moment they were
  no longer locked out. This does NOT change production's own one-slot rule in
  `usecase.PaperTrader`/§16.9 at all — the operator explicitly asked that production stay untouched,
  and the two paths share no code (`cmd/strategy-tester`'s `openMu`/`evaluateVersions` is a
  separate implementation from `PaperTrader.openMu`/`evaluateStrategies`).
- **A new timeout force-close was added, mirroring §15.14's reasoning but as fully separate code
  and config**: `tester.max_open_duration` (default 6h, same starting value as production's
  `paper_trading.rl_max_open_duration`) force-closes a tester position that has run that long,
  `close_reason='timeout'` — this service has no in-trade update mechanic at all, so unlike
  production, a position here can otherwise sit open indefinitely if price never reaches either
  level. `tester.IsTimedOut` (`internal/tester/engine.go`) is a separate pure function from
  `conductor.Conductor.IsTimedOut`, deliberately not shared, matching this package's own doc
  comment that the two services must stay independent. `tester_orders.close_reason`'s CHECK
  constraint only allowed `'sl'`/`'tp'` (migration `000010`), so migration `000011` widens it —
  this service has no model to report a terminal category to, so unlike §15.14 there was no
  observation-schema question to resolve here at all.
- 2 new tests (`TestIsTimedOut`/`TestIsTimedOut_Stateless` in `internal/tester/engine_test.go`, 219
  Go total). Verified against the real deployment: within minutes, positions spread from 10 (one
  per instrument) to 25 across all 10 instruments with no instrument capped, and every other
  service's uptime was unaffected by the redeploy.

`max_open_duration` was made panel-editable the same day (quick follow-up): a new `tester_config`
column (migration `000012`, TEXT, same Go duration-string format as `config.yaml`) round-trips
through `GET/PUT /api/tester/config` and the Config section's new "Max open (hours)" field, applied
at startup by `POST /restart` exactly like `bar`/`notional_usd`/`leverage` already were. Stored as
a duration string rather than a numeric column specifically to avoid a second value representation
that would need its own parse/format path on both the Go and panel sides — the panel converts to
and from whole hours purely for a friendlier input, the wire format is unchanged. Verified live:
saved `2h`, restarted, confirmed `2h0m0s` came back from `GET /config`, then reset to the `6h`
default the same way.

### 18.2 Automatic per-kind optimization loop (2026-08-31)

The panel had a fully-wired `POST /tester/versions` route (`internal/tester.Store.CreateVersion` +
`api.createTesterVersion`) with no UI ever calling it — verified end to end via a manual `curl`
(created and immediately reverted a real `rsi_sma_fuzzy_v2`) before concluding the backend was not
the problem. That traced back to the actual ask: the operator does not want to hand-edit params at
all — the tester service itself should propose new parameter candidates, judge them by their own
performance, and build progressively better versions on its own; the operator's role is only to
later pick a version to trust from the panel, never to invent one.

**Reuses production's Optuna sidecar (`optimizer-service/`) rather than standing up a second one**
(operator's own instruction, after confirming the sidecar has no state that could leak between
callers): `optimizer_service/api.py` keys every study by an opaque, caller-chosen `study_id` string
in an in-memory dict with zero cross-key interaction, so a distinct prefix is sufficient isolation.
`internal/tester.StudyID` uses `"tester:{kind}"`, which can never collide with production's
`internal/optimizer.StudyID`'s `"{inst_id}:{kind}"` convention (verified: `"tester"` cannot appear
as a real `inst_id`, and even a hypothetical collision would need not just a string match but an
identically-keyed request from both services, which the two services' code paths cannot produce).
`internal/optimizer.SidecarClient`/`Candidate`/`SidecarParamSpec` (pure HTTP types, no DB
dependency) are reused directly rather than duplicated — this is the one and only cross-import
between the two packages, and it carries no production state across.

**One in-flight candidate per kind, not a batch** (operator's explicit "یکی یکی، ساده" over a
parallel/batched design like production's optimizer): `tester_optimizer_state` (migration `000013`)
tracks at most one candidate version id per kind. `OptimizerLoop.Tick`, run every
`tester.optimize.check_interval` (default 1h, operator's explicit "لوپ رو هر ۱ ساعت اجرا کنیم تا
ببینه کی اماده هست"), either judges that kind's in-flight candidate once it has closed
`internal/tester.MinTradesToScore` (30) trades, or — if the kind has no candidate in flight —
proposes a new one immediately.

**A new candidate is created already `enabled`**, unlike production's shadow-fork A/B mechanic
(§15.4): this service trades one version per kind live at a time by design (§18), so there is no
shadow slot to hold a candidate in while comparing — the candidate itself starts trading and
accumulating the very trades that will judge it.

**Every new candidate is built from whichever version currently scores best across the kind's
entire history, never just the most recent version** — the operator's explicit simplification of an
earlier, more complex proposal: *"هر سری که میخواد ورژن جدید بسازه یدور تمام ورژن قدیمی ها رو بخونه
نتایجشون رو و آپدیت جدید رو از روی اون بسازه"*. `proposeCandidate` calls `Store.VersionsForKind`
(new — returns every version of one kind including the origin), scores each via `VersionStatsFor`,
and picks the best with `BestScore`. This means a candidate that underperforms never poisons the
lineage: the next proposal simply reverts to basing itself on the origin or whichever earlier
version still leads, exactly the "if bad, go back to the best version, not just the last one"
behavior the operator asked for, achieved without needing to track a separate "lineage" concept at
all — the full history is re-scored fresh every time.

**Scoring combines win rate AND realized PnL, deliberately not win-rate-alone like production's
optimizer** (operator's explicit "winrate , pnl در کنار هم و در تعداد بالای ترید" — §16's
`scoring.go` intentionally excludes PnL per §16.1's RL-independence rationale, which does not apply
here since this loop has no RL agent to protect the judgment from). `VersionScore.Better` requires a
candidate to be not-worse on **both** dimensions and strictly better on **at least one** — a
candidate with a higher win rate but worse PnL (many small wins funding a few large losses) does
NOT count as an improvement, and neither does the reverse. This is a deliberately conservative
choice: an ambiguous result leaves the current best version in place rather than churning on noise.
`MinTradesToScore = 30`, doubled from production optimizer's example config of 15, per the
operator's explicit "۱۵ ترید عدد کافی ای نیست" — each version needs enough trades to make a real
comparison, and the once-an-hour cadence just checks whether that threshold has been crossed yet,
it does not itself gate how long a candidate needs to run.

**No version is ever deleted automatically — origin or otherwise** (operator's explicit "ورژن
اولیه که همیشه هست و پاک نمیشه. ورژن های جدید هم پاک نمیشن مگر توسط خودم"). The panel gained a
**Delete** button (`DELETE /api/tester/versions/{id}` → `cmd/strategy-tester`'s own
`DELETE /versions/{id}` → `Store.DeleteVersion`) for the operator's own manual cleanup — refuses to
delete the currently-enabled version (would leave the kind with nothing trading and no obvious
fallback), and clears any `parent_version_id`/`tester_optimizer_state` reference to the deleted row
first so a delete can never leave a dangling foreign key or a candidate pointer aimed at nothing.

**`tester_strategy_versions` gained `source` (`'origin' | 'manual' | 'optimizer'`) and `trial_id`**
(migration `000013`) so a person reading the panel — and the code judging a candidate — can always
tell how a version came to exist. `trial_id` is the Optuna sidecar's own trial number for a
version proposed by this loop (`NULL` for the origin and for manual panel edits), letting
`judgeCandidate` call `sidecar.Report` on exactly the trial that produced the version being judged,
closing the ask/tell loop Optuna needs to actually learn across proposals for the same kind.

**`sidecarScore` blends win rate and PnL sign into Optuna's required `[0,1]` maximize objective**
rather than reusing production's win-rate-only `Score()` — a `+0.1`/`-0.1` nudge for positive/
negative PnL, clamped to `[0,1]`, so Optuna's own TPE sampler is pushed in the same direction as
this loop's own `Better` comparison rather than optimizing a different, win-rate-only signal that
`Better` would then partly override anyway.

A kind with no tunable `strategy.ParamSpec`s (a pure crossover with nothing to search over) is
skipped by the loop rather than erroring — there is nothing a candidate could differ by.

17 new Go tests (`internal/tester/optimize_test.go`, 228 Go total): win-rate/PnL scoring from raw
stats, the `Better` comparison's not-worse-on-either-dimension rule (including both single-
dimension-worse cases that must NOT count as improvement), `BestScore`'s eligibility filtering and
its fallback-to-first behavior when nothing has enough trades yet, the `sidecar:{kind}` study-id
prefix, and `sidecarScore`'s clamping. `docker-compose.yml`'s `strategy-tester` service gained
`OPTIMIZER_SERVICE_URL` (previously unset — silently defaulting to `localhost:8001`, unreachable
from inside the container) and a `depends_on: optimizer-service`.

**Post-deploy gotcha caught the same day: nginx caches an upstream container's IP past a
redeploy.** Redeploying `api` (a `docker compose up -d api`, new container = new internal Docker
IP) left the panel's nginx still holding the OLD IP for its `/api/*` proxy, since nginx resolves a
proxied hostname to an IP once and does not re-resolve it just because the container behind that
hostname changed — this presented as the front-end showing "Bad Gateway" on every `/api/*` call
while `api` itself was completely healthy (`curl` against it directly worked fine). Nothing about
this is specific to the strategy-tester optimizer work above; it will recur on ANY future redeploy
of `api` (or any service the panel's nginx proxies to) unless the panel container is restarted in
the same breath. Fixed by `docker compose restart panel`, which makes nginx re-resolve the
hostname on its own restart. **Operational rule going forward: redeploying `api` (or `rl-service`,
or any container nginx proxies to) requires restarting `panel` immediately after**, not just the
service that changed.

## 19. Unrealized PnL% missing leverage, and a hard 15% loss cap (2026-08-31)

### 19.1 Unrealized PnL% was never multiplied by leverage

Reported as a panel display bug ("we show price-change %, not PnL%") — real, but the actual bug
was in the shared Go helper the panel's display math mirrors, not the panel alone, and it reached
the RL model's own observation input.

**Root cause**: `usecase.unrealizedPnLPct` (`rl_sltp_adjust.go`) computed
`direction*(price-entry)/entry` — a raw price-change ratio with no leverage applied at all. Every
dollar-PnL computation elsewhere in the same package (`realizedPnL`, `papertrade.go`) already
multiplies by `o.Leverage`; this one didn't. Correct only by coincidence back when every paper
order opened at the old 1x default (§14's fixed-sizing history) — silently wrong at any other
leverage, e.g. a 1% price move at 20x displayed as 1% instead of the correct 20%.

**Worse than the display symptom**: this same function feeds `PositionState.UnrealizedPnLPct` —
sent to `rl_service` as a live model input — and `PnLMaxPct`/`PnLMinPct`, both persisted to
`paper_orders` and also fed to the model. Meanwhile the live/demo path (`trade.go`) reports OKX's
own `uplRatio`, which **is** already leverage-adjusted (unrealized PnL ÷ initial margin). So the
model was trained on paper-mode observations carrying an unlevered number and served live/demo
observations carrying a levered one for the identical field — a train/serve skew on top of the
raw wrongness, the same category of bug §15.6 already fixed once for `ActiveTokens`/token equity.

Also found: `rl_service/env/replay_env.py`'s `_unrealized_pnl_pct` (the actively-used training
env) had the identical unlevered bug, while the older/secondary `okx_futures_env.py` already
computed it correctly (`* self.leverage`) — the two Python envs disagreed with each other too.

**Not affected**: the reward function itself (`replay_env.py`'s `reward = (realized_pnl -
fee_cost) / initial_equity_usd`) uses dollar PnL derived from notional, which already embeds
leverage via `target_notional` — so training was not learning from a corrupted reward, only from
a corrupted *observation* feature (which the policy could not use correctly regardless of how
good the reward signal was).

**Fix**: `unrealizedPnLPct` now multiplies by `o.Leverage` (falling back to 1x if unset/zero, same
defensive pattern as elsewhere); `replay_env.py`'s equivalent now multiplies by `self.leverage`;
the panel's `unrealizedPnL()` (`PositionsPage.tsx`) now applies `leverage` to `pct` the same way
it already did to `usd`. `trade.go`'s live path needed no change — it was already correct — but
gained a comment explaining why, so a future reader doesn't wonder why it looks different from the
paper-mode helper it must agree with. One test updated (`TestPositionStateOf_MarksForks`, 10x
fixture: `0.04` → `0.4`); `TestTrackPnLExtremes_RecordsPeakAndTrough` needed no change since its
fixture already used 1x, where the corrected formula is numerically identical to the old one.

**Known consequence, accepted rather than migrated**: existing `paper_orders.pnl_max_pct`/
`pnl_min_pct` rows stay on the old unlevered scale; only newly-written rows use the corrected one.
Since these values are direct RL model inputs, the model sees a step change in this feature's
distribution at the deploy boundary — no different in kind from any other observation-schema
change already documented (§15.3's version-bump discipline), except this one didn't get a version
bump because the *shape* of the observation didn't change, only the *meaning* of one already-
existing field. Worth watching training curves for a discontinuity right after this deploys.

### 19.2 Hard 15% realized-loss cap on stop-loss placement, independent of leverage

Explicit operator decision, same conversation: "for signals and strategies we have to limit the
SL to not going down more than 15% with any leverage. But no limit for profit." — distinct from
`conductor.Clamps.MaxSLDistPct` (§15.11/§15.12), which bounds the raw price-distance of a stop and
knows nothing about leverage. At 20x, a stop placed at the old `MaxSLDistPct` ceiling alone could
still realize far more than 15% of margin — the two clamps answer different questions and both
are needed.

**`conductor.Clamps` gained `MaxLossPct`** (`clamps.go`): `Apply` and `EnsureStop` now take a
`leverage` parameter and narrow the effective SL-distance bound to `MaxLossPct/leverage` whenever
that is tighter than `MaxSLDistPct` — implemented as `maxSLDistPctFor(leverage)`, the single place
both entry points compute the effective bound from. A non-positive leverage is treated as 1x, the
same defensive fallback as §19.1's `unrealizedPnLPct` fix. No equivalent exists for take-profit —
profit is never capped, only loss, per the operator's explicit instruction. The existing ratchet
(`usecase.RatchetSLTP`) needed no change: it only ever tightens an already-placed (now correctly
capped) stop, so it structurally cannot reintroduce more risk than the initial placement allowed.

Both of `Apply`/`EnsureStop`'s two production call sites (`lifecycle.go`'s model-driven open path,
`papertrade.go`'s strategy-driven open path) already had `leverage` in scope at the call site
before this change — sizing/leverage is decided earlier in both paths and the clamp call was
simply never given it. `internal/config`'s `PaperTrading.RLClamps` gained `MaxLossPct`
(`max_loss_pct`), defaulted to **0.15 even when left unset in config.yaml** — unlike this
struct's other fields (zero = disabled), an unbounded loss at high leverage is exactly the failure
mode this clamp exists to prevent, so it is not opt-in.

**`cmd/strategy-tester` gained the same cap**, which it had no equivalent of at all before (its
own doc comment: "this service has no clamp/EnsureStop pass to fall back on" — true for the
missing-stop fallback, but a strategy's own SL still needs the leverage-aware ceiling). Rather than
importing `conductor` (this package's own doc comment requires it stay independent of
production/`usecase` code), `internal/tester.BuildOrder` gained a `maxLossPct` parameter and a
small duplicated `clampSLForLoss` helper — the tester's existing pattern for pure math it needs
without the production coupling (see `RealizedPnL`'s identical duplication-over-import precedent).
`config.Tester.RLClamps.MaxLossPct` defaults to 0.15 the same way; the struct's other clamp fields
exist only so its shape matches `PaperTrading.RLClamps`, unused today since this service's
missing-stop case is still a hard skip, not a fill.

13 new Go tests total (5 in `conductor/clamps_test.go`, 5 in `internal/tester/engine_test.go`, plus
3 pre-existing `clamps_test.go` tests updated for the new `leverage` parameter): tightening at high
leverage, no effect at 1x, zero-leverage treated as 1x, no effect on take-profit, and the
missing-stop fallback path respecting the cap too.

### 19.3 The 15% cap missed a third SL-placing path: `cmd/strategy-optimizer` (2026-09-01)

§19.2 covered production's model-driven open path (`conductor.Clamps`) and `cmd/strategy-tester`
(`clampSLForLoss`), but there is a third place a strategy's raw `SLPct` becomes a price: the
optimizer's own trial evaluation, `internal/optimizer.signalPrices` (called from
`Run.EvaluateCandle`). It had no cap at all — a candidate parameter set proposing a wide SLPct
(Optuna's search space is exactly `strategy.ParamSpec`'s full `Min`/`Max` range, so this is not a
hypothetical) opened a trial with an uncapped stop distance.

Optimizer trials carry no leverage (`OpenTrial` has no leverage field — trials are evaluated
unleveraged, §16.3), so this needed neither `conductor.maxSLDistPctFor`'s leverage division nor a
second duplicated `clampSLForLoss` — `signalPrices` now takes `maxLossPct` directly and clamps
`SLPct` to it before computing the SL price, tightening only (never widening a tighter stop,
never touching TP — the same "profit is never capped" rule as the other two paths).
`config.Optimizer.MaxLossPct` (`max_loss_pct`, defaults to 0.15 even if unset in config.yaml,
matching `PaperTrading.RLClamps.MaxLossPct`/`Tester.RLClamps.MaxLossPct`'s identical "not opt-in"
treatment) flows through `RunConfig.MaxLossPct` into `Run.EvaluateCandle`'s call site. All three
SL-placing paths — production's model-driven open, `strategy-tester`, and now the optimizer's
trial evaluation — enforce the same 15% cap.

5 new tests in `internal/optimizer/runner_test.go` (56 optimizer tests total): buy- and sell-side
clamping, a tighter SL left unwidened, and a zero `maxLossPct` disabling the cap entirely.

## 20. Manual close button in the panel (2026-08-31)

Explicit operator request: a Close button per open position in the panel, closing at the live
price on demand.

**The real design question wasn't the button — it was what the model is told.** §15.12's original
design deliberately reported nothing to the model for a manual close ("that is an operator's
action, and reporting it would attribute a human decision to the policy"). But §15.14 had already
revised that stance once for `timeout` closes, reasoning that the model has no `closed_manual`
category to report an operator close under anyway (`SIGNAL_CATEGORIES`/`ACTION_SCHEMA_VERSION` is
a fixed one-hot vocabulary of exactly three terminal categories — `closed_tp`/`closed_sl`/
`closed_early` — shared byte-for-byte between Go and `rl_service`, so adding a fourth needs an
observation-schema bump on both sides simultaneously), and reporting nothing means that trade
trains nothing at all. Asked directly whether the *model itself* only knows three close categories
or whether `manual` already had a defined slot — it does not — the operator chose to extend
§15.14's precedent rather than adding a fourth category: a manual close now reports `closed_early`
to the model, the same "this was a decision-driven exit, true of all three" treatment as `timeout`.
`close_reason` in `paper_orders` still stays `'manual'` (a plain-text column, no CHECK-constraint
or schema-version concern, already valid since migration `000001`), so a human reading the table
can always tell an operator close from a model-driven `rl_early` or a housekeeping `timeout` apart
— only what the *model* is told collapses the three into one category, not what a human sees.

**Why the button can't just call the close path directly**: `cmd/api` (where the panel's HTTP
request lands) runs in a separate OS process from `cmd/paper-trader`, which is the only thing that
owns the instrument's live tick stream, its conductor's per-order update-cadence state, and the
single `PaperTrader.closeOrder` path that atomically closes the order, reports the terminal call,
and updates the shared account balance. `cmd/api` cannot safely do any of that itself without
duplicating that whole path in a second process. So the flow is intent, not action: a new
`paper_orders.manual_close_requested` boolean (migration `000014`), set by a new
`Repository.RequestManualClose(id)` (erroring if the order isn't currently open, so a stale/
duplicate click from an already-closed row is visible rather than silently ignored) —
`POST /api/positions/{id}/close` sets it, and `PaperTrader.monitorOpenOrders` (the same per-tick
loop that already checks every open order for an SL/TP touch) checks the flag FIRST, ahead of the
SL/TP touch and timeout checks, and closes at the live price with `close_reason='manual'` the
moment it sees it set — "the operator asked to exit now" outranks the engine's own background
decisions, including a coincidental SL/TP touch landing on the very same tick.

Paper mode only — `RequestManualClose`/the panel button only make sense against `PaperTrader`'s
own monitoring loop; demo/real positions have no equivalent operator-close path yet, so the panel
button only renders for `Mode === 'paper'` rows. `conductor.CloseReasonManual` was added alongside
the existing `CloseReasonRLEarly`/`CloseReasonTimeout` constants rather than a bare string literal,
matching that file's own pattern of a named constant per close reason with a doc comment explaining
its terminal-category mapping.

3 new Go tests (`TestMonitorOpenOrders_ClosesOnManualCloseRequest`,
`TestMonitorOpenOrders_ManualCloseTakesPriorityOverSLTPTouch`,
`TestClose_ManualCloseReportsClosedEarly` — this last one replaces the now-inverted
`TestClose_NoTerminalCallForManualClose`), plus `TestTerminalCategory` updated for the new mapping.

## 21. Known issue: `cmd/strategy-tester` disables a version's predecessor on every new version, defeating its own comparison design (found 2026-09-01, not yet fixed)

**Stopped on the server (2026-09-01)** — `strategy-tester` was one of four consumers of the
high-volume `okx.tickers` topic (alongside `paper-trader`, `strategy-optimizer`, `api`'s WS
bridge), each committing a Kafka offset after every single tick message
(`kafkastream.Consumer.Run` has no `CommitInterval` batching — a separate, pre-existing
inefficiency shared by all four, not fixed here). Restarting `strategy-optimizer` earlier the same
day to deploy §19.3's SL cap made it rejoin its consumer group at full tick volume, and combined
with `strategy-tester` already running, pushed Kafka to ~71% CPU and 88% of its 768MB memory cap
(`docker-compose.yml`'s post-§16.10 limit) with no sign of settling. `docker compose stop
strategy-tester` dropped Kafka CPU to ~6% within 30s. Nothing on the main trading pipeline
(`ingestor`, `paper-trader`, `trader`, `api`, `rl-service`, `strategy-optimizer`,
`optimizer-service` — shared with production's optimizer, §16.7, so deliberately left running —
Kafka/Postgres/Redis) was touched. `strategy-tester` stays stopped until the underlying bug below
is fixed; restarting it before then just reproduces the same load for no benefit, since its
versions aren't being validated correctly anyway.

**The design bug, separate from the load issue above**: §18.2 states new tester versions are
created already-`enabled` and that "no version is ever deleted automatically... new versions
don't get deleted either, unless the operator does it manually" — the intent being every version
stays alive and comparable. In practice `internal/tester.Store.CreateVersion`
(`internal/tester/store.go:138`) does this on every insert, operator-triggered or automatic alike:

```sql
UPDATE tester_strategy_versions SET enabled = false WHERE kind = $1 AND id <> $2
```

This disables **every other version of that kind**, not just the immediate parent — the moment a
new version exists, its predecessor (and everything before it) stops trading. `enabled` is what
gates whether a version's strategy actually opens live positions (`evaluateVersions`), so
"disabled" is functionally the same as paused, not merely hidden — the row survives (matching
§18's "never deleted" claim literally) but stops accumulating the trade history the whole
optimizer loop depends on.

This directly undermines §18.2's own scoring design: `OptimizerLoop.judgeCandidate` waits for
`MinTradesToScore` (30) closed trades before comparing a new candidate against
`proposeCandidate`'s "best version across the kind's entire history" — but the candidate's
predecessor (frequently the actual best-scoring version so far) is disabled at the instant the
candidate is created, so it can never accumulate more evidence to be compared against. The
operator's own observation matches this exactly: **new versions were never being properly
optimized/validated before going live**, because there was no live A/B window at all — one
version silences the other on creation, so `judgeCandidate` at 30 trades is scoring the new
version against stale historical stats from whenever the predecessor was last enabled, not a
fair concurrent comparison.

**Not yet fixed.** The likely direction (not yet decided/built): stop disabling siblings on
`CreateVersion`/`ForceOverride` and let every version of a kind trade concurrently — closer to
production's own shadow-fork A/B mechanic (§15.4) than to "exactly one live version per kind."
That has knock-on effects worth resolving before implementing: `evaluateVersions`' one-open-
position-per-(instrument,version) guard (§18.1) already permits multiple *kinds* to trade the
same instrument concurrently, so multiple *versions* of one kind trading concurrently may be a
small extension of the same guard — but the panel's stats view and `judgeCandidate`'s "which
version is currently the comparison baseline" logic both assume at most one enabled version per
kind today and would need to change together with the store layer, not before it.

## 22. Panel control box + stats box for paper trading, and the DOGE → XAU-USDT-SWAP swap (2026-09-01)

Explicit product request, framed as the first step toward eventually activating OKX demo trading:
a stats box (open orders, total equity, 24h/1w/1month PnL) and a config box (pause/stop, long/
short disable, active strategies/tokens/timeframes) above the Positions table.

**Scoped to paper trading, not demo, per explicit decision.** `cmd/trader` (the live/demo loop)
was investigated first and found to predate the §15.10-§15.12 signal-lifecycle redesign entirely:
no strategy signals, no `conductor.SignalConductor`, no SL/TP clamps (§19.2's 15% cap is nowhere
in its path), it never writes to `paper_orders` (so a demo position would never even appear in the
panel), and it never sends a terminal close call — meaning it generates **zero training reward**
today. Building these controls against a not-yet-real demo loop would mean re-building them again
once `cmd/trader` is actually brought up to parity with `cmd/paper-trader`, so they were built
against paper-trading (the thing actually running and training the model) with the explicit intent
that demo trading inherits the same controls later rather than getting a separate set.

**Every control is "edit + restart," not live-reload**, per explicit decision — matches
`cmd/strategy-tester`'s already-proven `GET/PUT /config` + `POST /restart` pattern (§18) rather
than teaching `cmd/paper-trader` to poll/reload mid-run. `cmd/paper-trader` was headless before
this (only `/metrics` bound) — it gained a new small HTTP surface
(`cmd/paper-trader/handlers.go`, `PAPER_TRADER_ADDR`, default `0.0.0.0:8093`) purely to receive
`POST /restart` and self-`os.Exit(0)`, the same reasoning as strategy-tester's own handler: this
binary has no Docker socket access either, so self-exit + the container's existing
`restart: unless-stopped` policy is the only mechanism that guarantees restarting paper-trader can
never affect any other service.

**New singleton table `paper_trading_config`** (migration `000015`, same shape as `tester_config`):
`trading_state` (`running`/`paused`/`stopped`), `disable_long`/`disable_short`, `active_kinds`,
`disabled_inst_ids`, `active_bars`. Read fresh from Postgres at `cmd/paper-trader`'s own startup
(`GetPaperTradingConfig`), same crash-recovery posture as `loadStrategyAssignments` — a restart
resumes with exactly the restrictions the panel last saved, not whatever was true in memory before
the process last exited.

- **`trading_state`**: `paused` and `stopped` both set a new `PaperTrader.TradingPaused` field,
  checked as the first line of `evaluateStrategies` — no new position opens, but
  `monitorOpenOrders` keeps monitoring/closing existing ones normally regardless (a config toggle
  must never orphan an open position). `stopped` additionally does a one-time
  `Repository.RequestManualCloseAll` sweep at startup — flags every open paper order via the exact
  same `manual_close_requested` column and reward-reporting path as the panel's per-order Close
  button (§20: `close_reason='manual'`, reported to the model as `closed_early`) — rather than
  inventing a new close reason or a continuous poll. A real touch or the operator's own per-order
  Close always still wins on whichever tick actually closes it; this is just "flag everything,
  then let the existing per-tick close logic do its job."
- **`disable_long` / `disable_short`**: new `PaperTrader.DisableLong`/`DisableShort` fields,
  checked in `evaluateStrategies` right after the `Hold` check — a strategy still evaluates and its
  signal is still counted in `okxbot_strategy_signals_total`, this only gates whether the disabled
  side is acted on. Existing open positions on the disabled side are left alone (same "don't
  hard-close on a macro toggle" reasoning as the token disable below).
- **`active_kinds`** (the "which strategies are active" control): NOT a new `PaperTrader` field —
  applied once at startup via a new `Repository.SetAssignmentsEnabledForKinds`, a single
  `UPDATE strategy_assignments SET enabled = (kind = ANY($1)) ...` bulk toggle, run BEFORE
  `loadStrategyAssignments` reads them. This is deliberately a NEW, coarser layer on top of the
  existing per-token/per-timeframe `strategy_assignments.enabled` (§11.3) rather than replacing
  it: one switch per strategy KIND, applied uniformly across every token (e.g. "only `grid_like`
  trades, everywhere"), which is what the operator's framing ("activate grid_like for now, more
  later") actually asked for — the existing per-assignment granularity stays the underlying
  mechanism this toggle drives, still independently editable from the Strategies page. Empty
  `active_kinds` is a no-op (no restriction), preserving today's behavior for anyone who never
  touches this control.
- **`disabled_inst_ids`** (per-token disable, e.g. a future "disable DOGE"): explicitly does
  **not** remove that instrument's `PaperTrader` goroutine or drop it from the per-instrument
  construction loop — doing so would also stop `monitorOpenOrders` for that token, orphaning any
  position already open on it. Instead a new `PaperTrader.OpensDisabled bool` field
  (`slices.Contains(disabledInstIDs, instID)`) gates only the open path, identically to
  `TradingPaused`. Ingestion (`cmd/ingestor`) is entirely untouched by this — a disabled token
  keeps collecting candles/ticks the whole time, so re-enabling it later has no data gap. The
  operator can still manually close an existing position on a disabled token via the ordinary
  per-order Close button.
- **`active_bars`**: overrides which timeframes `evaluateStrategies` actually fires decisions on
  (`paper_trading.bars`), computed once at startup as `ptCfg.ActiveBars` if non-empty else
  `cfg.PaperTrading.Bars` — deliberately does NOT touch `cfg.Ingestion.Bars`/`candleBars` (what
  `cmd/paper-trader` consumes from Kafka and persists to the `candles` table for context, §9): a
  timeframe removed from `active_bars` just stops triggering trades, it keeps being collected and
  persisted, so re-adding it later has full history waiting rather than a cold start.

**Panel** (`PositionsPage.tsx` gains two new boxes above the existing toolbar/table, both polling
at 15s — slower than the position table's own 5s poll, since this data doesn't need that
freshness): `PaperTradingStatsBox` (open count, total equity, 24h/1w/1month PnL in both % and $)
and `PaperTradingConfigBox` (state selector with a confirm-dialog on Stopped since it force-closes
positions, long/short checkboxes, timeframe checkboxes, and two "Manage…" buttons opening
`StrategyKindModal`/`TokenModal` — the "top-up window" the operator asked for, both new components
mirroring `OrderDetailModal`'s existing conditional-render-in-parent pattern). The config box's
Save/Restart split and "restart required" messaging directly reuses `StrategyTesterPage.tsx`'s own
`ConfigPanel` UX rather than inventing a new one.

**No new backend aggregate query for the stats box** — `GET /api/paper-trading/stats`
(`internal/api/paper_trading.go`) computes 24h/7d/30d PnL by calling the existing
`Repository.ListEquityHistory(mode="paper", since=now-30d, limit=0)` once (the longest window
covers all three) and folding `DeltaUSD` where `Reason="trade"` over each sub-window in Go
(`realizedPnLOverWindow`) — no new SQL aggregate needed. `GET/PUT /api/paper-trading/config` read/
write Postgres directly through `Repo` (like every other `cmd/api` handler) rather than proxying
to `cmd/paper-trader`, so they stay available even when that process is down or mid-restart; only
`POST /api/paper-trading/restart` proxies through (`internal/api/paper_trader_proxy.go`, a small
duplicate of `tester_proxy.go`'s shape rather than a generalized one — matches this codebase's
existing "duplication over coupling" precedent for small per-service proxies, e.g. §16.9's
`RealizedPnL`). New `PaperTraderBaseURL` field on `Server`, `PAPER_TRADER_SERVICE_URL` env var
(mirrors `TESTER_SERVICE_URL`'s exact pattern), `docker-compose.yml`'s `api` service gains it
pointed at `http://paper-trader:8093`.

**DOGE-USDT-SWAP → XAU-USDT-SWAP token swap**, done directly in `trading.inst_ids` config
(independent of the per-token disable feature above — that's for toggling without a config edit
going forward, not how XAU was added this one time). Verified against the real OKX API before
swapping: `XAU-USDT-SWAP` exists (`ctValCcy: XAU`, `settleCcy: USDT`, `state: live`, `lever: 100`)
and — checked against real hourly candles spanning the most recent Saturday/Sunday — trades with
**zero gap** through the weekend, identical to any crypto perpetual (it's OKX's own USDT-settled
synthetic gold product, not a traditional gold-exchange contract with market hours), so no special
weekend-handling code was needed anywhere in the ingestor/paper-trader pipeline. Backfilled 4,500
candles across all three decision timeframes (`POST /api/candles/backfill`) immediately after the
swap so it wasn't starting cold, matching §17's whole reason for existing.

5 new Go tests in `internal/api/paper_trading_test.go` (window-sum arithmetic, the zero-division
guard, exclusion of out-of-window points) plus 4 new tests in `internal/usecase/papertrade_test.go`
(`TestEvaluateStrategies_TradingPausedOpensNothing`, `OpensDisabledStopsNewOpensOnly`,
`DisableLongSkipsBuySignalsOnly`, `DisableShortSkipsSellSignalsOnly`) — 252 Go tests total.
Verified end-to-end against the real server: migration applied cleanly, the paper-trader
control-box HTTP surface answers `GET /config` with the real 10-token roster (XAU included),
`PUT /config` correctly patches only the touched field (coalesce semantics confirmed both ways),
`POST /api/paper-trading/restart` proxied through `cmd/api` and genuinely restarted only the
`paper-trader` container (confirmed via `docker compose ps` showing a fresh uptime, every other
service untouched), and `GET /api/paper-trading/stats` returned real computed numbers (10 open
orders, live equity, real 24h/7d/30d PnL) against production data.

## 23. The 15% loss cap was never actually applied to a live paper order — found via a position at 80% loss (2026-09-01)

Reported directly by the operator: order 636 (PUMP-USDT-SWAP) was sitting at **-71.5% unrealized
loss**, only 1.48% from its own stop, which itself was a 5% price-distance SL on a **20x-leverage**
position — meaning a stop touch would realize ~100% of margin, not the 15% ceiling §19.2 was
supposed to guarantee. This was correctly identified as a trading-system-severity bug, not a
one-off, and the operator asked for it to be found, fixed at every relevant layer, and for the
already-broken *open* positions to be self-corrected by the controller rather than hand-edited.

**Root cause, isolated precisely**: `conductor.Clamps` itself (the leverage-aware
`maxSLDistPctFor`/`Apply`/`EnsureStop` machinery from §19.2) was correct and already covered by 18
passing tests — this was never a math bug. The break was one specific construction site:
`cmd/paper-trader/main.go`'s inline `conductor.Clamps{...}` struct literal, built once at process
startup and assigned to every per-instrument `PaperTrader.RLClamps`, simply never included
`MaxLossPct: cfg.PaperTrading.RLClamps.MaxLossPct`. Confirmed as the ONLY such construction site
in the codebase (`grep -rn "conductor.Clamps{"` across `cmd/`/`internal/` outside tests). Without
it, `maxSLDistPctFor` silently fell back to the leverage-blind `MaxSLDistPct` alone (0.05 on the
server) — so a strategy's raw 5%-distance SL passed the open-time `EnsureStop`/`Apply` calls in
`evaluateStrategies` cleanly at any leverage, 1x or 100x. This is the same failure shape as §16.9's
`stoch_cross`-no-stop incident and §18.2's version-disable bug: a correctly-implemented, correctly-
tested safety feature that one wiring point forgot to actually use.

**Blast radius, checked immediately**: querying every currently-open paper order for
`abs(entry_px-sl_px)/entry_px*leverage` found **13 open positions violating the cap**, 5 of them at
the full ~100% ceiling (BTC, TRUMP, ETH, PUMP, ENA×2 all opened at 20x with a naive 5% stop) — not
an isolated incident.

**Fix, at the three layers the operator specifically asked to be verified**:
1. **Strategy signal output** and **2. after the model's decision** — both already funnel through
   the SAME single `EnsureStop`→`Apply` call in `evaluateStrategies` (§16.9's own documented
   reasoning: validation runs after the model's decision so it sees the levels the order will
   actually carry, not the strategy's raw proposal alone) — so fixing the one construction site
   fixes both layers at once; there was no second gap to find there. `main.go`'s inline literal was
   replaced with a new `buildRLClamps(cfg *config.Config) conductor.Clamps` function specifically
   so a future field added to either `config.PaperTrading.RLClamps` or `conductor.Clamps` can't be
   silently dropped from a large struct literal buried inside `main()` — extracting it makes the
   field list a reviewable, independently testable unit. `TestBuildRLClamps_MapsMaxLossPct` and
   `TestBuildRLClamps_ProductionScenarioIsNowCaught` (the latter reproducing order 636's exact
   numbers end-to-end through the real clamp pipeline) both mutation-verified: reverting the fix
   fails them with the production numbers (100% loss reported, "not tightened").
2. **A third, previously-nonexistent layer — self-healing already-open positions.** The clamp fix
   above only protects orders opened AFTER it deploys; the 13 already-open violators needed
   correcting without a manual close for each one, per the operator's explicit instruction. New
   `PaperTrader.tightenOverWideStop` (`internal/usecase/papertrade.go`), called from
   `monitorOpenOrders` at the top of its per-tick loop — before the manual-close/SL-TP-touch/
   timeout checks, so a stop that needed tightening is corrected before this same tick's touch
   check runs against it. Re-derives the order's SL/TP through the identical `conductorClamps().
   Apply(side, entryPx, leverage, ...)` call the open-time path uses, and persists a correction via
   the already-existing `Repository.UpdatePaperOrderSLTP` only if the result actually differs —
   `Apply`'s own `clampRange` semantics guarantee this only ever tightens, never widens, so it is a
   safe no-op on every order that was never affected (confirmed by
   `TestMonitorOpenOrders_DoesNotTightenAnAlreadySafeStop`). No new close reason, no separate
   background job — reuses the per-tick loop and per-order update path that already runs
   unconditionally for every open position. 3 new tests
   (`TestMonitorOpenOrders_TightensAnOverWideStopToMaxLossPct`,
   `DoesNotTightenAnAlreadySafeStop`, `TightenedStopAppliesOnTheSameTick` — the last confirming the
   corrected, tighter stop is what the same tick's touch-check evaluates against, not the stale one).

**Server config gap fixed too**: the server's real `config.yaml` had `min_sl_dist_pct`/
`max_sl_dist_pct`/`min_tp_sl_ratio` under `paper_trading.rl_clamps` but was missing `max_loss_pct`
entirely (unlike `config.example.yaml`, which already documented it correctly) — added
`max_loss_pct: 0.15` there too, redundant with `buildRLClamps`'s code-level fix but explicit rather
than relying solely on `config.Load`'s in-code default.

**Verified end-to-end against real production data after deploying**: all 13 previously-violating
open positions now read exactly 15.00% loss-at-leverage in Postgres — 3 had already touched their
newly-tightened stop and closed with `close_reason='sl'` at a realized loss capped at 15% of
margin (order 636 itself: entry 0.004533, corrected stop 0.0044990025 = 0.75% price distance ×
20x = exactly 15%, vs. the original 5%-distance stop that would have realized ~100%), the other 10
remain open with their stops now correctly tightened. No position needed manual intervention.

3 new tests in `cmd/paper-trader/main_test.go` + 3 new tests in `internal/usecase/papertrade_test.go`
— 257 Go tests total.

## 24. Control-box panel fixes (2026-09-01 follow-up): mode-aware, one save action, correct checkbox defaults

Operator feedback on §22's panel, addressed directly:

- **"Paper trading controls" was the wrong title** — the box is now mode-aware: a Paper/Demo/Real
  tab selector at the top (`PaperTradingConfigBox`'s new outer component), Paper showing the real
  controls (`PaperControls`, unchanged plumbing) and Demo/Real showing an explicit "not wired up
  yet" message rather than silently doing nothing — per explicit decision, only the UI became
  mode-aware this pass; there is still no demo/real controller behind it (`cmd/trader` remains
  unbuilt for this, §22).
- **A real, wrong-default bug, not just styling**: `StrategyKindModal` inverted its own stated
  semantics. `activeKinds` empty means "no restriction — every kind currently enabled applies
  as-is," but the modal rendered that as **every checkbox unchecked**, which reads as "everything
  disabled" when the true state was the opposite. Fixed to pre-check every kind when the saved
  restriction is empty; unchecking one now correctly represents creating a restriction, and
  checking everything back saves an empty list again rather than an explicit list of all 14 (so a
  future 15th strategy kind isn't silently excluded by a stale snapshot). `TokenModal`'s equivalent
  checkbox was already correct (`checked={!disabled.has(instId)}`) — only the strategy modal had
  the bug, given consistent styling in the same pass.
- **One save action, not two.** The operator's point stands on its own: nothing here has a live-
  reload path, so a "Save" that only persists to Postgres without also restarting silently does
  nothing until a separate manual step — there is no such thing as "save without restart" that
  means anything. Save and Restart are now one `Save & Apply` button (`PaperControls.saveAndApply`,
  and the strategy/token modals' own save handlers) that persists the patch then immediately calls
  `restartPaperTrader`, matching how every other write in this box already behaves once you look at
  what "save" was supposed to accomplish.
- **Pause/Stop pulled out of the config form into their own buttons**, per explicit instruction:
  they're operational actions independent of any config field, not something to bundle with
  long/short/timeframe edits that get reviewed together before applying. `Resume`/`Pause`/`Stop`
  each fire immediately (`setTradingState`), with `Stop`'s existing confirm-dialog (force-closes
  every open position) kept since that's still the one destructive action here.
- **Visual pass**: replaced ad-hoc inline styles with a small new CSS vocabulary in `App.css`
  (`.config-box-header`/`.mode-tabs`/`.state-row`/`.state-dot`/`.config-grid`/`.config-tile`/
  `.checkbox-grid`/`.checkbox-row`/`.btn-primary`/`.btn-danger`) — a status badge with a colored
  dot for running/paused/stopped, grouped tiles for direction/timeframes/strategies/tokens instead
  of one flat toolbar, and the stats box reuses the same `.config-tile` grid for visual consistency
  between the two boxes rather than two different ad-hoc layouts.

No Go changes in this pass — confirmed `PaperTrader.OpensDisabled`/`TradingPaused` already only
gate `evaluateStrategies` (new opens), never `monitorOpenOrders`, matching the operator's
instruction that disabling a token/strategy must not force-close existing positions; only the
panel's own copy needed correcting to describe this accurately (both modals already said this,
`TokenModal`'s logic already matched it, only `StrategyKindModal`'s checked-state was wrong).

## 25. Panel crashed to a blank white page on every load (2026-09-01, same-day follow-up)

Reported directly: "it shows the menu for a millisecond and then just a white page" — the exact
signature of an uncaught render error unmounting the whole React tree with nothing catching it.

**Root cause**: `GET /api/paper-trading/config`'s `activeKinds`/`disabledInstIds`/`activeBars`
fields marshal as JSON `null` when the underlying Postgres array columns are unset (the fresh
`paper_trading_config` row migration `000015` inserts has all three `NULL` until an operator ever
saves a restriction) — the exact same Go-nil-slice-becomes-JSON-null behavior CLAUDE.md's Phase 3
history (§14) already documented and fixed once before via `client.ts`'s `requestList` wrapper for
every list endpoint. `paperTradingConfig()` used the plain `request()` instead, so this one
endpoint never got that normalization. `PaperControls` then called `.length` on
`cfg.activeKinds`/`cfg.disabledInstIds` (§24's tile summary counts) — `null.length` throws
`TypeError`, and with **no error boundary anywhere in the app**, React's default behavior on an
uncaught render error is to unmount the entire tree. Since the config box renders on the Positions
page (the app's default route), this meant **every single page load crashed**, immediately after
the nav bar's first paint — matching the reported symptom exactly. This was a real, 100%-reproducible
regression from the moment §22's feature deployed, not a flake.

**Fix**: `client.ts`'s `paperTradingConfig()` now normalizes all three array fields (plus
`allInstIds`) with `?? []` before resolving, same pattern as `requestList`, so every consumer can
rely on them always being arrays.

**Also added, since this incident showed the failure mode has no visibility at all**: a top-level
`ErrorBoundary` (`panel/src/components/ErrorBoundary.tsx`, wrapping `<App />` in `main.tsx`) — the
panel had zero error boundaries anywhere before this, so any future uncaught render error anywhere
in the tree still blanks the whole page today with nothing to look at. Now it shows the error
message and a "Try again" button instead of silently vanishing. This does not change the underlying
discipline (fields from Go must still be treated as possibly-null and normalized at the API client
boundary, not caught after the fact) — it exists so the NEXT bug like this is visible and
debuggable instead of an unexplained blank page.

Verified against the real server: `GET /api/paper-trading/config` still returns `null` for the
three fields (unchanged, correct backend behavior), but the panel now loads and renders normally.

## 26. Real-money sizing decision: 10x leverage cap, $40 account, $4/token (2026-09-01)

Explicit operator request ahead of connecting a real OKX account: OKX rejects leverage above 10x
on that account, and the account is funded with $40. Four config values changed together, all
consistently — `risk.max_leverage` (100 → 10), `defaultPaperLeverage` in
`internal/usecase/papertrade.go` (20 → 10), `account.initial_usd` (100 → 40), and
`paper_trading.notional_usd` (10 → 4) — both `config.example.yaml` and `internal/config`'s in-code
fallback defaults updated together so a config.yaml that omits these fields still gets the correct
value, not the old one.

**Sizing rule, and why it's temporary by design**: $40 ÷ ~10 configured tokens = $4/token, sized so
one open position per token fits the shared account at once — a deliberate placeholder rule for
the period before `paper_trading.rl_sizing` (§14 roadmap item 5, still off) is trusted enough to
turn on and let the model decide sizing itself from `target_exposure`/`leverage_frac`. Explicitly
not meant to be hand-tuned further as a fixed rule; revisit only by turning on `rl_sizing`, not by
picking a different fixed dollar amount.

**`account.initial_usd` is shared across paper/demo/real by explicit decision** — paper training
moves to the same $40 base rather than keeping its own larger balance, so paper-trading's own
per-position sizing stays representative of what the real account can actually do once demo/real
trading is eventually wired up (§22's scoping note: `cmd/trader` is still not built out for this).
The existing live `paper` mode `account_equity` row (`initial_usd=100`, running equity ~$92 from
real paper-trading history) was deliberately left untouched — a config default only seeds a
mode's row on first creation or after a drain-to-zero reset (`GetAccountEquity`/`ApplyRealizedPnL`,
§15.6/§15.7), so the paper account keeps its own history rather than being reset just because the
config default changed; it will pick up the new $40 default the next time it happens to drain to
zero and reset, not before.

**`risk.min_liquidation_buffer_pct` raised 2 → 5** alongside the leverage change, covered in detail
in a separate exchange with the operator establishing exactly what this value means (worth
recording precisely, since it's easy to conflate with the unrelated 15% SL cap from §19.2/§23):
`usecase.trade.go` estimates distance-to-liquidation as `100/leverage` — a price-move percentage,
not a margin-loss percentage — and this floor rejects any live order whose estimated distance
falls below it. At a fixed 10x leverage this distance is always exactly 10%, so a 5% floor never
actually blocks an order at this leverage; it exists purely as an independent backstop against
total-margin liquidation (the estimate is deliberately conservative, ignoring maintenance margin),
sitting behind and independent of the 15% SL cap that should always close a position long before
this point is ever reached — same "don't trust one layer alone" reasoning as the SL-cap incident
(§23). Chose the more conservative (larger) value on the operator's explicit "pick whichever
reduces risk" instruction once the actual mechanics were confirmed.

**`cmd/strategy-tester`'s own `notional_usd`/`leverage` were deliberately left untouched** — that
service measures raw strategy signal quality independent of the real account entirely (§18), so it
has no reason to track the real account's sizing; its leverage already happened to be 10 already,
coincidentally.

2 existing tests updated (`TestEvaluateStrategies_RLSizingDisabledKeepsFixedSizing`,
`RLSizingFallsBackWhenModelErrors`) to assert against `defaultPaperLeverage` rather than a
hardcoded `"20"` literal, so a future leverage-default change doesn't silently desync the test from
the value it's supposed to verify. 257 Go tests total, all passing after the change. Deployed to
the server (`paper-trader` rebuilt and restarted) and confirmed live: the first two orders opened
after the restart (TRUMP/ZEC, 11:40 UTC) both carry exactly `size=4, leverage=10`, and a sweep of
every open position found zero still violating the 15% SL cap (§23) under the new leverage.

## 27. Real-money go-live design (2026-09-01) — decided, not yet implemented

**Forced by circumstance, not by plan**: OKX would only issue real-account API keys, not demo
keys, for this account. §14's Phase 2 "OKX demo trading dry run" item is therefore skipped, not
completed — the first live order this bot ever places will use real capital. Per the operator's
own framing: the REST/WS endpoints and request shapes are identical between demo and real trading
(only the `x-simulated-trading: 1` header and host differ, §4), so nothing about §15's design
needs to change to *support* real trading — what changes is that starting to trade at all is now
a real-money action, and several gaps that were tolerable while nothing traded for real money are
not tolerable now. This section is the design for closing those gaps. **Nothing below is
implemented yet** — this is the plan, written before code per this project's own established
practice (§16.10's own lesson: a documented policy is not an implemented one, so each item below
needs an actual verification step, not just a code change, before being marked done).

Audited current state before designing (2026-09-01): `cmd/trader`/`usecase.Trader` (the only code
path that will place real orders) predates the entire §15.10-§15.12 signal-lifecycle redesign.
Concretely, today it has **no** strategy signals, **no** `conductor.SignalConductor`, **no**
SL/TP clamps (§19.2's 15% loss cap does not exist on this path at all), **no** order-status
polling after placement (a market order's `SCode` acceptance is checked, then never followed up),
`ExchangeClient.CancelOrder` exists on the REST client but isn't even in the `port.ExchangeClient`
interface so nothing can call it, and margin mode (`TdMode`) is a single static config string sent
on every request with no read-back verification that OKX actually applied it. This is a
substantially bigger gap than "flip some config values" — real trading needs `cmd/trader` brought
to real parity with the paper-trading engine's lifecycle/conductor/clamp machinery that already
protects every paper order, which is exactly what was still open in §22's own scoping note
("`cmd/trader`... predates the §15.10-§15.12 signal-lifecycle redesign entirely").

### 27.1 Central OKX API gateway (multi-tenant rate limiting)

**Decision**: one new Go service, `cmd/okx-gateway`, sits in front of OKX's REST API. Every other
service that currently constructs its own `rest.Client` (`cmd/trader`, `cmd/paper-trader`,
`cmd/ingestor`, `cmd/strategy-tester`, `cmd/strategy-optimizer`, `cmd/api`) instead calls the
gateway over an internal HTTP (or gRPC — decide at implementation, HTTP/JSON is simpler and
matches this project's existing internal-service style, §16.2/§18) API, and the gateway is the
only process that ever holds real OKX credentials and talks to `www.okx.com` directly.

**Why centralize rather than give each service its own limiter** (the alternative the operator
was offered and explicitly rejected): five independent processes each enforcing "don't exceed N
requests/2s" locally cannot see each other's request volume, so the actual aggregate rate hitting
OKX is unbounded by any single service's own limiter — which is exactly the failure mode already
hit once (§14's `history-candles` incident, ~30 concurrent requests from one process alone
triggering a confusing `51001` error, before any other service was even in the picture). A
gateway makes the aggregate rate an enforceable, observable, single number.

**Priority**: `cmd/trader`'s requests (order placement, cancel, leverage, position/balance reads
needed for the live risk-management loop) get strict priority over every other consumer's
requests — the gateway must never let a burst of paper-trading/backfill/optimizer traffic delay a
real order or a real risk-check read. Implementation approach: **per-consumer, per-endpoint-class
token buckets**, with the trader's bucket for trade-critical endpoint classes (order/cancel/
leverage/positions/balance) refilled and drained ahead of every other consumer's queued request
when both are contending for the same underlying OKX-side budget — a priority queue keyed by
consumer identity, not a single shared bucket every consumer draws from equally. Endpoint classes
matter because OKX's own limits are per-endpoint (§27.1's own rate-limit table below) — a gateway
that only rate-limits "requests per second" in aggregate, ignoring which endpoint each request
targets, would either under-utilize headroom OKX actually gives per endpoint or blow through a
tighter endpoint-specific limit while under the aggregate number.

**What the gateway owns**:
- The real API key/secret/passphrase (moved out of every other service's config/env — they call
  the gateway with a service identity, not OKX credentials directly). This also shrinks the
  credential blast radius per §16.10's incident: fewer processes holding real secrets.
- Per-endpoint-class rate limiting against OKX's actual documented limits (§4/§27.1 below) — one
  place to get this right instead of five.
- Retry/backoff on OKX 429/`50011`-class rate-limit responses (**does not exist anywhere in the
  codebase today** — `rest.Client.do()` returns the first error immediately, no retry of any
  kind). The gateway is the natural single place to add exponential backoff with jitter, since
  centralizing retries here means the other five services don't each need their own retry logic.
- A consistent request-logging/metrics surface (`okxbot_gateway_requests_total{consumer,endpoint,
  status}`, matching the existing Prometheus convention, §11.6) — visibility into exactly who is
  consuming how much OKX-side budget, which today is impossible to see (each service's own `sem`
  of 3 concurrent requests, per the code audit below, is invisible to every other service).

**Exact OKX rate-limit numbers must be re-verified from the live docs at implementation time**
(`https://www.okx.com/docs-v5/en/#overview-rate-limits` and each endpoint's own page), not taken
from this document — third-party summaries checked while writing this section disagreed with each
other on some numbers and the official page's exact per-endpoint table did not fetch cleanly
through available tooling. Build the gateway's limits as **config values**, not hardcoded
constants, specifically so a correction after checking the real docs (or a VIP-tier fill-ratio
change per OKX's own tiered-limit system, mentioned in every source checked) doesn't need a code
change. Directionally confirmed across multiple sources: trading endpoints (place/cancel/amend
order) share one limit independent from market-data endpoints, limits are defined **per
Instrument ID** (not global) for trading endpoints, and there is a separate account-wide aggregate
cap on top of the per-instrument one — the gateway's design (per-consumer, per-endpoint-class,
with an aggregate ceiling above the per-class buckets) already matches this shape; only the exact
numbers need confirming before shipping.

**Audited current state, confirming the gap this closes**: every one of the five services builds
its own independent `rest.Client`, each with its own `maxConcurrentRequests = 3` semaphore — this
exists only to work around a specific concurrency bug against `/market/candles` (§14's incident,
documented in-code as a `51001` misbehavior under burst load, not OKX's real rate-limit response)
and is not a general-purpose, endpoint-aware OKX rate limiter at all. Nothing today coordinates
actual OKX-side rate-limit budget across processes.

**Migration order**: `cmd/trader` moves to calling the gateway FIRST (it's the priority consumer
and the smallest, newest surface — least regression risk), verified against real OKX before
anything else migrates. The other four services (`paper-trader`, `ingestor`, `strategy-tester`,
`strategy-optimizer`) migrate after, each independently, so a problem in one migration can't take
down the others — matching this project's own "incremental and verified" pattern (§14's own
closing note). `cmd/api` never needs to migrate — it doesn't call OKX directly except through
`cmd/trader`/`cmd/paper-trader`'s existing proxy pattern (§18's `tester_proxy.go`/§22's
`paper_trader_proxy.go`).

### 27.2 Margin mode: cross → isolated for real trading

**Decision**: real trading uses `isolated` margin, not `cross` (the current, currently-checked-in
default, §27's audit above — `configs/config.yaml`'s `td_mode: "cross"`). Rationale (operator's
own): isolating each position's margin means one position's liquidation cannot cascade into
draining margin backing every other open position on the account — directly relevant now that
"the account" is one shared $40 real-money pool across every token (§15.6/§26), where a cross-
margin liquidation on one token would draw down the collateral every other token's position
depends on.

**Config**: `configs/config.yaml`'s `td_mode` becomes `isolated` for the real-trading deployment;
`internal/config`'s default (currently `"cross"` if unset, `config.go:362-363`) should also flip
to `isolated` so a config omitting the field fails safe toward the more conservative mode, not the
less conservative one — matches this project's own established "unbounded loss is not opt-in"
precedent for defaults (§19.2's `MaxLossPct` default-even-if-unset treatment).

**Verification gap to close, not just the config flip**: today `Trader.TdMode`/`LeverageChange.
MgnMode`/`OrderRequest.TdMode` are sent on every request but never checked against what OKX
reports back — `domain.Position.MgnMode` is already populated from `GetPositions`'s response and
already carried in the domain type, it's simply never compared against the configured mode
anywhere. Add that comparison (log/alert loudly, and treat a mismatch as a reason to halt via
`risk.Manager`, the same circuit-breaker path `CheckDrawdown` already uses) — silently trading in
the wrong margin mode because a config value didn't take effect the way it was assumed to is
exactly the kind of "documented but not verified" gap §16.10 warns about.

**Liquidation-buffer estimate must be revisited alongside this**: `risk.Manager.Approve`'s
liquidation-buffer check uses a rough `100/leverage` approximation that ignores maintenance margin
and — per the existing in-code comment already found during the audit — bakes in an
isolated-margin-shaped assumption in its derivation "regardless of the actual configured TdMode."
Moving to isolated margin for real is the point at which that approximation's assumption finally
matches reality, but it should still be cross-checked against OKX's own reported `Position.LiqPx`
(already fetched, never consulted by `risk.Manager` today) rather than trusted as exact — same
"don't trust one layer alone" pattern as §19's SL cap plus the liquidation-buffer floor.

### 27.3 No shadow-fork in real trading — one position per token per side, model edits in place

**Decision, confirmed with the operator**: the §15.4 shadow-fork A/B mechanic (baseline vs.
`rl_adjusted` as two independently-tracked virtual positions) is **paper-trading-only** and stays
that way — it was never reachable from `cmd/trader` to begin with (confirmed by the code audit:
`usecase.Trader.execute()` has no update/adjustment pass at all, `lifecycle.go`'s fork logic lives
entirely inside `usecase.PaperTrader`/`internal/postgres`'s paper-order tables). Real trading
needs a **different**, real-money-appropriate lifecycle, not a port of the fork mechanic:

- **One open position per token per side** (i.e. at most one long and, only if `PosMode` is
  hedge-mode, at most one short — matches OKX's own net-mode-vs-hedge-mode position model,
  `PosMode`, already config-driven per §27's audit). No forking: when the model wants to adjust
  SL/TP on an open real position, it **edits that position's stop/target in place** — there is
  only ever one real order/position per token+side to edit, never a second parallel one.
  **Clarified 2026-09-03 (the real-order-lifecycle plan's §3a, after an earlier draft of that plan
  incorrectly assumed OKX conditional/algo orders): "in place" means the SAME mechanism paper
  trading already uses — our own in-process tick monitor watches SL/TP and sends a plain market
  order to flatten when touched, with a periodic (1-minute) reconciliation poll against OKX's own
  `GetPositions`/`GetBalance` to catch drift. No resting conditional order is ever placed on OKX,
  and no new `ExchangeClient` methods beyond what already exists (`PlaceOrder`/`GetPositions`/
  `GetBalance`) are needed for this. Futures/perpetual-swap (`SWAP`) endpoints only, matching every
  other exchange call in this codebase — no other instrument type is ever used.**
- **Every edit is logged as its own row, not overwritten in place at the storage layer** — the
  operator's explicit requirement: "even if a stop-loss is changed 5 times on one order, all 5
  changes must be visible," each with its own timestamp. This reuses the existing
  `paper_order_adjustments` table (migration `000016`, added 2026-09-02 alongside the same "edit
  in place, don't fork" shift for paper trading's own SL/TP mechanic, §15.4/§15.12's revision note)
  as-is — real orders live in `paper_orders` too (`mode='real'`), and that table's `source` column
  already includes `'manual'` for a future operator override (§20's paper-trading precedent). No
  new table needed for real trading specifically.
- **Panel**: clicking an order's `order-id` opens the same shape of detail modal
  `OrderDetailModal` already provides for paper orders (§14 Phase 3), extended to show the full
  adjustment history for a real order as a chronological list (timestamp → field → old → new) —
  not just the latest SL/TP, the complete edit trail. This is a real-trading-specific view; it
  does not change the existing paper-order detail modal, which has its own baseline/fork
  comparison view already (§15.4's A/B tab, `SLTPComparisonPage.tsx`) that stays exactly as-is.
- **Conflicting signal while a position is already open**: per the operator's explicit decision,
  a signal for the *opposite* side while a position is already open on that token is **ignored**
  until the model itself decides to close the existing position — it is never treated as an
  automatic flip/reversal. This matches §15.12's conductor category rules exactly as already
  designed for paper trading (a strategy firing while a position is open is an `update` category,
  never a new `buy`/`sell` — §15.12's table), so real trading's conductor usage needs no new rule
  here, only for `cmd/trader` to actually route through `conductor.SignalConductor` at all (which
  it does not do today — see §27's opening audit, and §27.5 below).

**Consequence for `cmd/trader`'s architecture**: since real trading must reuse the conductor's
category/cadence/clamp logic (not reinvent a parallel version of it) and the conductor was built
as `PaperTrader`'s IO half (`internal/usecase/lifecycle.go`), bringing `cmd/trader` to this
design is not a small patch to `trade.go` — it needs the same strategy-signal-driven,
conductor-mediated lifecycle paper-trading already has, adapted for exactly-one-real-position
instead of paper-trading's baseline+fork model. This is the real scope hiding behind "remove
forking for real trading," flagged explicitly so it isn't underestimated at implementation time.

### 27.4 Balance and margin: continuous tracking, not poll-interval snapshots

**Decision**: given real money is now at stake, balance/margin must be tracked continuously
rather than only refreshed on `Trader`'s existing fixed `PollInterval` ticker (today's only
`GetBalance`/`GetPositions` read cadence, per the audit — `step()` polls once per tick and does
nothing between ticks). Two changes:
- Shorten `PollInterval` for the real-trading deployment specifically (exact value TBD at
  implementation — balance polling has its own OKX rate-limit budget the gateway must account
  for, §27.1, so this isn't a free "poll faster" change; it competes with order/leverage calls for
  the trader's own priority-consumer budget).
- Consider a private-WS-driven balance/position push (OKX's `wss://ws.okx.com:8443/ws/v5/private`
  account/positions channels, §4 — already documented as available, not yet used anywhere in this
  codebase) as a lower-latency, rate-limit-free complement to REST polling, matching the
  event-driven-over-polling preference already established for market data (§12). This would be
  new work — no private-WS client exists in `internal/okx/ws` today (only public WS is
  implemented, per `go-engine/internal/okx/ws/public.go`) — so treat it as a stretch goal for this
  phase, not a blocker for going live, since REST polling alone is a correct (if slower) baseline.
- Regardless of cadence, every balance change must land in `account_equity_history` in real mode
  exactly as documented in §15.7/§11's `account_equity` design — this already exists
  (`cmd/trader` records demo/real equity by observing the exchange's reported equity each poll,
  §15.7) and needs no new design, just confirming it's actually wired for the `real` mode path
  once real trading starts (verify, don't assume — §16.10's lesson again).

### 27.5 Order fill timeout and cancel-and-wait-for-next-signal

**Decision, per explicit operator instruction**: real futures orders are expected to fill
immediately in the overwhelming majority of cases (market orders against a liquid perpetual), but
the code must not assume this — an order that hasn't filled within a timeout gets **canceled**,
and the bot does **not** immediately retry at a new price. It waits for the model to produce a new
signal/price on its own next cycle, rather than the engine manufacturing a retry loop with a price
it invented.

- **Timeout: 60 seconds**, per the operator's own figure, added as a new config value:
  `trading.order_fill_timeout_sec` (default 60) — `internal/config`'s `Trading` struct and
  `configs/config.yaml`/`config.example.yaml`, alongside the existing `td_mode`/`pos_mode`
  fields the audit found nearby.
- **Mechanism**: after `PlaceOrder` returns an accepted `OrdID` (`SCode == "0"`), start a
  timeout-bounded poll of that order's status. This needs a capability that **does not exist
  anywhere in the codebase today** (confirmed by the audit): no `GetOrder`/order-status-query
  method exists on `rest.Client`, `port.ExchangeClient`, or the gateway design above — only
  `PlaceOrder`'s immediate acceptance response and the next poll cycle's `GetPositions` snapshot.
  Add `GET /api/v5/trade/order` (single order status by `instId`+`ordId`) to the REST client, the
  `ExchangeClient` port, and route it through the gateway (§27.1) like every other trade-critical
  call — it belongs in the trader's priority endpoint class, not the general one, since a slow
  status check directly blocks the fill-or-cancel decision.
  - **`CancelOrder` already exists on `rest.Client` today but is unused and not part of
    `port.ExchangeClient`** (confirmed dead code by the audit) — add it to the port interface as
    part of this work; the implementation is already there, only the wiring is missing.
- **On timeout**: call `CancelOrder`, log the outcome, and stop — no synthetic retry, no
  re-pricing. The next real signal (from the strategy/conductor lifecycle once `cmd/trader` is on
  it, §27.3) is what triggers the next attempt, on its own normal cadence.
- **Futures partial fills**: per the operator's own expectation, perpetual futures market orders
  against a liquid book essentially always fill completely, unlike spot/limit order books where
  partial fills are common — so this is treated as a rare edge case to detect and handle
  correctly, not the primary design target. The order-status poll above still needs to
  distinguish `filled` from `partially_filled` from `live`/`canceled` (OKX's `state` field on the
  order-status response) rather than assuming a binary filled/not-filled — a partial fill within
  the timeout window should be treated as a real, smaller-than-intended position (update the
  domain `Order`/position state to the actual filled size, do not wait for the remainder or
  assume it will complete) rather than left ambiguous.
- **New domain gap this closes**: `domain.OrderResult` today has no status/fill-price/filled-size
  fields at all (§27's audit — it's purely the *acceptance* response: `OrdID, ClOrdID, SCode,
  SMsg`). This work needs a proper order-lifecycle domain type (a new `domain.Order` or an
  extended `OrderResult`, decide the exact shape at implementation) carrying OKX's `state`
  (`live`/`partially_filled`/`filled`/`canceled`), `avgPx`, `accFillSz` — the fields OKX's own
  order-status response already provides but nothing in this codebase currently models.

### 27.6 Order-status sync with the exchange, not self-reported state alone

**Decision, per explicit operator instruction**: real order/position state must be kept
synchronized against what OKX itself reports, not trusted from this system's own bookkeeping
alone — the same principle already applied to balance (§27.4) extended to individual orders.
Concretely:
- The order-status polling added for the fill-timeout mechanism (§27.5) is the first piece of
  this — every real order's local state is confirmed against `GET /api/v5/trade/order`, not
  assumed from the placement response.
- Beyond the fill-or-timeout window, **open real positions must be periodically reconciled**
  against `GetPositions`' authoritative response — this already happens every `PollInterval` tick
  today (`step()` already calls `GetPositions`), so the gap isn't "add position polling," it's
  that nothing today **compares** the freshly-polled position against this system's own last-
  known state and surfaces a discrepancy (size, `MgnMode`, `LiqPx` drifting from what was
  expected). Add that comparison — logged loudly, and routed through the same halt/alert path a
  margin-mode mismatch (§27.2) would use — rather than silently overwriting local state with
  whatever OKX reports and moving on, which would hide exactly the kind of drift this section
  exists to catch. **Cadence fixed at 1 minute, per explicit operator decision 2026-09-03** — a
  deliberate contrast with `RealTrader`'s own SL/TP execution, which watches the live tick feed
  in-process (§27.3's clarification) rather than waiting on this poll; the poll exists only to
  catch drift (a manual close on OKX's own UI, a liquidation, a missed fill), not to drive the
  trading loop itself, so it does not need — and should not have — sub-minute cadence.
- This reconciliation is what a WebSocket-private-channel push (§27.4's stretch goal) would also
  serve, if built — the two aren't competing designs, a push channel plus periodic REST
  reconciliation is a reasonable defense-in-depth pair (matches this project's own repeated "don't
  trust one layer alone" precedent, e.g. §19.2/§19.3's three independent SL-cap enforcement
  points), not a case for picking only one.

### 27.7 Implementation checklist (ordered; update as work progresses)

- [x] `cmd/okx-gateway`: new service, per-(consumer, endpoint-class) token-bucket rate limiting
      (`internal/gateway.Limiter`) with strict priority for the literal consumer `"trader"`,
      full-jitter exponential backoff retry on OKX 429/`50011`/`50061`
      (`internal/gateway.RetryPolicy`), Prometheus metrics endpoint. Proxies GetTicker/
      GetPositions/GetBalance/GetCandles/PlaceOrder/CancelOrder/GetOrder/SetLeverage/Health. Rate
      limit numbers in `gateway.DefaultLimits()`/`config.example.yaml`'s `gateway.*` section are
      STILL EXPLICITLY UNVERIFIED against OKX's live docs — do not trust them, re-check before
      any high-volume real trading.
  - Order-status/cancel gap this closed: `domain.OrderStatus` added; `GET /api/v5/trade/order`
    wired on `rest.Client`; `CancelOrder` (previously implemented but unreachable/dead code) and
    the new `GetOrder` both added to `port.ExchangeClient` (§27.5's domain gap).
- [x] Migrate `cmd/trader` to call the gateway first — done and verified against real OKX
      (2026-09-01): `internal/gatewayclient.Client` implements `port.ExchangeClient` over HTTP to
      the gateway; `cmd/trader` no longer holds `OKX_API_KEY`/`SECRET`/`PASSPHRASE` at all. Real
      vs. demo mode moved to a NEW single source of truth — `GET /health` on the gateway (`{ok,
      simulated}`) — since credentials moved to a different process; `cmd/trader` refuses to
      start if it can't reach the gateway. Deployed and tested live on the server: gateway
      correctly proxied a real `BTC-USDT-SWAP` ticker from OKX; `trader` correctly resolved
      `mode=demo` from the gateway and correctly refused to proceed without real credentials
      (OKX's own `code=50103`, round-tripped through the gateway intact), then stopped cleanly at
      the `on-failure:5` cap — no infinite crash loop. Found and fixed a genuine pre-existing bug
      during this verification: `trader`'s compose service never set `POSTGRES_DSN`, silently
      falling back to an unreachable `localhost` default every poll instead of recording the
      equity timeline (§15.7) — unrelated to this migration, fixed alongside it.
  - [x] `strategy-tester`/`strategy-optimizer` migrated to `gatewayclient` (their only OKX call
        was read-only `GetCandles` seeding, no gateway priority). `paper-trader` needed no gateway
        migration at all — its only OKX REST dependency was the `-backfill` flag, which was
        removed from the codebase entirely (2026-09-01, explicit repeated operator instruction:
        the history-candles endpoint must never be called again), leaving it with zero REST
        dependency. `ingestor` was never in scope — it only uses `internal/okx/ws` (public
        WebSocket), never REST. All four items resolved; nothing left to migrate.
- [x] Margin-mode read-back verification (§27.2): `Trader.step` now compares OKX's own reported
      `Position.MgnMode` (already fetched every poll via `GetPositions`, previously never
      consulted) against the configured `TdMode`, for an existing position only — halts trading
      via a new `risk.Manager.Halt(reason)` (the same halted/haltReason state `CheckDrawdown`
      trips, not a second parallel mechanism) on any mismatch, rather than silently trading
      through a margin mode that didn't actually take effect. 3 new tests (mismatch halts and the
      halt persists across steps; a match doesn't false-positive; a flat/empty position is
      correctly skipped rather than treated as a mismatch).
  - Still open: `configs/config.yaml`/`config.example.yaml`/`internal/config` default `td_mode` →
    `isolated` — DONE in `config.example.yaml` only; the server's real `config.yaml` deliberately
    LEFT AT `cross` per explicit operator decision (2026-09-01: wait until the full real-trading
    chain — the conductor rebuild below — is ready before touching live trading config).
- [x] Liquidation-buffer cross-check against OKX's own reported `LiqPx`/`MarkPx` (§27.2): `Trader.
      execute` now compares the rough `100/leverage` estimate against the real distance implied by
      the position's actual `LiqPx`/`MarkPx` (both already fetched, previously unused for this),
      and uses whichever is TIGHTER — the cross-check can only make `risk.Manager.Approve`'s check
      more conservative, never less, so a real distance wider than the estimate is deliberately
      ignored (matches the §19.2/§19.3 "never let a cross-check loosen a safety margin" pattern).
      Only applied when a position already exists — OKX has no `LiqPx` to report before one is
      open. 2 new tests: a real buffer narrower than the estimate correctly rejects an action the
      rough estimate alone would have approved; a real buffer wider than the estimate does NOT
      flip a rough-estimate rejection into an approval.
- [ ] Bring `cmd/trader` onto the strategy-signal + `conductor.SignalConductor` lifecycle (the
      real scope behind "no forking for real trading," §27.3) — one position per token per side,
      model edits SL/TP in place, opposite-side signals while a position is open are ignored
      (routed as conductor `update`, never a flip) until the model itself closes the position.
      SL/TP is watched by our own in-process tick monitor — the SAME mechanism paper trading
      already uses — plus a 1-minute reconciliation poll against OKX's own `GetPositions`/
      `GetBalance` (§27.3/§27.6, corrected 2026-09-03: an earlier draft of the linked plan said
      the opposite — real conditional/algo orders on OKX, no in-process monitoring — which the
      operator does not recall requesting and rejected on being asked directly; there is no algo
      order anywhere in this design) + a manual panel SL/TP-edit control. Futures/perpetual-swap
      (`SWAP`) endpoints only, matching every other exchange call in this codebase. Full design in
      the approved plan, `CLAUDE.md`-external at
      `/Users/rez/.claude/plans/glimmering-hopping-dahl.md` (also corrected 2026-09-03).
      IN PROGRESS — commit 1 of the plan's 11-commit rollout done: `internal/usecase/tickfeed.go`
      extracts the candle/tick IO skeleton (`decodeTick`/`decodeCandle`/`applyCandle`/
      `snapshotCandles`/`seedCandlesFromRepo`/`decisionBarFor`/`barSeconds`/`parseCandleFields`)
      as free functions `PaperTrader` now delegates to — zero behavior change (all 297 pre-existing
      tests pass unchanged, +13 new tests for the extracted functions directly, 310 total).
      Deliberately did NOT embed a shared struct into `PaperTrader`/give it new field names, per a
      design correction made while implementing: `PaperTrader`'s ~40 existing struct-literal test
      constructions and direct `pt.candles`/`pt.candlesMu` field access would have needed changing
      for a refactor the plan intended to be low-risk and mechanical — the candle-window mutex+map
      pair stays as plain fields on each of `PaperTrader`/(future) `RealTrader`, only the LOGIC
      operating on them is shared via free functions taking that state as parameters.
      Commit 2 done (2026-09-03): `sizeFromAction` split into a thin `PaperTrader` method plus a
      free function `sizeFromModelAction(cfg sizingConfig, ...)` parameterized by
      `MaxLeverage`/`MaxPositionPct`/`MaxTotalExposurePct` — the sizing math itself is now callable
      by `RealTrader` without either type embedding the other, per the plan's explicit "share the
      literal function, not just the shape" call for position-sizing math. `applyAdjustment`'s pure
      computation (level-adjust-pct → `RatchetSLTP` → "did anything actually change") extracted into
      `computeAdjustedLevels(o, action, price) (newSL, newTP *decimal.Decimal, changed bool)`,
      leaving `applyAdjustment` itself as pure IO (persist, log, record the audit row) — this is the
      seam `RealTrader`'s in-place SL/TP edit (§3a, no fork, no exchange call — corrected
      2026-09-03) will call into with the SAME IO paper trading uses (`UpdatePaperOrderSLTP` →
      `RecordPaperOrderAdjustment`), since real trading's own SL/TP is watched in-process rather
      than resting on the exchange. Zero behavior change: all 310 pre-existing tests pass
      unchanged, no new tests needed (both extractions are pure code-motion — the same logic,
      reachable through the same call sites, just also callable from outside `PaperTrader`).
      Commit 4 done (2026-09-03): `internal/usecase/realtrader.go` — the new `usecase.RealTrader`
      type, mirroring `PaperTrader`'s shape (strategy evaluation, conductor-mediated open/update/
      close, sizing, clamps) but for real orders: one open position per token per side (no fork,
      §27.3), SL/TP watched by its own in-process tick monitor (`monitorOpenPositions`, the same
      mechanism `PaperTrader.monitorOpenOrders` already uses — no exchange-side algo order, per
      §3a's correction), a real `Exchange.PlaceOrder` call on open/close, and a 1-minute
      reconciliation poll (`reconcile`/`runReconcileLoop`) comparing `GetPositions`/`GetBalance`
      against this process's own bookkeeping — closing a locally-stale position when the exchange
      already shows it flat, and routing an untracked exchange-reported position through
      `risk.Manager.Halt` rather than silently ignoring it. Reuses `sizeFromModelAction`/
      `computeAdjustedLevels` (commit 2) and `paper_order_adjustments`/`ExchangeOrderID` (commit
      3) directly — no new persistence needed. A real, load-bearing bug caught before it shipped:
      `Repository.ListOpenPaperOrders` filters by `instID` only, with no `Mode` column in its
      WHERE clause — since this deployment runs paper and (eventually) real trading against the
      SAME instrument roster simultaneously, `RealTrader` would have seen paper trading's own open
      orders as its own. Used `ListPositions(PositionFilter{Mode, InstID, Open})` instead
      throughout (`openPositions`), which correctly scopes by mode. NOT yet wired into
      `cmd/trader/main.go` (the old `Trader`/`trade.go` flat-rebalance loop still runs in
      production) — inert by construction. `fakeRepository.ListPositions` (previously an unused
      stub returning `nil, nil`) was implemented for real to support this, filtering by Mode/
      InstID/Open exactly like the Postgres version. 10 new tests (open: model open/skip/no-model,
      one-position-per-token gate; update: in-place edit with zero exchange calls; close:
      exchange-failure-doesn't-close-DB ordering, success + terminal-call delivery; reconcile:
      exchange-flat-closes-local, untracked-position-halts, matching-state-is-a-no-op) — 320 Go
      tests total.
      Commit 6 done (2026-09-03): the manual SL/TP-edit endpoint (§3b),
      `POST /api/positions/{id}/adjust`. Design correction made while implementing, catching a real
      mistake in the plan's own draft: the plan originally said to run the operator's input through
      the SAME `conductor.Clamps.Apply` the model's open-time path uses — but `Clamps.Apply` treats
      any long SL above entry (or short SL below entry) as an incoherent "wrong side" level and
      drops it outright, which is EXACTLY the case the operator asked this endpoint to support
      ("bring the stop past entry into profit," their own phrase). The model's own in-trade moves
      actually go through `RatchetSLTP` instead (a different function, needing a live market price
      this endpoint has no reason to fetch). Asked the operator directly rather than guessing;
      explicit answer: apply **no clamp at all** for a manual/admin edit — trust the human acting
      directly, the same way nothing else automated in this codebase is trusted. The percentage
      converts straight to a price via `priceFromMarginPct(entryPx, leverage, side, pct)` (signed,
      leverage-adjusted, `pct` a whole-number percentage matching the JSON field's own convention)
      with no further check. New `Repository.GetPaperOrder(ctx, id)` (port + `internal/postgres` +
      fake) — no existing method could fetch a single order by id, every prior read was list-shaped.
      Real-mode-only (400 for paper/demo), rejects an already-closed order (409), rejects an empty
      body (400). Panel: new `AdjustPositionForm` component (`panel/src/components/`), an `Adjust`
      button next to `Close` gated to `!p.ClosedAt && p.Mode === 'real'` — the reverse gate from
      `Close`, which stays paper-only — new `api.adjustPosition` client method. 15 new Go tests
      (direction/leverage math table-driven for long/short/1x/zero-leverage-fallback, the exact
      "past entry into profit" scenario, an intentionally large unclamped move, closed/paper-mode/
      empty-body rejection) — 335 Go tests total. `tsc -b && vite build` clean. Not yet checked in a
      real browser against a live real position (none exists yet) — deferred to commit 9's demo
      verification window.
      Commit 8 done (2026-09-03): `cmd/trader/main.go` now constructs `usecase.RealTrader` instead
      of the old `Trader` when the new `trading.use_conductor_lifecycle` config flag is true
      (defaults `false` — Go zero-value, no override logic touches it). `runRealTrader` mirrors
      `cmd/paper-trader/main.go`'s Kafka-dispatcher and strategy-assignment wiring almost exactly
      (own consumer-group id `"trader"` so offsets never collide with `paper-trader`'s group on the
      same `okx.tickers`/`okx.candles.<bar>` topics), reusing `PaperTrading.Bars`/`CandleLimit`/
      `RLClamps`/`RLUpdate*`/`RLEarlyClose`/`RLMaxOpenDuration` rather than a new real-trading
      config section. `buildRealTraderClamps` is a deliberate separate copy of `cmd/paper-trader`'s
      `buildRLClamps` — not a shared import — so a field dropped from one struct literal can't hide
      behind the other already being correct, the exact incident class §23 documents; both have
      their own regression test pair now. Postgres becomes REQUIRED only when the flag is on
      (`RealTrader` has no "run without a database" fallback the way the old `Trader`'s equity-
      timeline-only Postgres use does); the flag-off path's diff was confirmed minimal by direct
      review, not just by the test suite passing — log-line wording only, no behavior change. 3 new
      tests (clamp-mapping regression pair, default-off assertion) — 338 Go tests total, `go build`/
      `go vet` clean. NOT yet deployed with the flag ON anywhere — gated on commit 9's demo
      verification.
      Commit 7 done (2026-09-03): the dedicated equity-source test named in the plan's §6 — the
      implementation landed already, as part of commits 4-5, but the isolated regression test
      calling it out by name didn't exist yet. `TestBuildObservation_UsesExchangeBalanceNotRepoBookkeeping`
      seeds the repo's own "real"-mode bookkeeping row at `EquityUSD=42` and the exchange at
      `Eq=777`, then asserts `RealTrader.buildObservation`'s `AccountEquityUSD` reports 777 — a
      guard against a future refactor accidentally falling back to (or blending with) the
      bookkeeping row PaperTrader owns but RealTrader does not. 1 new test, 339 Go tests total.
- [x] New append-only real-order-adjustment log — turned out to already exist. Commit 3
      (2026-09-03) started from the plan's §4(b) design (`real_order_adjustments`, a new table)
      but found `paper_order_adjustments` (migration `000016_paper_order_adjustments`, added
      2026-09-02 when the SL/TP mechanic itself moved from shadow-forking to in-place edits, see
      §15.4/§15.12's revision note above) already IS that table byte-for-byte against the plan's
      own spec: one row per field changed, append-only, `order_id REFERENCES paper_orders(id)`
      (already mode-generic, so real rows need no separate table), and a `source` column whose
      CHECK already includes `'manual'` — exactly the value the plan's §3b manual-edit endpoint
      needs. `port.Repository.RecordPaperOrderAdjustment`/`ListPaperOrderAdjustments` are the
      methods the plan's §4(c) asked for, already implemented and tested. Nothing new was needed
      here; `RealTrader`'s update/close paths call these directly with `source="model"`/`"manual"`,
      no new plumbing.
      What genuinely didn't exist and WAS added this commit (§4(a)): `paper_orders` gained
      `exchange_order_id`/`exchange_algo_order_id` (migration
      `000017_real_order_exchange_ids`, nullable, no index — display/audit fields) and matching
      `port.PaperOrder.ExchangeOrderID`/`ExchangeAlgoOrderID` fields, wired through
      `OpenPaperOrder`'s INSERT and the `ListOpenPaperOrders`/`ListPositions` scans so a real
      order's OKX order ID is visible wherever a paper order already is — zero new query, zero
      panel change needed for basic visibility. New `Repository.SetExchangeAlgoOrderID(ctx, id,
      algoOrderID)`. `fakeRepository` extended with the new method; no other fake `Repository`
      implementation exists in the codebase. Zero behavior change to paper/demo trading — the two
      new columns are nil for every existing row and every write path except `RealTrader`'s
      (not yet built). All 310 tests pass unchanged (additive migration, no new test needed since
      nothing new is exercised yet); migration applied clean on the server.
      **Correction, same day (§3a): `ExchangeAlgoOrderID`/`exchange_algo_order_id`/
      `SetExchangeAlgoOrderID` anticipated an OKX conditional/algo-order design that was rejected
      right after this commit — real trading watches SL/TP in-process instead (same mechanism as
      paper trading), so there is no second exchange-side order to track an ID for. Left in place
      as dead/unused (nullable, harmless) rather than reverted immediately; dropping them is
      optional future cleanup, not blocking.**
- [ ] Panel: order-detail modal for real orders showing the full chronological adjustment
      history, not just latest SL/TP (§27.3) — the data (`ListPaperOrderAdjustments`) is already
      served for paper orders' own adjustment history via the existing endpoint; extending the
      modal to real orders is a small frontend-only follow-up once real orders exist to view.
- [x] `GET /api/v5/trade/order` (order status) added to `rest.Client` + `port.ExchangeClient` +
      routed through the gateway (`ClassAccount`, a read not a mutating trade action); `CancelOrder`
      added to `port.ExchangeClient` (§27.5).
- [x] `domain.OrderStatus` added, carrying OKX's `state`/`avgPx`/`accFillSz`/`sz` (§27.5).
- [x] **Fill-timeout/cancel wired into `RealTrader` (2026-09-03)**, not `cmd/trader`'s old
      `execute()` — the old flat-rebalance `Trader` never gained this and isn't going to, since
      `RealTrader` supersedes it (§27.3). `RealTrader.waitForFill` polls `Exchange.GetOrder` every
      500ms until a terminal state (`filled`/`canceled`) or `FillTimeout` elapses (config
      `trading_fill_timeout.order_fill_timeout_sec`, default 60s, wired into `cmd/trader/main.go`'s
      `RealTrader{FillTimeout: ...}` construction), whichever first — on timeout it calls
      `Exchange.CancelOrder` and returns, with NO automatic retry/re-price (per the operator's
      original decision): the next real signal on its own normal cadence is what tries again.
      Both `PlaceOrder` call sites now confirm the fill before doing anything else:
      - **Open** (`openReal`): never filled at all → canceled, nothing persisted (the token's
        open-position slot stays free for the next signal rather than being occupied by a phantom
        row). **Partial fill** → recorded as a real, smaller-than-intended position: `order.Size`
        is scaled by `AccFillSz/Sz` and `order.EntryPx` is overwritten with the confirmed `AvgPx`,
        rather than persisting the originally-requested (unfilled) size.
      - **Close** (`closeRealWith`, the flattening order): a flatten that doesn't fully fill is
        deliberately NOT treated as closed — returns an error and leaves the DB row open, since a
        partially- or un-flattened position is still real exposure on the exchange and marking it
        closed would make the system believe it's flat when it isn't. Not split into a smaller
        closed row either (unlike a partial open): the next tick's ordinary SL/TP/timeout check and
        the 1-minute reconciliation poll both already handle a reduced-size still-open position
        correctly without new bookkeeping — this only needs to not lie about the close.
      `fakeExchangeClient` (`trade_test.go`, shared by `Trader`/`RealTrader` tests) extended with a
      scriptable `GetOrder` response queue and `CancelOrder` call tracking; its default `PlaceOrder`
      result now carries a non-empty `OrdID` (previously empty, which meant `waitForFill` was never
      actually exercised by any pre-existing test — a real gap, not just an omission, since every
      `RealTrader` open/close test up to this point silently skipped the fill-confirmation path
      entirely). 4 new tests (immediate-fill fast path incl. `AvgPx` override, never-filled cancels
      and opens nothing, partial-fill records the actual scaled size, unfilled flatten does NOT
      mark closed) — 343 Go tests total, `go build`/`go vet` clean.
- [ ] Position-state reconciliation: compare each poll's `GetPositions` response against this
      system's last-known state per position, surface/alert on drift instead of silently
      overwriting (§27.6). **Folded into the `RealTrader` build (§27.3's linked plan, corrected
      2026-09-03) as a fixed 1-minute poll, rather than left fully deferred** — build alongside
      `RealTrader`'s update/close paths, not as separate later work.
- [ ] Confirm `account_equity_history` is actually being written for `real` mode once live
      trading starts — don't assume the existing `cmd/trader` equity-recording code (§15.7) was
      ever exercised against a real account before now (§27.4). Related, found 2026-09-01: the
      `trader` compose service's missing `POSTGRES_DSN` (now fixed) meant this had never actually
      run successfully even in demo mode either — the whole equity-timeline write path was
      silently failing every poll until that fix landed.
- [ ] (Stretch, not a go-live blocker) OKX private WebSocket client
      (`wss://ws.okx.com:8443/ws/v5/private`) for lower-latency balance/position/order pushes,
      complementing REST polling/reconciliation rather than replacing it (§27.4/§27.6).

**Deployment note**: per standing instruction, connect to the server via the `okx` SSH host and
transfer files with `scp`; keep the server in sync with every change made here, and commit each
implementation step in its own git commit as the work lands (matching this project's existing
one-focused-commit-per-step convention, §6) rather than bundling §27's items into one commit.

## 28. Kafka consumer goroutines died permanently on any transient error, not just real failures (found and fixed 2026-09-01)

Reported directly: after a service restart, the panel's live price stream (positions page) never
connects/shows a price again. Traced to `internal/kafkastream.Consumer.Run` — used by every
service's Kafka reading in this codebase (`cmd/api`'s two WS-bridge consumers, `cmd/paper-trader`'s
tick/candle dispatchers, `cmd/strategy-tester`'s, `cmd/strategy-optimizer`'s) — which returned
immediately on **any** error from `FetchMessage`/`CommitMessages` other than context cancellation.
Every call site's own wiring is `go func() { err := consumer.Run(ctx, handler); if err != nil {
logger.Error(...) }}()` — the goroutine just logs the error and exits. The outer process keeps
running and looks completely healthy (HTTP still answers, WebSocket clients still connect
successfully) while that one data path is silently, permanently dead — no crash, no restart, no
visible symptom except "this specific thing stopped updating."

This is exactly what happened live: building three services concurrently on the server (§16.10's
"don't build multiple Go services at once on this box" lesson relearned the hard way, see the
session log) OOM-pressured Kafka into an unclean shutdown and restart. `cmd/api`'s price-ticks
consumer hit a transient fetch error during that window, `Run` returned, the goroutine exited, and
`cmd/api` itself never noticed or recovered — it kept serving the panel's WebSocket connection
perfectly, just with nothing ever broadcasting a `type:"price"` event on it again, for the rest of
that process's lifetime. Only restarting `cmd/api` itself would have fixed it, which is not
obvious from any visible symptom ("the panel's WS connects fine, it's just quiet").

**Fix**: `Consumer.Run` now retries transient fetch/commit errors with exponential backoff (1s
doubling to a 30s cap, same shape as the existing OKX WS reconnect backoff,
`internal/okx/ws/public.go`) instead of returning. Only two things actually stop the loop now
(`classifyRunError`): `ctx` being done, or `io.EOF` — kafka-go's own signal that `Close()` was
called on the reader, confirmed against its source rather than assumed. Every other error is
treated as transient and retried forever, on the reasoning that a Kafka consumer's job is to keep
consuming for the life of the process; there is no error a live trading/data pipeline should give
up on silently.

**A real correctness gap surfaced while building the commit-retry path, not just the fetch-retry
path**: the natural-looking fix — on a commit failure, back off and `continue` the outer loop — is
wrong, because `kafka.Reader.FetchMessage` always returns the *next* message from its internal
channel regardless of whether the previous one was ever committed (confirmed against kafka-go's
own source, not assumed). Looping back to `FetchMessage` on a commit failure would silently
abandon that specific commit retry and move on to a different message instead — the retry would
appear to work (no crash, message flow continues) while quietly never actually retrying the thing
that failed. Fixed by giving the commit path its **own** inner retry loop that calls
`CommitMessages` again for the *same* message, never re-fetching until that exact commit succeeds.

`internal/kafkastream.kafkaReader` — a new interface narrowing `*kafka.Reader` to exactly
`FetchMessage`/`CommitMessages`/`Close` — was added so this retry/backoff decision logic is
unit-tested against a fake that can inject a scripted sequence of transient failures, a `Close()`-
shaped `io.EOF`, and context cancellation on demand, none of which are practical to provoke
reliably against a real broker in a unit test. 11 new tests (289 Go total), including a
regression test proving the exact bug (`TestRun_RetriesTransientFetchErrorThenSucceeds`) and one
proving the commit-retry-must-not-re-fetch correctness fix
(`TestRun_RetriesTransientCommitErrorThenSucceeds`, synchronizing its assertion from inside the
fake's `onCommitSuccess` callback — called synchronously before `Run`'s goroutine can loop around
to a second fetch — after an earlier version of the test flaked by checking from the test
goroutine instead, racing that exact next iteration).

**Known gap, not fixed here**: there is still no metric/alert for "this Kafka consumer goroutine
has exited" — the fix means transient errors can no longer kill a consumer, but a `Close()`-shaped
`io.EOF` or an unexpected `ctx` cancellation still legitimately stops `Run`, and nothing currently
surfaces that as anything other than one log line. Matches §16.10's own "a documented property
needs a check that would fail if it were absent" lesson — worth a `okxbot_kafka_consumer_up{topic,
group}` gauge (set to 0 in the `Run` goroutine's own exit path) at some point, not done as part of
this fix since the actual reported bug (silent permanent death from a transient error) is now
structurally impossible rather than just monitored-for.

## 29. Ratchet simplified: no per-step size cap, TP moves freely except across entry (2026-09-04)

Prompted by the operator investigating a real trade (order 1812, ENA-USDT-SWAP) whose in-trade
SL/TP adjustments were all clustered in the last ~19 seconds of an 85-minute-old position, right
before it closed — not spread across the position's life the way a healthy adjustment stream
should look. Auditing eight recent orders confirmed this was systematic, not a one-off: in every
one, the model's first *successful* adjustment landed seconds to minutes before close, never in
the middle of the trade. Root cause was `usecase.RatchetSLTP`'s two independent constraints
compounding: the ±2% per-step size cap (`MaxSLTPAdjustPct`, §15.4's original design) meant an
untrained/still-noisy policy's small, mostly-rejected proposals rarely moved SL far enough to pass
the one-way tightening check, so *visible* adjustments only accumulated once price had moved far
enough, which tends to be late in a trade's life. The operator did not recall specifying the 2%
figure and, on review, asked for it to be removed outright — Claude's own choice during §15's
original design, not a value the operator provided.

**Decision**: `MaxSLTPAdjustPct` and the size clamp it enforced are **removed entirely** — the
model's proposed SL/TP move applies at whatever magnitude it outputs, in one step, with no
per-step ceiling. The SL side's core safety property is unchanged and explicitly kept: **SL is
still a one-way ratchet** (`ratchetSL`, untouched) — it can only move to reduce risk (toward
locking in profit for a long/short), never loosen past its current position or undo a prior
tightening. That check is the one the operator explicitly said to keep ("ریسک بزرگ شدن sl رو چک
کنیم خیلی خوبه بزار باشه").

**TP changed more fundamentally, not just losing its size cap**: the old `ratchetTP` only allowed
TP to move *closer* to the current price (a "lock in a nearer target" ratchet, mirroring the SL
side's one-way logic) and additionally rejected any proposal that would push TP past the live
price. Per the operator's explicit instruction — a TP moving *further away* (a bigger profit
target) is not something to guard against, it's exactly the behavior worth letting the model
express if it can identify a trade worth letting run — `moveTP` (renamed from `ratchetTP`) now
applies the model's proposed TP move in **either direction**, by any magnitude, with the single
remaining guard being the one this function's predecessor was originally built to fix (2026-08-29,
orders 100/110): **TP may never cross the position's entry price**, in either direction, since
touching a TP on the wrong side of entry would realize a loss rather than a profit — that would
not be a take-profit at all. Guarding only against the live price was already known to be
insufficient for this (once price has moved against the position, "toward price" can span the
entire region past entry); guarding against entry directly is unaffected by price movement.

**Consequence for existing open positions and training data**: no migration or backfill — this
takes effect only for adjustments computed after the deploy. The change plausibly explains (not
just describes) the order-1812 pattern: with the cap gone, the model's proposals should now be
visible earlier and more often across a position's life rather than clustering right before close,
since a proposal doesn't need to accumulate several ~2%-capped steps to become large enough to
pass the tightening check. Whether that's actually what happens is worth checking against fresh
trades once several have gone through this path.

11 tests in `internal/usecase/sltp_ratchet_test.go` updated/added for the new semantics (both
sides' TP now moving in either direction, two large-single-step tests replacing the old
clamped-to-2% one, TP crossing the live price allowed, TP-crosses-entry-still-rejected re-derived
with proposals that actually exercise the guard under the new sign convention) — 344 Go tests
total. `internal/api/server.go`'s `handleAdjustPosition` doc comment (the manual SL/TP-edit
endpoint, §27's plan §3b) updated to stop citing the removed per-step cap as a reason it stays
unclamped — that endpoint was always unclamped for a different, still-valid reason (a human
operator acting directly is trusted, full stop), so its behavior didn't change here, only the
comment's accuracy.

## 30. Twelve new scalp/ICT/price-action strategies added (2026-09-03)

Explicit operator request: existing strategies were producing too many SL-closed positions with
weak profits, and most were built for swing-style setups rather than the 5m scalping the operator
actually wants to run. Rather than tune the existing 14 (that work is intentionally deferred —
`cmd/strategy-tester`'s optimizer loop is known-broken per §21 and the operator explicitly declined
to touch it this round, judging its auto-generated parameter changes as low-quality and not
grounded in real strategy knowledge), added 12 new well-known strategies as plain `strategy.Kind`
registrations in `strategy.Factories` — pure additions, zero risk to the existing 14 (§9's registry
pattern is exactly built for this). Optimization/review cadence for these, per the operator: manual
only ("هروقت خودت بگی") — no scheduled job, no automatic re-tuning.

All 12 live in `go-engine/internal/strategy/`, follow every existing convention (`ParamSpec`/
`WithParams`/`resetState()` beside stateful fields, structural price levels via `EntryPx`/`SLPx`/
`TPPx` where the strategy genuinely computes one per §16.8's audit, percentage-only where it
doesn't). They are nominally covered by the existing `TestWithParams_DoesNotCarryAccumulatedState`
(which iterates every `Factories` entry automatically, §16.8's own regression test) — but **that
coverage turned out to be vacuous for several of them**, which is what let three real bugs through;
see §30.1, and prefer the dedicated `scalp_test.go` suites as the actual guarantee. New indicator
helpers added to `indicators.go`: `StdDev`,
`BollingerBands`, `SessionVWAP` (rolling, not calendar-session-anchored — this codebase's candle
windows aren't segmented by exchange session boundaries), `KeltnerChannel`, `AvgVolume`.

- **`vwap_reversion`** — fade a price extension away from a rolling VWAP back toward it, sized in
  ATR units rather than a fixed percentage so the trigger distance scales with the instrument's own
  volatility. Classic intraday mean-reversion scalp.
- **`bb_squeeze_breakout`** — Bollinger Band width contracting to a multi-bar low (volatility
  compression) followed by a close outside the bands trades the breakout at the start of the move
  rather than mid-run.
- **`range_breakout`** — Donchian-style rolling N-candle high/low breakout (the same family as
  opening-range-breakout scalps, generalized off any fixed session). Structural stop at the
  opposite side of the broken range.
- **`keltner_trend_scalp`** — pullback-in-trend scalp: a short EMA trend filter plus entries on a
  pullback to the Keltner Channel midline, stop at the channel's own far band (volatility-scaled
  structural level). Faster/tighter cousin of the existing `dual_ma_atr`.
- **`ict_fvg`** — ICT/Smart-Money Fair Value Gap: a 3-candle imbalance (candle 1's high below
  candle 3's low, or the mirror) that price later trades back into is taken as a continuation entry
  in the gap's original direction, stop at the gap's far edge.
- **`ict_order_block`** — ICT order block: the last opposite-direction candle immediately before an
  ATR-scaled impulse move marks a zone; price returning to that zone once is taken as continuation
  in the impulse's direction, stop at the block's own range.
- **`ict_liquidity_sweep`** — ICT stop-hunt/liquidity-sweep reversal: a wick beyond a recent swing
  high/low that closes back inside the prior range on the same candle signals the breakout was a
  sweep, not genuine continuation; enters the reversal, stop beyond the sweep's own wick. Resolves
  within one candle, well matched to 5m.
- **`engulfing_reversal`** — classic candlestick engulfing pattern (a full-body engulf of the prior
  candle, opposite a short EMA trend) as a two-candle reversal, stop beyond the engulfing candle's
  own extreme.
- **`inside_bar_breakout`** — classic price-action inside-bar consolidation breakout: trade the
  break of an inside bar's own high/low, stop at its opposite extreme.
- **`macd_momentum`** — MACD histogram zero-line cross, one of the most widely used momentum-shift
  scalp signals. Percentage SL/TP (MACD has no structural price level of its own).
- **`volume_breakout`** — a range breakout confirmed by a volume surge (current candle's volume
  well above its own recent average) — the standard filter against low-volume breakout fakeouts.
- **`ema_ribbon_pullback`** — three-EMA ribbon (fast/mid/slow stacked in trend order) with entries
  on a pullback to the middle EMA that closes back in the trend direction — a well-known scalp/
  day-trading continuation system, buying dips in an uptrend rather than chasing highs.

### 30.1 Code review of the initial commit found three real bugs (2026-09-03, same day)

The first version of the above (commit `4e27430`) was reviewed before being deployed anywhere, and
three defects were found and fixed in `internal/strategy/scalp_test.go` + the three strategy files.
All three were invisible to the test suite as it stood, which is the part worth keeping.

**The review's own starting point was a false claim.** The commit asserted the new strategies were
"covered by the existing `TestWithParams_DoesNotCarryAccumulatedState`". They were not, in any
meaningful sense: that test drives every `Factories` kind over the package's `oscillating()`
fixture, which builds candles with `Open == Close` (zero-height bodies), constant `Volume`, and no
`Timestamp`. Every strategy keying off candle body direction or a volume surge returns `Hold` on
all 400 of those candles, so the warmed-vs-fresh comparison compared `Hold` against `Hold` from
end to end and reported PASS while exercising none of the state it exists to check. Same root
cause as §16.8's `pmax` incident — a registered, assignable strategy can be completely inert and
every aggregate metric still looks normal.

1. **`ict_fvg` and `ict_order_block` re-armed a setup they had already traded.** Both rescan the
   trailing window on every call, and state was keyed only on "is something armed" with no
   identity — so re-detecting the same 3-candle triple (or the same impulse/order-block pair) on
   the next candle silently re-armed a setup the previous call had just consumed. Measured on the
   real code: **one gap produced 10 entry signals across 10 candles**, and one order block produced
   8. In production each of those is a separate paper order on the same setup.
   - Fixed by identifying a setup by the **timestamp of the candle that formed it** and refusing to
     arm anything not strictly newer than both the currently-tracked setup and the last one handled.
   - A timestamp rather than a slice index specifically because **the caller's candle window
     slides** — `PaperTrader` trims to `CandleWindow` (`applyCandle`), so index 5 means a different
     bar once the window fills, and index-based identity would have started matching the wrong
     candle instead of failing visibly. The first version of this fix used indices and was replaced
     after checking how the window actually behaves rather than assuming it only grows.
   - Consequence worth knowing when writing fixtures: these two strategies now need a real
     `Timestamp` to fire at all. A timestamp-less fixture makes every candle compare as
     "not newer than nothing handled", so they return `Hold` forever — the safe direction to fail,
     but it reads as a broken strategy rather than a broken fixture, which is why the new test
     helpers set it centrally and say so.
2. **`ict_fvg` entered on the very candle that created the gap.** The zone-return test was
   `last.Low <= gapHigh`, where on the forming candle `last` IS c3 and `gapHigh` IS `c3.Low` — so
   the comparison was trivially true and it entered at the top of the impulse, the worst price in
   the setup, rather than on the pullback the strategy's own doc comment describes. Fixed by
   requiring the evaluated candle to be strictly after the one that formed the setup (applied to
   `ict_order_block` too, where the impulse candle necessarily overlaps its own block's range).
3. **`macd_momentum` compared against a histogram value carried in struct state.** That is only the
   previous *candle's* value if `Evaluate` is called exactly once per closed candle, which the
   engine does not guarantee — a restart reseeds the window (§14) and re-evaluating the same window
   compared a value against itself. Confirmed: the same window evaluated twice returned `buy` then
   `""`. Both values now come from the computed series, so the signal depends only on the candles
   passed in, and the strategy became stateless (its `resetState` was removed rather than left as a
   no-op).

**Two suspicions the review checked and dismissed rather than "fixing".** MACD's index arithmetic
looked wrong on inspection but is correct (verified: first non-zero histogram at exactly
`slowLen+signalLen-2`, last at `n-1`). And the per-evaluation cost, while real, is not a production
risk — strategy evaluation is candle-close driven (`papertrade.go`'s `handleCandle`), not per-tick.
It was still worth reducing: `ict_order_block` recomputed ATR from scratch inside its scan loop
(O(lookback × window)), now hoisted to one ATR for the whole scan — **634µs → 40µs** per evaluation
on a 300-candle window, ~16x. `macd_momentum` went 1651µs → 887µs by building the series once
instead of twice; it remains the most expensive strategy in the package because three EMA series
over the window is inherent to MACD, and that was left alone rather than over-engineered.

**Tests: `internal/strategy/scalp_test.go`** (new, 405 Go tests total, was 356). Three suites over
all 12 kinds — they fire at all on realistic data, every emitted signal is a coherent trade (stop on
the losing side of entry, target on the winning side), and the `WithParams` state-isolation contract
holds — each **asserting the comparison was non-vacuous**, so a strategy that silently stops firing
fails the test rather than passing it trivially. Plus targeted tests per bug above. Fixtures are
timestamped and have real bodies/varying volume, and drive strategies **one closed candle at a
time** via `driveCandleByCandle`, the way the engine does; an early version of these tests passed a
whole sequence in one call and mis-reported a correct `inside_bar_breakout` as broken.

**All fixes are mutation-checked**: reverting each one individually fails the test written for it
(the re-arm guard removal reproduces the original 10-entries-from-one-gap exactly; the forming-
candle guard removal fails 3 tests; restoring MACD's state-carried previous fails the determinism
test). A test that passes against both the fixed and broken code proves nothing, and several of the
tests in this package's history were written before that was verified.

**Not yet assigned to any token/timeframe** — these are registered kinds only, per §11.3's model
(a strategy exists as a locked origin row the moment `strategy.SeedOrigins` runs, but doesn't affect
any live trading until a sub-strategy is cloned from it and given a `strategy_assignments` row for a
specific token+bar). Assigning several of these to the 5m bar across the current token roster, and
judging them from real paper-trading closes, is the natural next step — deferred here since it's an
operational/assignment action (via the panel or `POST /api/strategies`), not a code change.

## 31. A stale local `config.yaml` overwrote the server's real one during deployment (2026-09-04)

Found via a real, wrong-sized position: order 1851 (ZEC-USDT-SWAP) opened at `size=100` instead of
the configured `4` (§26). Root cause was a deployment mistake, not application code: deploying the
12-strategy work (§30) was done by `tar czf go-engine/` on the local machine and extracting it
directly over `/opt/okxBot/go-engine/` on the server. `go-engine/configs/config.yaml` is real,
server-specific, and gitignored (§6) — but it still physically existed in the local working tree (a
leftover Aug 25 dev-setup file, never the real deployed config), so the tar archive included it
uncritically, and extracting the archive overwrote the server's real config with that stale local
copy. The pre-deploy backup that would have made this trivially reversible was written to `/tmp` on
the server and lost when the server was reset after an unrelated incident (§31's own build-crash,
below) — recoverable values had to be reconstructed from this document instead of restored from a
backup, which is the reason this incident is documented in this much detail: it is now the only
record of what the correct values are.

**Blast radius was narrower than it first looked**, because several of the fields the broken config
was missing have safe non-zero defaults in `internal/config.Load` that a merely-absent field falls
back to — the actual damage was confined to fields the broken file set **explicitly** to a stale
value, which a default can't override:
- `paper_trading.notional_usd: 100` (stale) vs. the correct `4` and `Load`'s own default of `4`
  (§26) — this is what actually produced order 1851's oversized position. Every other field with a
  hardcoded, non-zero-forcing default (`risk.max_leverage`, `risk.min_liquidation_buffer_pct`,
  `account.*`, `paper_trading.rl_clamps.max_loss_pct`) was either simply absent from the broken
  file (and therefore fine) or set to a value that was more conservative than intended, never less
  — `max_leverage: 5` and `min_liquidation_buffer_pct: 15` in the broken file are both tighter than
  the documented `10`/`5`, and neither actually affected paper-order leverage anyway, since paper
  trading's fixed-sizing fallback uses a hardcoded Go constant (`defaultPaperLeverage`,
  `papertrade.go`) independent of `risk.max_leverage` while `rl_sizing` is off.
- `paper_trading.rl_sltp_adjust` has no such default — a `bool` field silently reads `false` when
  absent, with nothing in `Load` correcting it the way `MaxLossPct` is (§19.2's "not opt-in" design
  was deliberately applied to that one field, not this one). This one was genuinely dangerous in a
  quieter way than the sizing bug: **the RL model stopped adjusting any open paper order's SL/TP
  the moment the broken config deployed**, with no error, crash, or visible symptom anywhere — the
  service looked completely healthy the whole time it was silently not doing part of its job.
- `ingestion.bars`/`paper_trading.bars` in the broken file (`["1m","3m","5m","1H","4H","1D"]` /
  `["1m"]`) were not just stale from this incident — they predate the 2026-08-28 decision (§9) to
  standardize on `5m/15m/1H` as the decision set entirely, and the ingestion list didn't even
  contain `15m`, which the corrected `paper_trading.bars` needs — `Config.ValidatePaperTradingBars`
  (paper_trading.bars must be a subset of ingestion.bars) caught this immediately at startup once
  both were corrected together, rather than letting a bar with no matching ingestion topic silently
  produce an empty candle window.
- **The DB-backed `paper_trading_config.active_bars` (`["5m"]`, the panel's own, currently-tighter
  restriction to 5m-only, §22) was never touched by any of this** — it lives in Postgres, not
  `config.yaml`, and `cmd/paper-trader/main.go` already applies it as an override on top of
  `config.yaml`'s `paper_trading.bars` whenever it's non-empty. Restoring `paper_trading.bars` to
  `["5m","15m","1H"]` only restores the ceiling of what the process is CAPABLE of deciding on; the
  actual effective decision cadence stayed exactly 5m throughout, both during the incident and
  after the fix — confirmed by reading `GET /api/paper-trading/config` before and after.

**Recovery**: no copy of the correct values survived anywhere retrievable — checked the lost `/tmp`
backup (gone), dangling Docker image layers (none matched; the only stale layers found on the box
belonged to `rl-service`'s unrelated config, not go-engine's), and Loki (no service logs its loaded
config at startup, so there was nothing to grep for). The corrected file was reconstructed field by
field from this document's own dated decisions (primarily §9, §14, §19.2, §22, §26) and validated
against `internal/config.Load` directly (a throwaway test loading the reconstructed YAML and
asserting every field resolved to its documented value) before being deployed — not just eyeballed,
since a config mistake here would be the same class of silent, hard-to-notice failure that caused
this incident in the first place.

**Fix, both the immediate one and the recurrence guard**: the corrected `config.yaml` was deployed
and `paper-trader` rebuilt+restarted (config is baked into the image at build time, not volume-
mounted, so a plain container restart alone would not have picked it up) — confirmed live within
seconds by the model adjusting order 1851's own SL/TP again (`lifecycle: sl/tp adjustment applied`
in the logs), which could only happen with `rl_sltp_adjust` genuinely back on. The stale local
`go-engine/configs/config.yaml` that caused this was deleted from the local working tree entirely
— it served no purpose (the example file is what a fresh checkout should reference) and its mere
presence on disk, despite being gitignored, was sufficient to end up inside a naive `tar czf`. Going
forward, deploying go-engine changes must not archive the whole local directory tree uncritically —
either transfer only the specific changed files, or explicitly exclude `configs/config.yaml` the
same way `panel/node_modules` and `panel/dist` are already excluded from panel deploys.

Separately, the same deployment attempt first triggered a genuine resource-exhaustion incident
(load average 23.6, ~69Mi free with no swap, SSH connections dropped/refused) by rebuilding all 5
Go services plus the panel **concurrently** — the exact failure mode §16.10 already documented once
for an external scan, this time self-inflicted by the deploy process itself. The server had to be
reset by the operator to recover. Every rebuild after that reset was done one service at a time,
with `docker builder prune -f` between each — confirmed to keep load under 2 and reclaimable build
cache at 0 throughout six sequential single-service builds, versus the crash from building them
together. This is now the required deployment pattern for this box, not merely a preference: it has
caused a full outage once already (§16.10) and a second, self-inflicted one here.

### 31.1 Re-enabling `rl_sltp_adjust` immediately exposed an unrelated, unbounded-TP bug

Restoring `rl_sltp_adjust: true` (the actual fix above) surfaced a second, independent defect within
seconds: order 1851's TP was adjusted 19 times in 6 minutes, each step roughly **doubling** the
previous move's magnitude, from `936.84` down through zero to `-566552.46` — for a `ZEC-USDT-SWAP`
sell entered at `955.96`. Order 1852 (BTC) diverged the same way in parallel, reaching `-670026.57`
against an `81495.9` entry. Both are nonsense prices no real take-profit could ever be.

**Root cause: `computeAdjustedLevels` (`rl_sltp_adjust.go`) converts the model's absolute
`action.TPPx` into a relative adjustment (`levelAdjustPct`) before handing it to `RatchetSLTP`,
which reapplies it as a delta** — mathematically a round trip back to `action.TPPx` with no
information lost, so the conversion itself isn't the bug. The bug is what §29 removed: **`moveTP`
(§29, 2026-09-04) dropped the per-step size cap on TP entirely**, on the reasoning that letting a
winning trade's target run further away is desirable behavior, not a defect to guard against — a
reasonable call for a *trained* policy, but this model has completed only 4 trades against SAC's own
`learning_starts=100` threshold (confirmed via `GET /health` on rl-service, `completed_trades: 4`),
i.e. it is still effectively producing near-random output, and `moveTP`'s only remaining guard is
that the result stays on the profitable side of entry — a check an arbitrarily large magnitude still
satisfies. Nothing bounds how large the move itself can be, and at a ~2-30 second re-adjustment
cadence (`RLAdjustInterval`, §15.9) a model whose output keeps drifting further from entry each call
compounds without limit. This is a real gap in §29's own design, not a coding mistake in the sense
of a wrong formula — the code does exactly what that section asked for.

**Immediate action**: `rl_sltp_adjust` was turned back OFF (reverted in `config.yaml`,
`paper-trader` rebuilt+restarted a second time) the moment this was found — confirmed no other
currently-open position was touched (`paper_order_adjustments` has zero rows for any order besides
1851/1852 since the restart that re-enabled it). This is a **known-broken, currently-disabled**
feature again, not merely reverted-by-accident-back-to-off — turning it on again requires fixing
the missing TP-magnitude bound first, not just retrying.

**Order 1851 and 1852's disposition**: 1851 closed naturally at its own (still-original,
never-actually-touched-because-the-runaway-was-on-TP-not-SL) stop-loss mid-investigation, realizing
a real, if oversized, `-$10.03` loss; 1852 was still open. Per explicit operator instruction, both
were deleted outright from `paper_orders` (and their `paper_order_adjustments`/
`account_equity_history` rows) rather than closed through the normal manual-close flow — a manual
close would have recorded them as legitimate trade history with a `close_reason`/realized PnL,
which they were never meant to be: both only existed because of the §31 config-overwrite bug in the
first place, and both then became test subjects for a second, unrelated bug on top of that. Deleting
`account_equity_history`'s row for 1851 does NOT retroactively corrupt the account balance — that
table's `equity_usd` column is a running snapshot written incrementally by the live application at
each trade close, not recomputed from history, so `account_equity`'s actual stored balance already
reflects 1851's real loss and needed no separate correction. The only visible artifact is a $10.03
gap in the equity **history/chart** around 21:31:57 with no corresponding row to explain it — a
minor, accepted cosmetic gap in the audit trail versus the alternative of guessing at a "corrected"
balance for an account that was never actually wrong.

**Not yet fixed, and required before `rl_sltp_adjust` can be turned back on**: `moveTP` needs some
bound on adjustment magnitude restored — not necessarily the old fixed ±2%-of-price-per-step cap
§29 removed (that cap's own problems, clustering adjustments right before close, were real and
worth solving), but something that prevents an untrained model's output from compounding toward
infinity across repeated calls. Candidates worth considering when this is picked up: cap the
absolute distance from entry a TP may ever reach (independent of SL's own distance), cap the
per-call delta as a fraction of the CURRENT TP-to-entry distance rather than of live price (so the
cap shrinks as the model's own prior moves make the position more extreme, rather than staying
constant in raw price terms), or simply hold off enabling this flag at all until `completed_trades`
has cleared SAC's own `learning_starts=100` by a comfortable margin — matching the exact caution
§14's roadmap item 5 already applies to the `rl_sizing` flag, for the same underlying reason
(an undertrained policy's output should not be trusted at full strength).

## 32. Account Balance vs. Total Equity, and dynamic per-position sizing (2026-09-04)

Prompted directly by the §31 config-overwrite incident's fallout: after fixing that config, the
panel's "Total Equity" number ($6-7) had nothing to do with the real $40 the operator intended to
trade with, because `paper_trading.notional_usd` was a config constant with no memory of "how much
capital did I actually decide to allocate." Reused the observation that a real exchange account
has exactly this same distinction and built it properly rather than patch the symptom.

### 32.1 Two numbers, not one

- **Account Balance** (`account_equity.account_balance_usd`, migration `000018`): the real,
  continuous running total since the account was first seeded. Moves by the exact same realized-PnL
  delta as Total Equity on every trade close, and is NEVER independently reset.
- **Total Equity** (`account_equity.equity_usd`, pre-existing column, reused rather than replaced):
  the balance since the operator last explicitly chose a baseline. This is what new position sizing
  computes against.

Before this column existed, `equity_usd` played both roles at once — which is exactly what made
the §31 incident's aftermath confusing: there was no way to tell "what did I actually deposit" from
"what am I currently sizing against."

### 32.2 `SetAccountCap`: reused the existing reset mechanism, not a new concept

`account_equity_history.reason` already had `'trade' | 'reset' | 'seed'` — a `'reset'` row was
already exactly the right shape for "the operator chose a new baseline," just previously only ever
triggered automatically by `ApplyRealizedPnL`'s drain-to-zero path (§15.7). `SetAccountCap`
(`internal/postgres/account_equity.go`) is the same mechanism triggered explicitly instead, via a
new `POST /api/account/cap` (`{mode?, newCapUsd}`) — no restart required, since sizing reads the
live `account_equity` row on every position open.

`GET /api/account/history` now defaults `since` to the account's own `LastResetAt` when the caller
doesn't pass one explicitly — the chart shows the story since the chosen baseline, not the account's
entire lifetime, which may span sizing regimes with nothing to do with the current one. An explicit
`?since=` still overrides it.

### 32.3 The bug: `SetAccountCap` initially left a gap between Balance and Equity

The first version of `SetAccountCap` set `equity_usd`/`initial_usd` to the new cap but left
`account_balance_usd` untouched, reasoning (wrongly) that "Account Balance should never be touched
by a reset." Deployed, then caught immediately by the operator: after setting a $40 cap against a
real balance of $7.30, the panel showed Total Equity $39.66 and Account Balance $6.96 —
**Balance sitting below Equity**, which cannot happen in a coherent model.

The root cause was a category error, not an edge case: in this schema neither field ever carries
unrealized PnL — both only move on `ApplyRealizedPnL`, at a position's close. With no open
positions' unrealized PnL to explain a difference, **Balance and Equity are mathematically required
to stay equal** outside of the one legitimate divergence (§15.7's automatic drain-to-zero "give it
another chance" reset, which is deliberately fictional — the real balance stays drained/negative in
`account_balance_usd` while `equity_usd` alone bounces back so paper trading can keep generating
data; that reset is not a human choosing a real deposit).

Choosing a trading cap is economically a **deposit or withdrawal**: it changes the real balance by
construction, to the same new value, in the same transaction — not an independent re-baselining of
one field. Fixed in `SetAccountCap` to write `account_balance_usd = newCapUSD` alongside
`equity_usd`/`initial_usd`. `TestSetAccountCap_MovesBalanceAndEquityTogether` (mutation-verified:
reverting the fix reproduces the exact reported symptom, $40 vs $51) asserts the invariant
`EquityUSD == AccountBalanceUSD` holds immediately after a cap change and continues to hold as
trades accumulate afterward. The live server data was corrected by calling the fixed endpoint with
the account's own current equity value (not a manually chosen number), closing the $32.70 gap the
buggy version had created.

### 32.4 Dynamic per-position sizing replaces the fixed `notional_usd` constant

`paper_trading.notional_usd` is gone from `PaperTrader` entirely. Every new position now opens at
`CurrentEquity / ActiveTokenCount` (`PaperTrader.dynamicNotional`,
`internal/usecase/papertrade.go`) — computed live from the account's current Total Equity on every
open, the same way a real exchange account's position sizing tracks the account's current balance
rather than a number fixed at deploy time: grows after a win, shrinks after a loss, and changes
immediately when a token is enabled/disabled.

`ActiveTokenCount` (how many of `trading.inst_ids` are not in `paper_trading_config.
disabled_inst_ids`) is computed once at `cmd/paper-trader` startup and shared by every
per-instrument `PaperTrader`, matching this service's existing "config changes need a restart"
posture — enabling/disabling a token mid-run doesn't retroactively resize an already-open position,
only the next one to open. A drained or unreadable equity falls back to
`AccountInitialUSD/ActiveTokenCount` rather than opening near-$0, which would round-trip through
every downstream percentage/leverage calculation as noise.

3 new dedicated tests in `internal/usecase/papertrade_test.go` (mutation-verified) cover: sizing
tracks live equity, sizing tracks the active-token divisor, and the not-positive-equity fallback.
7 new tests in `internal/api/account_test.go` cover `handleSetAccountCap`'s mode defaulting/
validation/real-mode allowance and `handleAccountHistory`'s since-defaults-to-LastResetAt behavior
(with an explicit `?since=` still winning, and a never-reset account correctly showing full
history rather than nothing). 417 Go tests total (was 405 after §30/§31).

Panel: `PaperTradingStatsBox` gained an "Account Balance" tile alongside the existing "Total
Equity" one, plus an inline "Set Trading Cap" number input + button that calls the new endpoint and
immediately refetches stats — no restart button needed for this one, unlike the box's other
controls, since the cap takes effect on the very next position open.

## 33. First real-API-key test uncovered the account can't trade the classic SWAP instrument at all — real trading now targets a separate execution instrument via short internal symbols (2026-09-04)

The operator's first real OKX API key test (`cmd/okx-apitest`, a throwaway diagnostic —
account balance/positions, a far-from-market order verifying the 60s fill-timeout+cancel path,
SL attach/amend/cancel via `order-algo`) surfaced two real, load-bearing findings before any order
could even be tested, plus a design decision on how real trading and market-data collection now
relate to each other.

### 33.1 `www.okx.com` rejects EEA-hosted requests with a misleading "key doesn't exist" error

The server (OVH, Roubaix, France) got `50119: API key doesn't exist` from `www.okx.com` for a
fully valid key. This is a documented OKX regional-routing behavior, not a key problem — EEA
traffic must use `my.okx.com` instead. `cmd/okx-apitest` now defaults to `my.okx.com`
(`OKX_BASE_URL` overridable). **Not yet applied to `internal/okx/rest.Client`'s own
`rest_base_url` config default** — still `https://www.okx.com` in `config.example.yaml` — since
production traffic goes through `cmd/okx-gateway`, which was not yet exercised against a real key
when this was found. Revisit before the gateway's own credentials go live.

### 33.2 This account has zero usable margin on the classic SWAP/MARGIN/FUTURES(dated) instruments

Every real order attempt against `BTC-USDT-SWAP` (linear), `BTC-USD-SWAP` (inverse), a dated
`BTC-USD-<expiry>` future, and `BTC-USDT` margin all returned `50124: This API Key does not have
trading permission for the market` — not a key-permission problem (Trade permission was confirmed
present via `GET /account/config`'s `perm` field) but an account-mode one: `GET /account/max-size`
returned `maxBuy=maxSell=0` for **every** one of them, regardless of instType. Root cause: this
account is in OKX's **Multi-currency margin mode** (`acctLv=3`) with a **USDC** balance, and the
only instrument category it can actually trade is OKX's newer **"X-Perp"** product — USD/USDC/
USDG-settled perpetual futures with an auto-rolling far-dated expiry, exposed via `instType=
FUTURES` with instId format `BTC-USD_UM_XPERP-<date>` (e.g. `BTC-USD_UM_XPERP-310404`), confirmed
directly against a mobile-app screenshot showing the same instrument traded successfully as
"X-Perp"/"UM" with a real USDC balance. `GET /account/max-size` against this instId returned
`maxBuy=maxSell=14` — the first non-zero result in the whole investigation. This account was also
found to be in **hedge mode** (`posMode: long_short_mode`), not net mode — `configs/config.yaml`
and `config.example.yaml` both still say `pos_mode: "net"`; flagged in the example config as
something to verify against the real account before going live, not fixed there directly since
real trading is still off (`use_conductor_lifecycle: false`).

Price comparison across all 10 configured tokens (SWAP vs. the matching X-Perp instrument, live
ticker data): price differs by under 0.1% in every case (arbitrage keeps the two markets tight),
but 24h volume on the X-Perp side is **29x to 144x lower** than the classic SWAP market this
project's ingestor has always collected data from — real liquidity/spread on the execution venue
is materially thinner than what paper-trading/RL training have been learning against. This does
not invalidate price-based training signal, but it does mean volume-dependent strategies
(`volume_breakout`, `vwap_reversion`) and any future order-book work were reasoning about a
different market's depth than the one real orders actually execute against — see §33.4.

### 33.3 Real-order execution now targets `ExecInstID`, separate from `InstID`

`RealTrader` (and, for consistency, the older/being-phased-out `Trader`) gained `ExecInstID`/
`ExecInstType`/`SettleCcy` fields, all falling back to today's behavior when unset (`InstID`
directly / `"SWAP"` / `"USDT"`) — every literal exchange call (`PlaceOrder`, `CancelOrder`,
`GetOrder`, `SetLeverage`, `GetPositions`, `GetBalance`) now targets these, while `InstID` stays
the market-data/observation identity. Fixed a second bug in the same pass: `reconcile()` compared
OKX's returned position `InstID` against the market-data `InstID`, which would never match once
the two diverge — silently breaking drift detection.

Also fixed real order sizing, previously `notional.Div(price)` — assumes a contract multiplier of
1, a gap CLAUDE.md §14 already flagged as open ("order sizing assumes a contract multiplier of 1").
The X-Perp instrument's real shape (`CtVal=0.0001`, `LotSz=1`) would have sized every real order
roughly 10,000x too large. `domain.Instrument` + `ExchangeClient.GetInstrument` (`GET /public/
instruments`) were added end-to-end (port, `rest.Client`, the gateway proxy, `gatewayclient`), and
`sizeToContracts` converts notional through `CtVal` and rounds **down** to the nearest `LotSz`
multiple — cached once per process via `sync.Once` (an instrument's contract shape doesn't change
while the process runs).

### 33.4 Design decision: short internal symbols everywhere, OKX's wire format only at the boundary

Explicit operator instruction, given this bot only trades USD-quoted perpetual futures: every
service, DB row, Kafka topic key, and config entry should carry a short symbol (`"BTC"`, `"ETH"`,
...), never OKX's full wire-format instId — which additionally varies by product (`BTC-USDT-SWAP`
vs. `BTC-USD_UM_XPERP-<date>`) and, for X-Perp, changes over time as OKX rolls the contract's
expiry. **Full switch, not a parallel/dual-source addition** (operator's explicit choice over
keeping SWAP-shaped data alongside X-Perp): `trading.inst_ids` now holds short symbols directly,
and `trading.symbol_map` (`internal/okx.SymbolMap`) is the *one* translation table, used *only* at
the boundary where a service is about to make a real OKX WS/REST call. `SymbolMap.Resolve`/
`ResolveAll` fail loudly on an unmapped symbol — never a silent empty-instId call, matching this
project's own "loud failure over a data gap that looks healthy" precedent (§9's bar-casing
validation).

`cmd/ingestor` is now the **only** process that ever touches OKX's wire-format instId: it resolves
each symbol to a real instId for the WS subscription, then translates every inbound message's
`instId` back to the short symbol before publishing to Kafka (`rewriteInstID` for the raw-JSON
ticker passthrough; a plain struct-field set for the already-typed `candleEvent`). Every downstream
consumer — `PaperTrader`, `RealTrader`, the panel, every DB row — only ever sees the short symbol
from here on. `cmd/trader`, `cmd/strategy-tester`, and `cmd/strategy-optimizer` each resolve a
symbol to its real instId only immediately before the one REST call that needs it (order
placement/cancel/query/leverage, and read-only `GetCandles` window-seeding respectively); every
other piece of state in those services (candle windows, trials, targets, strategy assignments)
stays keyed by the short symbol, unchanged.

**Deliberately not yet done**: no DB migration for historical `paper_orders`/`candles` rows already
keyed by the old `BTC-USDT-SWAP`-shaped instId — those stay as historical data under the old
identity; only newly-collected data uses the short symbol going forward. `config.example.yaml`'s
`symbol_map` is populated with the real X-Perp instIds verified live for all 10 configured tokens,
but — per §33.2's own warning — these carry expiry dates OKX periodically rolls and must be
re-verified against a live account before every real-trading deployment, not assumed stable.

19 new tests across `internal/okx` (SymbolMap, GetInstrument decoding), `internal/usecase`
(`sizeToContracts`, `execInstID`/`execInstType`/`settleCcy` accessors for both `RealTrader` and the
legacy `Trader`, mutation-verified regression tests proving `PlaceOrder`/`SetLeverage` target
`ExecInstID` not `InstID`), `internal/config` (symbol_map parsing), `cmd/ingestor`
(`rewriteInstID`, `reverseSymbolMap`), and `cmd/strategy-optimizer` (`seedWindow`'s symbol
resolution) — 453 Go tests total (was 424 before this section's work). None of this is deployed to
the server yet; `use_conductor_lifecycle`/`allow_real_money` remain off there.

### 33.5 Deployed to the server (2026-09-04): OKX's demo environment has no X-Perp instruments at all, so strategy-optimizer/strategy-tester are disabled; DB rows migrated to short symbols

The symbol-map design from §33.4 was deployed: `config.yaml`'s `trading.inst_ids` switched to the
10 short symbols and `symbol_map`/`exec_inst_type`/`exec_settle_ccy` were added (verified via a
before/after diff restricted to exactly those fields — nothing else in the file changed).
`cmd/ingestor` and `cmd/okx-gateway` were rebuilt and restarted; `cmd/ingestor`'s own metrics
confirmed live ticks/candles flowing under the short symbols within seconds
(`okxbot_ingestor_events_total{inst_id="BTC",...}`), sourced from the real X-Perp WS subscription.

**Found while rebuilding `cmd/strategy-optimizer`**: its candle-seeding call failed with `51001:
Instrument ID... doesn't exist` against every X-Perp instId. Root-caused, not assumed: `okx-gateway`
is currently running with **demo/simulated** OKX credentials (`simulated=true` in its own startup
log), and a direct curl to the same public `/market/candles` endpoint with
`x-simulated-trading: 1` reproduces the identical `51001` that succeeds instantly without that
header. **OKX's demo/simulated trading environment does not have the X-Perp product at all** — it
only exists on the real account. This is an environment mismatch, not a code bug: nothing here can
be fixed until the gateway holds real credentials (§27's still-pending migration step).

**Explicit operator decision, given this**: `strategy-optimizer` and `strategy-tester` must not run
at all for now — reaffirming §21's existing strategy-tester call and extending it to
strategy-optimizer (which the operator had also already decided against running, independently of
this X-Perp finding). Both containers were stopped and removed on the server. `docker-compose.yml`
gained `profiles: ["disabled"]` on both service definitions — Docker Compose excludes a service
from `docker compose config --services` entirely when its profile isn't explicitly activated, so a
plain `docker compose up -d` (no service name, no `--profile` flag) structurally cannot start
either one again, including across a host reboot. `optimizer-service` (the shared Python/Optuna
sidecar both depend on) was left running — the operator's instruction named the two Go services
specifically, and the sidecar is cheap/idle with no other consumer.

**A real, non-obvious consequence found before restarting `cmd/paper-trader`**: this project's
`strategy_assignments`/`paper_orders`/`candles`/`account_equity_history`/`strategy_param_changes`/
`tester_orders` tables are all keyed by `inst_id` as free-text, not a foreign key into any
"instrument" table — so renaming `trading.inst_ids` in config does not migrate existing rows, it
just makes `paper-trader` see every token as brand-new. Confirmed directly: `BTC-USDT-SWAP` had 54
tuned `strategy_assignments` rows built up over the project's history; after the first restart with
the new config, `BTC` (the new symbol) had exactly 1 — the origin-seeding logic's own default
`rsi_sma`/5m row, auto-created fresh. Every other token showed the identical pattern. This was
caught by comparing `strategy_assignments` row counts before declaring the deploy done, not assumed
safe from the code review alone.

**Migrated via one transaction** (`UPDATE ... SET inst_id = '<symbol>' WHERE inst_id =
'<OLD-INSTID-SWAP>'` per table, restricted to exactly the 10 currently-configured tokens):
`strategy_assignments`, `paper_orders`, `candles`, `account_equity_history`,
`strategy_param_changes`, `tester_orders`. Historical/retired tokens this project no longer trades
(`ENA-USDT-SWAP`, `XAU-USDT-SWAP` — see §22's earlier DOGE→XAU swap) were deliberately left
untouched, since they have no `symbol_map` entry and aren't part of the active roster.

One real collision found and handled before running the migration, not assumed clean: the 10
freshly auto-seeded `rsi_sma`/5m default assignments (created at the exact restart timestamp) would
have violated `strategy_assignments`' `UNIQUE(strategy_id, inst_id, bar)` constraint once the old
`BTC-USDT-SWAP`/5m/`rsi_sma` row (same `strategy_id`) was renamed to the same `(strategy_id, "BTC",
"5m")` tuple — deleted those 10 rows (identified precisely by their shared `created_at`, not by
`inst_id` pattern alone, since a same-symbol row could otherwise be mistaken for the seed) before
the rename, so the real 27-kind × 3-timeframe tuned history is what survived, not the incidental
seed. `candles`' `(inst_id, bar, ts)` primary key was checked for a similar risk and found safe
without needing a delete: old data's latest timestamp (12:30 UTC) was already behind new data's
earliest (13:00 UTC) by the time of the migration, so no `ts` overlap was possible between the
pre-rename and post-rename candle sets.

`cmd/paper-trader` was restarted a second time after the migration (its first restart, right after
the config deploy, had already cached the pre-migration empty assignment state in memory) — logs
confirmed the full historical candle windows re-seeded from Postgres under the new symbols with no
gap (e.g. `instId=BTC bar=5m candles=100`), and `okxbot_paper_orders_open{inst_id="BTC"} 1` etc.
confirmed every position that was open before the rename stayed correctly tracked afterward, keyed
by its new short symbol. One cosmetic-only artifact, left as-is: `paper_orders.features_json`'s
embedded observation snapshot (captured at order-open time as an immutable point-in-time JSON blob,
CLAUDE.md §15.3) still reads the old `"inst_id": "BTC-USDT-SWAP"` for orders opened before the
rename — correct behavior, since that field is historical record of what the model was actually
shown at the time, not live state the migration should touch.

## 34. Real orders get their own table; Paper/Real mode fully separated end to end (2026-09-04)

Explicit operator decision, **reversing §27.3/§27.7's earlier call** that real orders should share
`paper_orders` (`mode='real'`, no separate table needed). The trigger: showing an in-flight order's
fill status (pending/partial/filled/canceled) on the panel needed somewhere to live, and a status
column meaningful only for real rows didn't belong on every paper row. This section documents that
reversal is deliberate, not an oversight — §27.3/§27.7's text is left as-is (this project's own
practice of appending corrections rather than rewriting history, per §33's "correction, same day"
notes).

**New `real_orders`/`real_order_adjustments` tables** (migration `000019`), a near-exact mirror of
`paper_orders`/`paper_order_adjustments` minus the paper-only shadow-fork columns
(`parent_order_id`/`variant` — real trading has no forking, §27.3), plus a `status` column tracking
the fill lifecycle independently of `closed_at`/`close_reason`: `'pending'` (order accepted,
fill not yet confirmed — written the instant `PlaceOrder` succeeds, *before* `waitForFill` blocks,
so an in-flight order is visible on the panel for the whole wait rather than only after it
resolves), `'partial'`, `'filled'`, `'canceled'` (never filled before the timeout — the row STAYS,
so a timed-out attempt stays visible rather than being silently dropped). No `mode` column: every
row is real by construction, the table itself is the discriminator.

`usecase.RealTrader` rewired throughout to `port.RealOrder`/the new repository methods
(`OpenRealOrder`, `UpdateRealOrderStatus`, `CloseRealOrder`, `UpdateRealOrderSLTP`,
`ListRealPositions`, `RecordRealOrderAdjustment`, ...). The shared pure math this file reuses from
paper trading (`closeReason`, `realizedPnL`, `computeAdjustedLevels`, `positionStateOf`,
`unrealizedPnLPct`, `RatchetSLTP` — all typed against `port.PaperOrder`) stays untouched; a small
`asPaperOrderView(o port.RealOrder) port.PaperOrder` adapter converts at the call boundary rather
than either duplicating the math or forcing paper trading's proven code to change shape.
`RealTrader.monitorOpenPositions` also gained a `ManualCloseRequested` check (mirroring
`PaperTrader`), so the panel's Close button now has a real effect on real positions rather than
being a client-side-only no-op.

**Mode scoping extended to `strategy_assignments` and `paper_trading_config`** (migration `000020`)
— both were global before this: `cmd/paper-trader` and `cmd/trader` read the identical rows, so a
strategy tuned or a token disabled for paper trading silently took effect on real trading too.
`strategy_assignments` gained a `mode` column (unique constraint widened to include it, so the same
strategy+token+timeframe can be independently assigned under both modes); `paper_trading_config`
converted from its `id=1` singleton to one row per mode, the new `'real'` row seeded
`trading_state='stopped'` — a deliberate fail-safe distinct from paper's `'running'` default, so a
fresh real-mode config never silently defaults to running before an operator has reviewed it.
`Repository.ListAssignments`/`GetPaperTradingConfig`/`SavePaperTradingConfig`/
`SetAssignmentsEnabledForKinds` all gained a `mode` parameter; every call site updated
(`cmd/paper-trader` passes `"paper"`, `cmd/trader` passes `"real"`, `cmd/strategy-optimizer` passes
`"paper"` since it only ever compares against production's paper-trading assignments, §16.1).

**Deploying migration `000020` ahead of the Go code that reads it briefly took `paper-trader`
down** — a real, if short-lived, incident worth recording: the schema change (dropping
`paper_trading_config.id`) landed and was applied before the corresponding Go query (`WHERE
id = 1`) was updated in the same deploy step, so `paper-trader` crash-looped (`os.Exit(1)` on
`GetPaperTradingConfig`'s error) for a few minutes until the fix was built and deployed. Caught
immediately from the container's own restart-loop status and fixed same-session — no data loss,
since the crash was on startup before any trading logic ran. Lesson already documented once for
this exact failure shape (§16.10's "a documented policy is not an implemented one" — here it's "a
migration is not the same deploy as the code that depends on it," the schema/code half of the same
underlying risk): going forward, a migration that changes a column's *shape* (not just adds one)
should land in the same build/deploy step as its Go consumers, not a separate one.

**`internal/api` positions/adjust/close endpoints unified across both tables.**
`handleListPositions` routes `?mode=real` to the new `ListRealPositions`/`CountRealPositions` and
maps each `RealOrder` onto the existing `port.PaperOrder`-shaped response DTO
(`realOrderToPosition`), so the panel's `/api/positions` contract stays one shape regardless of
which table backed a row — avoiding a second parallel endpoint the panel would need entirely
separate plumbing for. This surfaced a real design gap the table split introduces: `paper_orders.id`
and `real_orders.id` are independent sequences, so an id is no longer globally unique across the
two tables. Every ID-addressed endpoint (`close`, `adjust`, `adjustments`) now requires (or, for
the read-only adjustments endpoint, defaults to `"paper"` for) an explicit `?mode=` query param to
say which table an id addresses — a caller that omits it on close/adjust gets a 400, not a guess.
`handleAdjustPosition`'s old `o.Mode != "real"` runtime check is gone; routing to `real_orders` by
table makes an accidental paper-mode call 404 for a different, more accurate reason ("no such id in
this table") rather than a mode-mismatch message. `account.go`'s `validModes` drops `'demo'`
(existing `'demo'` rows/CHECK constraints in `account_equity`/`paper_orders` are left untouched —
this only stops the API from accepting/seeding a *new* demo row going forward, per §33.4-style
"don't erase historical data just because a config default changed" precedent).

**`cmd/trader` gained a minimal restart-only HTTP surface** (`POST /restart`, mirroring
`cmd/paper-trader`'s own self-`os.Exit(0)` + Docker-restart-policy mechanism), so the panel's
Real-tab Save & Apply / Pause / Stop buttons can actually take effect — before this, real trading's
config had a write path (Postgres, via `cmd/api`'s now mode-aware endpoints) but no way to make a
running `cmd/trader` process pick up a change. `cmd/api`'s `POST /api/paper-trading/restart` became
mode-aware (`handleRestartTrading`), proxying to `cmd/paper-trader` or `cmd/trader` depending on
`?mode=`. Unlike paper-trader's control box, `cmd/trader` needed no `GET/PUT /config` mirror — its
config lives entirely in the same mode-scoped Postgres rows `cmd/api` already reads/writes
directly, and `cmd/trader` itself only reads them once at its own startup.

**Panel restructured around a page-level Paper/Real tab** (`/positions/paper`, `/positions/real`
routes — route-based rather than component state, for bookmarkability and consistency with every
other tab in this app), replacing `PositionsPage`'s old in-page mode dropdown and
`PaperTradingConfigBox`'s own internal paper/demo/real tabs + "not wired up yet" placeholder — both
modes now render the identical controls/stats components, parameterized by `mode`, reading from the
same mode-scoped backend this section describes. `PositionMode` itself drops `'demo'` from the
panel's type entirely (this project never built a demo controller and has decided not to pursue
one). Real positions show a new Status column (pending/partial/filled/canceled, dimmed for the
unremarkable `filled` case, colored for the other three) driven by `real_orders.status`. The
per-row "Adjust" button is relabeled "Update" (label-only change — the underlying component/
function/API-method/URL names are untouched, a smaller and lower-risk change with an identical
user-visible result).

**Known gap, deliberately out of scope this round**: `StrategiesPage.tsx` gained no mode-assignment
UI — the backend capability (mode-scoped `strategy_assignments`) is delivered, but assigning a
strategy to real mode today requires a direct API call (`POST /api/assignments` with
`{"mode":"real",...}`) or direct DB access, not a panel control. This directly affects the first
live real-money test: at least one `mode='real'` assignment must exist for the chosen test
token/timeframe before a real signal can ever fire, and none exist yet by default.

All 465 Go tests pass (11 new: real-order pending/canceled/status-transition coverage, positions/
adjust/close mode-routing, missing-mode rejection, real-vs-paper table isolation). `tsc -b` and
`vite build` both clean. Deployed and smoke-tested live on the server: `real_orders`/mode-scoping
migrations applied, `paper-trader`/`api`/`panel` rebuilt and confirmed serving correctly
(`GET /api/positions?mode=real` returns `{"items":[],"total":0}` — an empty, correctly-shaped real
positions view, since no real order has been placed yet), paper trading's own data/behavior
unaffected throughout. `cmd/trader` itself was rebuilt and confirmed to compile but was
**deliberately not started** — `use_conductor_lifecycle` remains off, `okx-gateway` still runs
demo credentials (§33.5) — flipping either is real-capital-risk territory reserved for an explicit,
separate go/no-go decision, not something this deploy pass does on its own judgment.

## 35. SL/TP now rests on the EXCHANGE, not only in our own process (2026-09-09)

Reported by the operator against real order 33: its stop-loss was never settled on the exchange.
Correctly identified as a red flag rather than a one-off — and it was not a bug in the sense of
code doing the wrong thing, it was the **design** doing exactly what §27.3's own "§3a correction"
(2026-09-03) had specified: real trading watched SL/TP with an in-process tick monitor, the same
mechanism paper trading uses, and deliberately placed no resting conditional order on OKX.

**That decision is now reversed** (explicit operator instruction: "we should do it on exchange
always"). The reasoning for the reversal is worth stating plainly, because the original call was
not unreasonable in isolation: an in-process monitor works only while this process is alive,
connected, and receiving ticks. A crash, a deploy, an OOM (§16.10 has one on record), a stalled
Kafka consumer (§28 has one on record), or a network partition leaves real capital running with
**no protection at all** — and nothing anywhere reports the position as unprotected, because a
position with no stop looks exactly like a position with a stop right up until it doesn't. Every
one of those failure modes has already happened to this project at least once. The exchange's own
conditional order survives all of them, because it does not depend on this process existing.

### 35.1 What was built

- **`internal/okx/rest/algo.go`** — `PlaceAlgoOrder` / `AmendAlgoOrder` / `CancelAlgoOrder` /
  `GetAlgoOrder` over OKX's `order-algo`, `amend-algos`, `cancel-algos` endpoints, plus
  `domain.AlgoOrderRequest`/`AlgoOrderAmend`/`AlgoOrderStatus` and the matching wire types. The
  wire shapes were **not invented here**: `cmd/okx-apitest/okxraw.go` had already exercised them
  against the real account (§33), deliberately holding them outside the production client until
  they were actually needed. That is now.
- Routed through `cmd/okx-gateway` like every other OKX call (`ClassTrade` for the three mutating
  calls, `ClassAccount` for the read), so real trading keeps its rate-limit priority (§27.1) and
  the credentials stay in one process.
- **`internal/usecase/real_protection.go`** — the lifecycle: `placeProtection`, `amendProtection`,
  `cancelProtection`, `ensureProtection`.

**Both levels ride on ONE conditional order**, not two. OKX treats a conditional carrying both
`slTriggerPx` and `tpTriggerPx` as OCO, so whichever fires cancels the other. Two separate orders
would leave the losing side resting after the winner filled — a live order on an account that
believes it is flat, free to open a brand-new position in the opposite direction. `slOrdPx`/
`tpOrdPx` are `"-1"` (market on trigger) rather than a limit price: a limit stop can fail to fill
in exactly the fast move that triggered it.

### 35.2 Where it hooks in, and why the ordering is what it is

| Point | Behavior |
|---|---|
| **Open** (`openReal`) | Rest SL/TP on the exchange right after the entry fills. **If it cannot be placed, the position is CLOSED again immediately.** |
| **Model adjustment** (`applyRealAdjustment`) | Amend the exchange **first**; a failure aborts the whole adjustment, leaving both sides at the old level. |
| **Panel edit** (`handleAdjustPosition`) | Same ordering. No resting order, or no exchange client → refuse (409/503) rather than write locally. |
| **Close** (`closeRealWith`) | Cancel the resting order on **every** close, including `sl`/`tp`. |
| **Verify** (`reconcile`) | Re-check every open position's protective order; re-place any that vanished. |

- **Flattening an unprotectable position** was the operator's explicit choice over keeping it and
  retrying. It costs a round-trip fee on a rare failure; holding an unprotected leveraged position
  costs an unbounded loss. If that close ALSO fails, `risk.Manager.Halt` fires — an unprotected
  position that will not flatten is the one state worth stopping everything for. Note this runs
  even when the DB insert failed: the position is live on the exchange either way, and losing our
  record of it is a bookkeeping problem while running it without a stop is a capital one.
- **Exchange-before-database on every edit.** If the row were written first and the amend then
  failed, OKX would keep enforcing the OLD stop while this system believed the new one was in
  force. Both-old and both-new are coherent states; the dangerous one is the level actually
  protecting real money being the one nobody is looking at. This is the same reasoning as
  `closeRealWith`'s existing exchange-first flatten.
- **Cancel on `sl`/`tp` closes too**, which looks redundant and is not: a stop touch detected by
  the in-process monitor is *this system's* view of why the position closed, not evidence the
  exchange's own order fired — the backup exists precisely for the case where it did not.
  Cancelling an already-triggered order is a harmless no-op; leaving a live one is not.
- **A failed verification READ never triggers a re-place.** Unknown is not absent, and re-placing
  on an unreadable status would risk two protective orders on one position, which on a hedge-mode
  account can close it twice — the opposite of protection.
- **`ensureProtection` does not escalate to a close** the way `openReal` does. A close is right at
  open time, when the position was just acquired and can be undone cheaply; force-flattening a
  running position because one API call failed would turn a transient exchange problem into a
  realized loss.

`ExchangeAlgoOrderID` / `exchange_algo_order_id` / `SetRealOrderExchangeAlgoOrderID` — added in
§27.7's commit 3 and left as documented dead code when §3a's correction landed — are now live and
load-bearing. No migration was needed.

### 35.3 The in-process monitor stays, as a backup

Explicit operator decision ("exchange and verify"). The exchange order is **primary**; the tick
monitor's close path is unchanged and still fires. This is the same defence-in-depth posture as
the 15% loss cap living in three independent places (§19.2/§19.3/§23) — no single layer is trusted
as the boundary.

### 35.4 Private WebSocket for position sync (operator's request)

`reconcileInterval` dropped **60s → 5s**, because the poll's job grew: it no longer only catches
bookkeeping drift, it also bounds how long a position can run unprotected.

On top of that, §27.4's long-deferred stretch goal finally landed: **`internal/okx/ws/private.go`**,
a reconnecting client for `wss://.../ws/v5/private` subscribed to the account's `positions`/`orders`
channels. A fill, liquidation, triggered stop, or manual close from OKX's own app now reaches the
trading process in roughly the time the message takes to arrive.

- **It runs in `cmd/okx-gateway`, not `cmd/trader`.** The private WS needs credentials, and the
  gateway is the only process holding them (§27.1) — giving `cmd/trader` its own socket would hand
  back exactly what was deliberately moved out of it. Events are republished onto a new
  `okx.account-events` Kafka topic; `cmd/trader` consumes it and calls the new
  `RealTrader.ReconcileNow` for the named instrument.
- **The events carry no position DATA, only which instrument changed.** Reconciliation re-reads
  both sides authoritatively; shipping OKX's payload across would create a second, independently
  decoded view of a position, free to disagree with the one the trading logic actually acts on.
  "Something changed on BTC, go look" is the whole message.
- **Login is awaited before subscribing.** Subscribing first gets the subscription silently
  rejected — a connected socket that never pushes anything, the same silent-gap failure shape as
  §9's mis-cased bar name. Note the login timestamp is **unix seconds**, not REST's ISO8601
  milliseconds; getting that wrong fails as an unhelpful "login failed" that names nothing.
- **`reconcile` is now serialized** (`reconcileMu`). It previously had one caller on a fixed ticker
  and could not overlap with itself; a pushed event can now trigger a pass while the periodic one
  is mid-flight, and both read-then-act on the same difference — the §16.9 race class exactly.
  Covered by a `-race` regression test that releases two passes from one channel and asserts a
  single close (mutation-checked: removing the lock fails it with 2).
- The poll **stays** underneath the socket. A WebSocket can be connected and silently stale, and
  this entire section exists because a component being up is not evidence it is working (§27.6's
  own "push plus periodic REST reconciliation is a defence-in-depth pair, not a case for picking
  only one").

### 35.5 A fake that had diverged from reality

`fakeExchangeClient.GetOrder`'s default reported a **filled** order with `AccFillSz = 0` — which no
exchange does. `order.Contracts` is derived from exactly that field, and it is what the protective
order is sized to, so every test position was unprotectable in a way production never is. Fixed the
fake rather than weakening the assertion, the same call §17 made for `SaveCandle`.

**Every new test is mutation-checked** — each one was run against the code with its own fix
reverted and confirmed to fail. 571 Go tests pass under `-race` (was 548).

**Alerting**: `okxbot_real_protection_missing_total` is the one to watch. Any nonzero value means a
live position was found unprotected, which should be impossible if placement and cancellation both
work. `okxbot_real_unprotected_closed_total` counts positions flattened for want of protection.

### 35.6 Deployed 2026-09-09 — and the EEA host caught the private WebSocket too

Deployed onto a genuinely clean slate: zero open real positions and `real` mode `paused` (the
operator had closed everything and paused before reporting the bug), which is the safest possible
moment for a change of this kind — no live position had to be migrated onto exchange-side
protection.

**§33.1's EEA-routing trap recurred, on the socket this time.** The gateway's private WS failed to
authenticate with `60032: API key doesn't exist` — the WS twin of the REST `50119` that §33.1
already documents, and just as misleading: the key is fine, the HOST is wrong. Only
`rest_base_url` had been switched to `my.okx.com` back then; `private_ws_url` still pointed at
`ws.okx.com`. Fixed to `wss://wseea.okx.com:8443/ws/v5/private` (which resolves to the same
Cloudflare endpoints as `my.okx.com`, consistent with being its EEA counterpart) and the socket
connected immediately.

Worth being precise about which sockets this affects: the **authenticated** one only. The public
and business WS URLs are unauthenticated and work from anywhere, which is exactly why this went
unnoticed for months — every other socket in this project was fine. `config.example.yaml` now
carries the EEA hosts as inline comments on all three URLs so the next person does not have to
rediscover it a third time.

**Config safety on this deploy** (§31's lesson applied): only the 25 files the commits actually
touched were transferred, as an explicit file list — never a `tar czf` of the tree — and the
server's `config.yaml` md5 was checked before and after the transfer to prove it was untouched.
The one config edit (`private_ws_url`) was made in place on the server with `sed` and verified by
diffing everything EXCEPT that line to confirm nothing else moved. The pre-deploy backup went to
`/opt/okxBot/config.yaml.bak.20260909`, not `/tmp`, since §31's backup was lost to exactly that.

Verified live after deploy: private WS `connected`, `channels=[positions orders]`, `instType=FUTURES`,
zero reconnects or errors in the following minutes; `GET /order/algo` routed through the gateway to
OKX and came back with OKX's own `51000 Parameter algoId error` for a deliberately fake id —
proving the whole path (consumer header → rate limiter → REST client → OKX) works end to end rather
than merely returning a plausible-looking local error.

### 35.7 Deploy completed 2026-09-09 — and what "one at a time" actually has to mean here

`trader` and `api` were rebuilt and restarted; both binaries were verified to genuinely contain the
new code by `strings`-grepping the running container for a string unique to it (`"rested sl/tp on
the exchange"` in trader, the manual-edit refusal message in api) rather than trusting that a
successful build was deployed. All 16 services up afterwards, zero ERROR/panic lines across
trader/api/gateway/paper-trader/ingestor in the following minutes.

**§31's "one service at a time" rule was found to be necessary but NOT sufficient.** Building
`trader` and `api` together drove free memory to 266MB and was aborted. Building `api` *alone*
then still killed **Kafka** — the broker restarted (`Up 45 seconds`) at 534MB against its 768MB cap
(§16.10's limit), and `cmd/trader` logged a burst of `connection refused` on every topic until it
came back. Nothing was lost (real trading was paused with no open positions, and the trader
correctly discarded the ticks that went stale during the outage — `dropping stale kafka message
... age=2m48s max=2m0s`, working as designed), but on a live account that same restart would have
interrupted the tick feed the backup SL/TP monitor runs on.

The real constraint is not the number of concurrent builds, it is **absolute free memory against
what the resident services already hold**: Kafka (574MB) + Grafana (369MB) + rl-service (348MB) is
~1.3GB of the 3.9GB box before a Go compiler starts. What finally worked was
`docker compose stop grafana prometheus` first (monitoring tier, not trading-critical — 2.1GB free),
build, then `start` them again.

**Deployment procedure for this box, going forward:**
1. Check free memory first. Under ~1.5GB, stop `grafana`/`prometheus` before building.
2. Build ONE service. Never two.
3. Watch **Kafka's own container status**, not just load average — it is the first thing to die and
   its death is what actually reaches the trading loop. Load average is a lagging, misleading signal
   here; it peaked at 11 during a build that harmed nothing, while the run that killed Kafka looked
   milder.
4. Restart `panel` after redeploying `api` (§18.2's nginx-caches-the-old-IP rule).
5. Verify the new code is actually in the running container, not merely that the build exited 0.

**The private WebSocket's reconnect proved itself in production the same evening**: OKX closed the
socket with a 1006 abnormal closure and the client re-authenticated and resubscribed **1.2 seconds
later**, unattended. That is the behavior the 5s REST poll exists to back up, now observed working
rather than assumed.

## 36. Two panel controls that saved successfully and did nothing (2026-09-09)

Both reported the same way — "I click Save, come out of the box, and nothing happened" — and both
turned out to be real, with different causes. Neither was a lost write: in both cases the panel
sent the right request, `cmd/api` stored it correctly, and something downstream then ignored or
undid it. That shape is worth remembering, because the instinct is to go looking at the frontend.

### 36.1 Unchecking a token was saved, then immediately overruled

`disabled_inst_ids` was ONE list with no record of who wrote each entry, and
`AffordabilityService` re-enables any disabled token that has become affordable again (§35's
roster logic). A manually disabled token is affordable **by definition** in the normal case — that
is why a person is disabling it rather than the service — so the service's next pass turned it
straight back on. That pass runs at `cmd/trader` startup, and the panel's Save triggers a restart,
so the re-enable landed within seconds of every save. The operator's decision was not lost, it was
reversed by a service that could not tell a person's choice from its own.

**Fix**: `auto_disabled_inst_ids` (migration `000030`) records the service's own entries, and
`diffFrom` proposes re-enabling only tokens in that set. The panel never writes the column, so a
manual choice survives every pass until a person reverses it. `applyPlanAuto` maintains the ledger
(claim what it disables, release what it re-enables), written in the SAME `SavePaperTradingConfig`
call as the roster it explains — writing them separately would leave a window where a token is
disabled with nothing recording who did it.

Backfill is deliberately **empty** rather than a copy of `disabled_inst_ids`: existing entries have
unknown provenance, and treating unknown as manual can only leave a token off until someone turns
it on, whereas guessing "auto" would re-enable tokens a person had deliberately disabled — the very
bug being fixed.

**The panel's `auto` tag was part of the same defect.** It was *inferred* as "disabled AND not
admissible", which cannot distinguish the two cases and got the manual one wrong exactly when it
mattered. It now reports the recorded fact.

### 36.2 Activating a strategy kind created nothing to run

`trend_confluence` was activated for real mode and produced no signals all day.
`SetAssignmentsEnabledForKinds` only ever ran an `UPDATE` — it flipped `enabled` on
`strategy_assignments` rows that already existed. Real mode had rows for exactly two kinds
(`stepped_trailing`, `vwap_reversion`, 10 instruments × 1 bar each), so selecting a third wrote
`active_kinds`, reported success, and had **nothing to enable**. The kind read as active in the
config while being entirely absent from the roster the engine loads — no error anywhere, because
nothing was wrong from the UPDATE's point of view.

**Fix**: the same call now also CREATES the missing rows, from each kind's locked origin strategy
(§11.3) across the configured instruments × decision bars, with `ON CONFLICT DO NOTHING`. The
conflict clause matters as much as the insert: `CreateAssignment`'s own `DO UPDATE SET enabled`
would re-enable a per-token assignment an operator had deliberately disabled on the Strategies
page, which is a *different, finer-grained* setting that this global per-kind switch must not
overwrite.

`cmd/trader`'s `decisionBars` computation moved above the call so the real bar list is available to
it, rather than defaulting to something the engine would not then decide on.

**The general lesson in both**: a control that writes config is only half a feature. The other half
is whether anything downstream actually consumes what was written, and whether something else is
free to overwrite it. Both bugs sat in that gap, and both looked identical from the panel — a
successful save followed by no observable change.

## 37. An exchange-executed stop recorded as a manual close (2026-09-09)

Reported as three separate observations about real orders 39 and 40: the close data was missing,
the reason said `manual` when it should have said `sl`, and trying to close them by hand from the
panel returned an error. All three came from the same place, and one of them was masking a
materially wrong number in the trade history.

**What actually happened.** The exchange-side stop-loss (§35) worked exactly as designed — verified
against OKX directly, both algo orders read `state: effective, actualSide: sl`. OKX triggered the
stop and closed the positions. What failed was how this system *recorded* that.

`reconcile`'s stale-close branch predates §35. It was written when the only way a position could
vanish from OKX without this system closing it was a person doing it in the OKX app, so it recorded
`conductor.CloseReasonManual` and — having no live tick on that path — used the order's own **entry
price** as the close price. Once the stop lives on the exchange, that assumption is wrong in the
most common case.

The consequence was not cosmetic. A close price equal to the entry price implies a trade that went
nowhere, so the recorded loss was **~15x smaller than the real one**:

| | recorded | OKX's actual |
|---|---|---|
| order 39 close px | 0.00000358 (= entry) | 0.000003527 |
| order 39 PnL | −0.018 | **−0.265** |
| order 40 PnL | −0.019 | **−0.292** |

These rows are also the model's reward signal (§15.12), so this taught the policy that two
stopped-out trades were nearly free.

**The fix: ask the exchange instead of assuming.** OKX's algo-order response carries `actualSide`
("sl"/"tp") and `ordId` — the ordinary order the trigger created, which holds the true fill price,
realized PnL and fee. Both fields were being decoded away. `closeFactsFromExchange` now reads them
and records the real reason and real numbers. A position that vanished **without** its protective
order firing is still recorded as `manual`, so the original behavior is kept as the fallback rather
than deleted — a guess is never substituted for an answer.

### 37.1 Closing was not idempotent

Found while investigating, not reported: **real order 38 was closed twice**, by two reconciliation
passes 3 seconds apart. `CloseRealOrderConfirmed`'s `UPDATE` had no `closed_at IS NULL` condition,
so the second write replaced a −0.014 loss with a +0.472 gain. Both figures were wrong, but the
point is that the second write should never have been allowed to land.

The guard is a **conditional UPDATE, not a mutex**. §35.4's `reconcileMu` only serializes passes
within one instrument's engine; each instrument has its own engine, several paths reach the close
(tick monitor, reconcile, manual, timeout), and after a restart a second *process* can race the
first. The database covers all of that atomically; an in-process lock covers none of it. Zero rows
affected returns `port.ErrOrderAlreadyClosed`, which the caller treats as "someone else already did
this" and stops — rather than going on to deliver a duplicate reward or publish a duplicate close
event, both of which really happened on order 38.

### 37.2 The panel error, and why it was correct

The manual-close error was `real order 39 is not open` — accurate, and useless. OKX's stop had
already closed the position, and the panel was still showing a Close button for it.

**This was NOT the 5-second poll**, which was my first answer and was wrong. The close events *were*
published to `okx.paper-order-events` and the panel's WebSocket bridge *did* fire — verified by
reading the topic back. The real window is much smaller: orders 40 and 39 closed 2 seconds apart,
and a click landing in that window races a close that is already in flight. No amount of WebSocket
pushing removes that race; it can only be made legible.

So the message now says what is actually true — "position #39 is already closed (sl, at …) —
nothing to close" — and is **logged server-side**. Previously it existed only as a browser alert,
which is why there was no record to look up when it was reported.

### 37.3 Correcting the history

Orders 38, 39 and 40 were corrected in place from OKX's own records, after backing the rows up to
`real_orders_backup_20260910`. 39 and 40 got their real reason (`sl`), close price, exchange PnL and
fee; 38's stored close price already matched OKX (its algo order reads `canceled` — it never fired,
so its `manual` reason was genuinely correct) and only its exchange figures were missing.

`realized_pnl` was recomputed for 39/40 from the corrected close price using the code's own formula,
and **independently reproduced OKX's numbers to the cent** (−0.265 and −0.2919) — which is what
confirms both the formula and the corrected prices rather than merely making the columns agree.

`account_equity` needed no correction: real mode records the exchange's own reported balance each
poll (§35.4) rather than summing these rows, so it was never affected by the wrong PnL.

### 36.3 The token fix broke every config save (2026-09-10)

Resuming real trading from the panel failed with:

```
column "auto_disabled_inst_ids" is of type text[] but expression is of type text (SQLSTATE 42804)
```

A regression from §36.1's own fix. The new column is bound inside
`coalesce($11, '{}')`, and a NULL parameter inside `coalesce` gives Postgres no target column to
infer a type from — so it typed the `'{}'` literal as `text` and rejected the whole expression
against a `text[]` column. The three array columns beside it are bound bare (`$4`/`$5`/`$6`), where
the target column *is* available to infer from, which is why only the new one broke.

The blast radius was every save the panel makes whose patch does not itself set auto-disabled —
resume, pause, long/short, timeframes, active strategies. Fixed by casting both binds explicitly
(`$11::text[]`, `coalesce(..., '{}'::text[])`).

**`internal/postgres` had no tests at all**, and that is precisely how this shipped: every caller is
covered through `fakeRepository`, and a fake cannot reproduce Postgres's type inference — the Go
code was correct, the SQL was not. Added `paper_trading_config_test.go`, which runs against a real
database and skips cleanly without one. A mocked version of that test would have passed against the
broken SQL, which is the whole reason it is written this way.

Verified the type rule directly on the server before deploying, rather than reasoning about it:

```
select pg_typeof(coalesce(NULL, '{}'))               -> text
select pg_typeof(coalesce(NULL::text[], '{}'::text[])) -> text[]
```

Rebuilt all three services that link the function (`api`, `paper-trader`, and `trader` via
`AffordabilityService`). The affordability caller never failed because it always sets the field —
which also means the bug was invisible to the one path that exercised the new column most often.

## 38. Three bugs stacked behind one error message (2026-09-10)

Reported as one thing — a manual SL/TP edit from the panel failing — and it was three defects in a
row, each only visible once the one in front of it was cleared. Worth recording as a sequence,
because the lesson is that each "fix" looked complete until the next layer answered.

**1. `cmd/api` had no `OKX_GATEWAY_URL`.** §35 added a gateway client to it for the manual edit
without adding the env var, so `config.Load`'s own `http://localhost:8094` default applied — which
inside that container is its own loopback, not the gateway. Presented as
`dial tcp [::1]:8094: connect: connection refused`. The Manage Tokens affordability column shares
that client and had the same latent gap. Fixed in `docker-compose.yml`, plus a `depends_on` so
`api` cannot start before the gateway.

**2. Prices were not rounded to the instrument's tick.** With the request finally reaching OKX, it
was rejected. A level derived from a percentage lands on arbitrary decimals — `100.41424` against
SOL's `0.01` tick. `domain.Instrument` did not carry `tickSz` at all, so nothing could have rounded
it. Now decoded and applied everywhere a price reaches the exchange: levels rested at open, the
model's in-trade amends, and the operator's manual edit. The manual path stores the ROUNDED price,
so the panel shows the level the exchange is actually enforcing rather than the one requested.
Ticks on this account span `0.01` (SOL, ETH) to `1e-9` (PEPE), so a fixed precision would have been
wrong for most instruments.

**3. The trigger-price TYPE was missing.** Still rejected, now with `code=1` and an empty message.
Recovered by replaying the request by hand against the account:

```
sCode=51000  sMsg="Parameter newTpTriggerPxType error"
```

OKX requires `slTriggerPxType`/`tpTriggerPxType` whenever a take-profit side is present, on both
the place and amend paths. **This is why every real position has been opening with a stop and
`TPTriggerPx: 0`** — OKX silently dropped the TP side rather than failing the placement, so every
real position has run with no take-profit on the exchange since §35 shipped. `"last"` is chosen
deliberately: the in-process backup monitor compares against the last traded price, so exchange and
backup now agree on what counts as a touch; mark price would have them disagree exactly when it
matters.

### 38.1 The error handling is what hid it

OKX's `code=1` means "an item in this batch failed" and its top-level `msg` is **empty** — the
reason is per-item in `data[].sMsg`. `rest.Client.do` reported only the envelope, turning a
perfectly specific rejection into `code=1 msg=`. That cost two rounds of debugging before the
message was recovered manually. `firstItemError` now surfaces the per-item detail.

**Only `cmd/okx-gateway` links `internal/okx/rest`** in production — every other service reaches
OKX through it. I rebuilt `api` twice before checking that, which was wasted time on a box where a
build takes ~10 minutes. Verify which binary actually contains a changed package (`grep -rln` over
`cmd/`) before choosing what to rebuild.

### 38.2 A fired stop read a beat too early, and a poll that rate-limited itself

Real order 43 closed minutes after §37's fix shipped and was STILL recorded as `manual` at its
entry price. Two further causes, both in its own logs:

- OKX reports `state="effective"` the instant a protective order fires but fills in WHICH side
  fired a beat later. The poll read it ~1s after the trigger, got an empty `actualSide`, and fell
  back to manual; the same read moments later carried `actualSide="sl"`. Now retried briefly — this
  is a close being recorded once, and getting its reason and real close price right is worth a
  one-second wait on a path that only runs when a position has just disappeared.
- The same logs showed `50011 Too Many Requests` on `/account/positions` and `/account/balance`.
  The reconciliation poll runs **per instrument**, so §35.4's 5s cadence issued 20 account-class
  calls every 5 seconds across a 10-token roster — for two endpoints that are account-wide and
  return identical data to every one of them. Not harmless: a pass that cannot read positions
  cannot detect drift, and one that cannot read the protective order falls back to manual. Raised
  to 20s. **The right fix is one account-wide poll shared across instruments rather than N
  identical ones**, which is a larger change than a constant.

### 38.3 History corrected

Orders 41 and 43 were corrected from OKX's own records the same way §37.3 handled 39/40 (backup in
`real_orders_backup_20260910b`); 42 was closed by our own flatten and only lacked its fee.
`realized_pnl` recomputed from each corrected close price **independently reproduced OKX's own
figures to the cent** (−0.237 and −0.07692), which is what confirms the prices rather than merely
making the columns agree. All six real orders (38-43) now carry truthful reasons, prices and fees.

## 39. One account-wide reconciliation pass, not one per token (2026-09-10)

Operator's own observation, following §38.2's rate-limit finding: "seems like we run it for each
token separately, but we can do it all together, right?" — correct, and the fix §38.2 applied
(raising the interval 5s → 20s) had only slowed the waste rather than removed it.

**The shape of the problem.** `GetPositions` takes an `instType`; `GetBalance` takes a settlement
currency. Neither takes an instrument — both are **account-scoped** and return the identical
response to every engine. But `reconcile` ran per `RealTrader`, so a 10-token roster issued **20
calls per cycle to learn what 2 calls carry**. That is what OKX rate-limited with `50011` on
`/account/positions` and `/account/balance`, and it is not cosmetic: a pass that cannot read
positions cannot detect drift, and one that cannot read the protective order records a real close
as `manual` at the entry price (§37's bug).

**Fix**: `usecase.AccountSnapshot` carries the two reads; `usecase.ReconcileDriver` owns the cadence
and hands the same snapshot to every engine's new `ReconcileWith`. Per-instrument work is untouched
and still per-instrument — verifying each open position's own protective order via `GetAlgoOrder`
genuinely differs per engine and stays inside it.

Every **decision** about a position stays in `ReconcileWith`. That is the same principle §35.4
applied to the WebSocket push: one definition of how this system responds to a position change, and
a second would be free to drift from it in a way only visible when the two disagreed about real
money.

**The WebSocket push now enters at `ReconcileDriver.ReconcileInstrument`**, which fetches a **fresh**
snapshot rather than reusing the periodic one (explicit operator decision). A push means that
instrument genuinely changed, so acting on data up to a full interval old would give up exactly the
latency the socket exists to provide. It still costs 2 calls, not one per engine.

**Equity recording moved to the driver too**, and this was the subtler half. `recordEquityReal`
writes ONE shared `account_equity` row, so ten engines calling it opened **ten transactions for one
row's work** — and when the delta was nonzero, whichever engine ran first stamped its own `instID`
on the history row, attributing an account-wide balance change to one arbitrary token.

**`ReconciledExternally`** lets a `RealTrader` keep its own loop when no driver is wired. Dropping
the loop outright would make an engine's safety depend on a caller remembering to wire a driver —
`cmd/trader` sets the flag, and a source-level test fails if it is ever removed, the same guard
`buildRealTraderClamps` has for the same reason (§23: a field silently dropped from a large inline
struct literal is what left every real position uncapped).

**The call COUNT is the behavior under test.** A per-engine poll and a shared one are
indistinguishable by their effect on positions — which is precisely why this went unnoticed for
months — so `fakeExchangeClient` now counts `GetPositions`/`GetBalance` calls and the test asserts
1, not 11.

**A test that passed against the broken code, and what it taught.** The first equity test asserted
on the resulting history ROWS and passed under mutation, because the fake (correctly mirroring
Postgres) computes its delta from `AccountBalanceUSD`, which the first engine already updated — so
the nine redundant calls each saw a zero delta and wrote nothing. The test was proving the
repository is idempotent, not that the driver calls it once. Rewritten to count calls. Worth
remembering: when the waste is *redundant work* rather than *wrong data*, asserting on the data
cannot see it.

9 new tests, every one mutation-checked (each fails with its own fix reverted; the per-engine-fetch
mutation reproduces `got 11` exactly). 596 Go tests pass under `-race` (was 587).

**Deployed 2026-09-10** following §35.7's procedure: 239MB free, so `grafana`/`prometheus` were
stopped first (1834MB available), `trader` built alone, Kafka confirmed still `Up 11 hours (healthy)`
— no restart — then monitoring resumed. Config md5 checked before and after the file transfer
(§31's guard), unchanged. The running binary was verified to contain the new code by `strings`,
not by trusting the build's exit code.

**Initially unverifiable in production, then measured.** At first deploy there was no way to observe
the call reduction on the server: real mode is `trading_state=stopped` with zero open positions and
a flat balance, so the poll has nothing to log, and neither `cmd/trader` nor `cmd/okx-gateway`
logged individual requests. §27.1 had described a per-consumer request counter since the gateway was
designed, and it turned out never to have been implemented (§40) — so the missing measurement was
built, and the reduction is now confirmed against live traffic:

```
okxbot_gateway_requests_total{consumer="trader",endpoint="account",status="ok"}
  6 calls in 60 seconds
```

Which is exactly right: 3 passes/minute at the 20s interval × 2 account calls (GetPositions +
GetBalance) = 6. The per-engine code would have made 3 × 20 = **60**. A tenfold reduction, measured
rather than reasoned about.

## 40. The gateway's request counter existed only in this document (2026-09-10)

Found while trying to verify §39's call reduction on the server and discovering there was no way to
observe OKX call volume at all.

§27.1 has described `okxbot_gateway_requests_total{consumer,endpoint,status}` since the gateway was
designed — "visibility into exactly who is consuming how much OKX-side budget" — and §27.7 lists a
"Prometheus metrics endpoint" as delivered. Both were true only on paper: `cmd/okx-gateway` called
`metrics.Serve(...)`, which starts the HTTP handler, and registered **no metrics of its own**. So
`:9105/metrics` served Go runtime stats (goroutines, GC, heap) and nothing else.

**Why it mattered more than a missing dashboard.** The gateway is the single process that talks to
OKX (§27.1), so it is the only place where call volume can be attributed to a consumer. Without the
counter, §38.2's `50011 Too Many Requests` on `/account/positions` had to be root-caused by reading
code and reasoning about it — the direct question ("which service is spending the budget?") could
not be asked at all.

**Fix**: the counter lives in `service.call`, the one choke point every proxied request passes
through, so a handler added later is counted without anyone remembering to instrument it. Being held
back by the gateway's OWN limiter is counted as `rate_limited` rather than `error` — that is a
capacity signal, while an error is a request that actually went out and failed, and conflating the
two would hide exactly the case this counter exists to expose. A request with no
`X-Gateway-Consumer` header counts under `unknown` rather than being dropped: `consumerAndPriority`
already defaults it that way rather than rejecting the request, and traffic from a service that
forgot the header is precisely what should not be invisible here.

3 tests, mutation-checked (reporting every outcome as `ok` fails the error test; hardcoding the
consumer label fails all three). 599 Go tests total.

**It paid for itself within a minute of deploying** — see §39's measurement, which was impossible an
hour earlier and is now a single `curl`.

Another instance of §16.10's lesson, which this project keeps relearning: **a documented property
needs a check that would fail if it were absent.** Prose in this file is not that check. Both §11.2's
`restart: unless-stopped` and §11's VPN-only network model read as settled decisions for months
while being absent from `docker-compose.yml`; this counter read as delivered in a checklist that
marked the gateway complete.

**Still open**: the counter is registered but nothing scrapes it usefully yet — `prometheus.yml`
should have a job for `okx-gateway:9105`, and a Grafana panel showing calls/sec per consumer would
turn the next rate-limit incident into a glance instead of an investigation. Not done here; the
counter itself was the blocking gap.

## 41. realized_pnl ignored the exchange's own figures (2026-09-10)

Reported as real orders closing with data that "doesn't update properly" — the reason coming out
`manual`, and the PnL not updating. Two claims, and checking the live rows separated them.

**The `manual` reason was CORRECT, not a bug.** Every row labelled `manual` also has
`manual_close_requested = true` — those were genuinely closed from the panel by the operator, not
mislabelled exchange closes. §37 and §38.2's fixes are working: orders 39-43 all recorded `sl` with
real exchange close prices. Worth stating plainly because the instinct was to go looking for a
regression in the close-reason logic, and there wasn't one.

**The PnL claim was real, and worse than it looked.** `closeRealWith` computed `realized_pnl`
**locally on every close**, even where OKX's own figures had just been fetched two lines above and
written to adjacent columns — under a comment saying they were "preferred over a local calculation
that cannot see fees, funding, or the true fill price."

**Why the disagreement read as a puzzle: the two sides measure different things.** OKX's `pnl` on
the flattening order is **gross** — the price move alone, with the fee reported separately in `fee`
as a negative charge — while the local `realizedPnL` subtracts its own **estimated** fee from its
own gross. So the stored column and the exchange column looked like a cross-check that disagreed,
when in fact **neither held the net figure** the panel shows and the model trains on.

Verified against every closed real order before changing anything: OKX's gross `pnl` equals the pure
price math `(close-entry)/entry * size * leverage` to the last digit on **all** of them. The exchange
and this codebase agree completely about the price move. Every discrepancy was the fee:

| rows | what was stored | gap |
|---|---|---|
| 39-43 (backfilled by hand, §37.3/§38.3) | OKX's **gross**, no fee subtracted at all | exactly one fee |
| 32-38 (closed by live code) | local calc with an **estimated** fee | ≈ one fee |

**Fix**: `netRealizedPnL` prefers `exchange gross + exchange fee`, falling back to the local
calculation only where OKX reported nothing (a `skipExchange` close whose algo order could not be
read) — an estimated fee still beats no fee. The fee is **added**, not subtracted, because OKX
reports it negative; subtracting would credit it and overstate every trade by twice its cost. That
sign is the one thing here that would look plausible on every row while being wrong on all of them,
so it has its own test.

**This also fixes the model's reward.** `reportTerminalReal` passes the same `pnl`, so every real
trade was training on a number off by roughly a round-trip fee — and on order 37, off by its
**sign**: stored as a `-0.0159` loss when the truth is `+0.0017` gross and `-0.0071` net.

**38 historical rows corrected** from OKX's own figures (backup `real_orders_backup_20260910c`);
every row with an exchange figure now satisfies `realized_pnl = exchange_realized_pnl +
exchange_fee` exactly.

5 tests, mutation-checked — computing locally, flipping the fee's sign, and ignoring the fee each
fail their own test. 604 Go tests pass (was 599).

**Deployed** following §35.7: monitoring stopped first (486MB free), `trader` built alone, Kafka
confirmed `Up 11 hours (healthy)` — no restart — new code verified in the running binary by
`strings`, config md5 unchanged before and after transfer.

**Not yet observed on a live close**: real mode is `trading_state=stopped`, so the corrected path
has not run against a fresh close yet. The historical rows are corrected and the tests cover the
formula, but the first real close after trading resumes is worth checking against OKX directly.

## 42. "SL/TP stopped adjusting" — the model changed its answer, and nothing recorded it (2026-09-10)

Reported as paper trading no longer adjusting SL/TP at all, where it "used to update constantly a
few days ago". Real, and measurable — `paper_order_adjustments` per day:

| 09-05 | 09-06 | 09-07 | 09-08 | 09-09 | 09-10 |
|---|---|---|---|---|---|
| 256 | 541 | **690** | 262 | 26 | **1** |

**Nothing was broken.** `rl_sltp_adjust` is still `true`, `runUpdates` still runs on every throttled
tick, and one adjustment applied during the investigation itself. What changed is the model's
**answer**. Over 13 hours of `okxbot_model_update_decisions_total`:

```
close:  2150
none:      22
update:     2   <- the answer that adjusts SL/TP
```

The giveaway was that `okxbot_controller_updates_total` and the `close` decision counter were
**identical per instrument** (BTC 89/89, PEPE 389/389): every update call was being answered
"close".

**Why the model wants out of everything**: paper PnL has been negative every day since 09-04.
Closing is the only action that acts on that, so a policy learning from those outcomes converges on
it. That is the model working, not failing.

**But asking to close is not the same as being able to.** `rl_early_close` is off (§15.12 keeps it
opt-in because early close destroys the counterfactual), so all 2150 requests were discarded — and
this is the actual defect: **`PaperTrader.closeEarly` discarded them with no log and no metric**,
while `RealTrader.closeEarly` has recorded both since 2026-09-08. From the outside the engine looked
idle: the model was asked, answered, and its answer left no trace anywhere.

That silence is what turned a straightforward behavior change into an apparently broken feature. The
symptom pointed at the adjustment path, which was fine; the cause was one branch up, invisible.

**Fix**: paper now matches real — `okxbot_paper_early_close_ignored_total` plus an info log. No
policy change: whether to enable `rl_early_close`, or to act on the model wanting out of these
trades, is a separate decision this does not take. 2 tests, mutation-checked (removing the metric
and closing anyway each fail). 606 Go tests pass (was 604).

**The general lesson, and it is the same one as §16.9**: a path that declines to act leaves no trace
unless something is written to make it visible. §16.9 recorded exactly this for `sizeFromAction`'s
silent fallbacks ("the model was consulted on all 522 signals, answered unusably every time, and the
metric stayed empty, which read as 'the model was never called'"). This is the same failure shape in
a different branch — worth noting that the fix there did not generalize, because each suppressed
path has to be instrumented individually.

**Open question this surfaces, not answered here**: the model asking to exit 99% of open positions
is a strong signal about the current strategy mix, and it is currently unheard. Either it is right
(and these trades should not be held), or it has overfit to a losing stretch. The counter now makes
that answerable — pair `paper_early_close_ignored_total` with how those positions actually resolved
(SL/TP/timeout) to judge whether the model was right to want out, which is exactly what
`RealEarlyCloseIgnoredTotal`'s own comment already proposed for real trading.

## 43. Every "exchange error" was the service racing the exchange's own stop (2026-09-11)

Surfaced by §42's notification work: once the 13 errors were readable in one place, all of them
turned out to be the same thing. The operator diagnosed it before any code was read — "it's
probably the SL/TP, the exchange closes it itself and then the service also tries to close it" —
and the data agreed exactly.

**9 of 9 genuine failures are `sCode=51169`**: *"Order failed because you don't have any positions
in this direction for this contract to reduce or close."* Every one has `close_reason` of `sl` or
`tp`, and every one is on an order that had a protective order resting on the exchange. They begin
the day exchange-side SL/TP shipped (§35). (The other 4 rows carrying `last_error` are notes
written by hand during earlier incidents, not failures — worth checking before counting.)

**Nothing was ever mis-closed.** The exchange fired its stop and closed the position correctly;
this process's tick monitor then saw the same touch a moment later and asked OKX to close a
position that no longer existed. What the race damaged was the alert channel — each success wrote
an alarming `last_error` onto a trade that went exactly as intended, and an alert channel full of
false alarms is worse than no alert channel.

**Fix**: `monitorOpenPositions` asks the exchange before flattening, but **only on an SL/TP
touch**. A manual close or a timeout is this system deciding to exit, and no resting order will
have done that for us — deferring those to the exchange would be a real bug. When the protective
order reads `effective`, the position is recorded closed through `closeFactsFromExchange` (OKX's
own reason, fill price, PnL and fee, §37) instead of sending a duplicate flatten.

**The case that mattered most while writing it**: an unreadable status is "don't know", never
"already handled". `protectionAlreadyFired` returns `(fired, ok)` and any error gives `ok=false`,
falling back to the normal close — declining to flatten on a failed API call would leave real
exposure open on nothing more than a network blip. That has its own test, and the mutation
inverting it fails.

4 tests, all mutation-checked; removing the guard reproduces the production bug exactly ("got 1"
duplicate flatten). 610 Go tests pass under `-race`.

**Not yet exercised live**: real mode is `paused` with zero open positions, so the guard has not
run against a real SL touch. The next real stop-out is worth checking — it should record `sl` with
the exchange's own numbers and write no `last_error` at all.

**Historical rows left as-is**: the 9 existing `51169` errors describe a real (if harmless) race
that happened, and rewriting them would erase the evidence for this section. They stay readable in
the bell until an operator marks them read.

## 44. Unbounded decimal precision stopped paper trading entirely (2026-09-11)

Reported as "paper SL/TP isn't working", with a fair question attached: was it the edit just made?
It was not — §43's change touched only `RealTrader` files — but the report was right, and the cause
was mine from 2026-09-04.

**Symptom**: paper trading had stopped closing positions completely. Last close 04:45, last open
07:25, nothing for hours. The process was up, consuming ticks, evaluating strategies, and calling
the model; it just could not finish a close. Every attempt failed with

```
ERROR: invalid scale in external "numeric" value (SQLSTATE 22P03)
```

and since a failed close is logged and retried rather than fatal, nothing looked crashed. The
engine looked healthy from every angle except the one that mattered.

**Root cause**: `paper_orders.size` had grown to **16,380 digits**. Dynamic sizing (§32.4) makes
`size = equity / activeTokens`, a repeating decimal for any roster size that is not a power of ten.
Closing that position computes PnL from `size`, and multiplication **adds** digits rather than
capping them, so the PnL comes out longer than the size was. That PnL is added to stored equity,
which sizes the next order. Nothing bounded the loop.

Digits in `paper_orders.size`, by day, straight from production:

| 09-03 | 09-04 | 09-05 | 09-06 | 09-07 | 09-08 | 09-09 | 09-10 |
|---|---|---|---|---|---|---|---|
| 1 | 18 | 395 | 4,647 | 11,243 | 14,366 | 15,491 | **16,380** |

09-03 is the day before dynamic sizing shipped, when `notional_usd` was a config constant. 16,380
is where PostgreSQL's NUMERIC limit rejected the write.

**This is §16.8's EMA bug in a different place.** `decimal.Decimal` is arbitrary-precision and
nothing truncates it on its own; any recurrence that feeds its own output back into its next input
grows without limit. The fix is the same: round at each step, so the value entering the next
computation is already bounded. `usdScale = 8` is applied to the sizing division, realized PnL,
trading fees, and funding cost — every value that reaches stored equity. Eight decimal places is a
hundred-millionth of a dollar, far finer than any real amount needs.

**Why `Div` alone was not enough to catch it**: `shopspring/decimal` caps division at
`DivisionPrecision` (16), so a single division looks bounded and testing one in isolation proves
nothing. It is the `Mul` afterwards that grows the value, and only the round trip through the
database makes it compound. A test that exercises one generation cannot see this — the regression
test runs 40 and asserts the width stays flat.

**Data repaired** (backup: `paper_precision_backup_20260911`): 874 orders, 1,401 equity-history
rows, and the account row rounded to 8dp. Paper equity went from 16,364 digits to `15.87060161`.

**Verified recovered**: within 30 minutes of the deploy, 8 positions closed — 2 `sl`, 1 `tp`, 5
`timeout` — and zero `22P03` errors. New orders carry clean 8dp sizes.

**Worth keeping from this one**: a failing write that is logged-and-retried rather than fatal can
stop a subsystem completely while every health signal stays green. The bell (§42) showed nothing
because these were paper orders, and `paper_orders` has no `last_error` column at all (§43). The
only visible symptom was an absence — no closes — which is exactly the kind of thing nobody
notices until they go looking.

## 45. Unreachable take-profits: a missing ratio cap, and twelve V2 strategies (2026-09-12)

Reported as two observations about paper trading: some TPs sat "60 or 70 percent" away, which is
very hard to touch at 10x, and separately, positions that ran to +20 or +30% gave it all back
because the target was too far and the SL/TP never adjusted. Both were real, and quantifying them
from `paper_orders` is what found the cause.

**The cause was not the strategies.** `conductor.Clamps` enforced a MINIMUM reward:risk
(`MinTPSLRatio`) and no maximum, and the two clamps interacted:

1. A strategy proposes a structurally tight stop. `ict_fvg` used the fair-value gap's own far edge,
   which on a 5m bar measured **0.011%** from entry on real orders.
2. `Apply` clamps that stop UP to the `min_sl_dist_pct` floor of 0.5% — a ~45x widening.
3. `MinTPSLRatio: 1.5` is then applied to the **widened** stop, carrying the widening into the
   target.
4. Nothing capped the result.

So a strategy's own intended 2:1 became an effective 40:1-80:1. Mean realized reward:risk measured
across 1841 closed paper orders: **47:1** on `ict_fvg`, **69:1** on `range_breakout`, **80:1** on
`ict_order_block`. Production orders 2514/2518/2549 all carry the signature — a stop 0.011%-0.074%
away paired with a target 4.5%-6.5% away, which at 10x is 45-65% of margin.

The give-back claim was the same bug seen from the other end: **309 closed positions reached +10%
of margin or better, and 74 of them still closed at a loss** — they had no reachable exit.

### 45.1 The fix: `MaxTPSLRatio`, for paper AND real

`conductor.Clamps` gains `MaxTPSLRatio`, applied after `MinTPSLRatio` so the min/max order is
well-defined. Config `max_tp_sl_ratio` defaults to **3** even when unset — the same "not opt-in"
treatment `MaxLossPct` gets (§19.2), because an unreachable target is the failure this prevents
rather than a mode to opt into. 3:1 is deliberately generous: it leaves every strategy's intended
1.5:1-2:1 untouched and bites only on the pathological ratios.

Capping the RATIO rather than an absolute distance is deliberate — it scales with the instrument's
own volatility exactly as the stop does, so one bound is correct for BTC and PEPE alike.

Applied to **both** paper and real, per explicit operator instruction ("چون اونجا هم همش خودم دارم
دستی تغییرش میدم" — they had been correcting real TPs by hand). Both `cmd/trader` and
`cmd/paper-trader` build their own `conductor.Clamps` struct literal, the duplication §23 records as
having already caused one incident, so both were wired and both got a regression test.

**`min_sl_dist_pct` was deliberately NOT lowered**, which was the first instinct. Checking how often
the floor actually binds showed it pins only `weekly_dip_buy` (281 of 281 trades) and is rare
elsewhere — average stops sit at 0.28%-0.86%, above it. The floor contributes to the mechanism but
the missing TP cap is what makes it pathological, so only the cap was added.

### 45.2 Twelve V2 strategies

Registered as separate `_v2` kinds rather than edits to their parents, per §11.3's locked-origin
rule: V1 keeps its accumulated history and stays independently comparable. Panel names were tagged
`<kind>_V1` / `<kind>_V2` so the two are distinguishable (the `kind` column is untouched, so every
existing assignment and historical order still resolves).

What changed uniformly, driven by the V1 data rather than taste:

- **Levels are sized in ATR units**, not fixed percentages. A median 5m candle is **0.081% on BTC
  and 0.453% on ZEC** — 5.6x apart — so V1's single pair of percentages was simultaneously too
  tight to survive noise on ZEC and too wide to be reached on BTC.
- **Reward:risk is bounded at the source** (`v2Levels`), with a hard floor at **1:1** per the
  operator's explicit requirement that TP% never fall below SL%.
- **Most gained a regime filter** — trend, volatility, or conviction. The V1 record showed the
  losses came from taking every occurrence of a pattern rather than from the pattern itself:
  `ema_ribbon_pullback` won 12.5% of 40 trades because it entered on a bare touch of the mid EMA,
  which on 5m is as often a trend failure as a pullback.

### 45.3 Real market data corrected two things a synthetic fixture hid

Worth recording because the synthetic fixture was written specifically to avoid §30.1's vacuity
trap and still misled:

1. **Six V2 strategies appeared inert** against generated candles and fire normally against real
   ones — the generator was too smooth to produce the patterns they look for. Had that been trusted,
   six working strategies would have been "fixed" until they fired on a fixture that was itself
   tuned until they fired, which measures nothing. The tests now run against **1200 real OKX 5m
   candles per instrument** (BTC/SOL/ZEC, committed as `internal/strategy/testdata/`).

2. **The ratio bound alone was insufficient.** ATR scales with the instrument, so a 1.3-ATR stop at
   2:1 is a reachable ~8% of margin on BTC and was **32% on ZEC** at the same 10x — identical ratio,
   unreachable distance. `v2Levels` now also caps absolute distance at 2% of price, shrinking
   **risk before reward** so the cap can never push the ratio below the 1:1 floor.

Every guard is mutation-checked: removing the TP cap reproduces the production ratio exactly
(32.5:1), disabling the distance cap fails the ZEC case, removing the 1:1 floor fails its own test.
The floor's test calls `v2Levels` directly rather than going through `WithParams`, because
`ClampParam` already floors `risk_reward` at its ParamSpec `Min: 1` — a test driven through the
parameter path passes whether or not the floor exists, and was discarded once that was verified.

712 Go tests pass, up from 610.

### 45.4 Activation, and a config layer that silently overrides assignments

V2 activated in **paper only** (explicit instruction); real mode's four assignments are untouched.
The V1 kinds of the twelve that were still running (`vwap_reversion`, `bb_squeeze_breakout`) were
disabled; the three pre-September originals still active (`rsi_sma_fuzzy`, `stepped_trailing`,
`trend_confluence`) were left alone as not mine to retire.

**Inserting `strategy_assignments` rows was not enough, and this is worth knowing.**
`paper_trading_config.active_kinds` (§22) is a coarser per-KIND switch that `cmd/paper-trader`
applies at startup via `SetAssignmentsEnabledForKinds`, which runs
`enabled = (kind = ANY(active_kinds))` across every paper assignment — so it **overwrites** whatever
the assignment rows say. The first restart after inserting 120 V2 assignments showed only V1
strategies evaluating, because `active_kinds` still listed the old five. Activating a new kind means
updating that list too. Confirmed from `okxbot_strategy_signals_total` rather than assumed.

Two cleanups in the same pass: the activation cross-joined every (inst_id, bar) pair present in
paper assignments, which produced 216 rows on 15m/1H that `active_bars` (`{5m}`) means never
decide — disabled rather than deleted, so re-enabling a timeframe is a flag flip. And 48 rows
landed on `ENA-USDT-SWAP`/`XAU-USDT-SWAP`, retired instruments left behind by §33.5's symbol
migration — deleted. Final state: **120 enabled V2 assignments** = 12 strategies × 10 live tokens
on 5m.

**Verified on live orders**, not just in tests. The first two V2 orders: `ict_fvg_v2` at R:R 3.00
with TP 14.6% of margin, `ict_order_block_v2` at R:R 1.50 with TP 22.5% — against V1's 47:1 and
80:1. Open positions carried over from before the restart still show TPs at 79%, 84% and 98% of
margin, which is the before/after in one query. The 15% loss cap holds on every one.

### 45.5 Advice on the model, separate from the strategies

Three things the data says, none of them fixed here:

- **The model is being asked to close almost everything and is not being heard.** §42 measured
  2150 `close` answers against 22 `none` and 2 `update` over 13 hours, all discarded because
  `rl_early_close` is off. Whether it is right is now answerable: pair
  `okxbot_paper_early_close_ignored_total` against how those positions actually resolved.
- **`rl_sizing` should stay off until `completed_trades` clears 100 by a real margin.** §14's own
  record of the first attempt (39 trades, converged to 100% `skip` within 20 minutes) is the
  precedent.
- **The reward signal was distorted for as long as the targets were.** A policy trained where TP is
  unreachable learns that holding is pointless, which is consistent with it now wanting to close
  everything. Retraining judgement should wait for post-fix trades rather than being drawn from the
  existing buffer.

### 45.6 The panel's "new" badge was pointing at the superseded kinds (2026-09-12)

Caught by the operator immediately after §45.2 shipped: the twelve were supposed to be relabelled
V1 with the badge moving to the new generation, and only the badge half was missed.

`StrategyKindModal.tsx` drove the badge from `NEW_KINDS`, a hardcoded list naming the twelve kinds
added in §30. Shipping their V2 revisions made that list stale in the worst possible direction — the
panel went on labelling the **superseded** kinds "new" while the actual new ones carried no tag at
all, which is more misleading than having no badge.

Now derived from the kind's own `_v2` suffix (`isNewKind`). The V2 names already carry their
generation, so the tag reads the same thing that makes a kind new rather than a parallel list that
has to be remembered separately — the class of bug §36.1/§36.2 keep producing, where a second
record of the same fact drifts from the first. A third generation would want a real column rather
than a third convention.

Verified against the live panel: 12 kinds tagged, the V1 originals not.

## 46. A second exchange: MEXC, and what it proved about the adapter layer (2026-09-13)

Prompted by the decision to open-source this project: the operator wanted to confirm the adapter
and port layers were real abstractions rather than OKX-shaped, and chose MEXC as the test. The
instruction that shaped the work: OKX's peculiarities — contract-name handling, expiry dates,
volume constraints — must be scoped to OKX and not imposed on every exchange.

**The headline result is one line** (`internal/mexc/adapter.go`):

```go
var _ port.ExchangeClient = (*rest.Client)(nil)
```

It compiles. A second exchange satisfies all 14 port methods with **no change to the port at all**.
Verified load-bearing rather than assumed — renaming one method fails the build naming it.

### 46.1 The audit found one real layering violation

`port.ExchangeClient` was genuinely exchange-agnostic and `usecase` depended on it correctly. But
`usecase/affordability_service.go` imported `internal/okx` and held an `okx.SymbolMap` — a use-case
depending on an adapter, which §10's layering exists to prevent. MEXC would have had to fabricate a
redundant symbol map or fork the service.

Fixed with `port.SymbolResolver`, because the mapping is genuinely per-exchange:

- **OKX needs a configured table.** Its real-trading instruments are X-Perp futures whose id embeds
  a rolling expiry (`BTC-USD_UM_XPERP-310404`, §33.4) — not derivable from "BTC", and it changes
  when OKX rolls the contract.
- **MEXC needs none.** Its perpetuals are plain `BTC_USDT` (verified live). A table there would be
  ceremony: config lines restating a rule, each one a chance to typo an instrument into silence.

`port.IdentitySymbolResolver` covers the derivable case. This is exactly the operator's instruction
applied: OKX's contract-naming problem stays OKX's.

**`TestUsecaseImportsNoExchangeAdapter`** now parses the package's imports at AST level and fails if
a use-case imports any adapter. §10 stated this rule in prose for months while it was already
broken — §16.10's own lesson is that a documented property needs a check that would fail if it were
absent.

### 46.2 What differs between the exchanges, and where each difference lives

Every one of these is handled **inside `internal/mexc`**, never pushed into shared code:

| Concern | OKX | MEXC |
|---|---|---|
| Symbol | `BTC-USD_UM_XPERP-310404`, rolling expiry | `BTC_USDT`, stable |
| Symbol mapping | configured table required | identity |
| Auth | key+secret+**passphrase** | key+secret, no passphrase |
| Response envelope | `{code:"0"}` — code is a STRING | `{success, code:0}` — a NUMBER |
| Klines | array of row-arrays | **parallel column arrays** |
| Kline timestamps | ms | **seconds** (funding is ms — the two endpoints disagree) |
| Prices | strings | JSON numbers |
| Candle close | explicit `confirm=1` | **no flag at all** |
| WS ack vs push | separate `event` field | channel prefix (`rs.` vs `push.`) |
| WS ping | plain text `"ping"` | JSON `{"method":"ping"}` |
| Sockets | public + separate business host | one socket |
| Order side | side + posSide | **one integer** encoding both |
| SL/TP | separate OCO algo order, has algoId | attached to a **position**, no algoId |

### 46.3 The subtle one: MEXC never says a candle closed

OKX marks a finalized bar with `confirm=1`. MEXC re-pushes the forming bar continuously with **no
equivalent field**. There is nothing to read, so closure must be inferred: a push for a newer bar
means the tracked one is complete.

`ws.KlineFinalizer` does that, and deliberately does NOT hide it inside the decoder — finalization
is per-subscription state, and a stateful decoder would make two subscriptions to one symbol
interfere. Keyed by (symbol, interval) because one process runs several instruments across several
timeframes on one connection (§9), and a shared key would let a 5m push finalize a 1H candle.

Treating every push as closed would re-run strategies several times per second on an unfinished
candle, which §14 is explicit is wrong.

**A weak test nearly let a real bug through.** The first independence test used the same bar
timestamp for every stream, which made a shared-key implementation behave identically to a correct
one — it passed against the very bug it was written to catch. Found by tracing both versions rather
than trusting the green result, then rewritten with interleaved bar times (the real case: a 1H and a
5m bar have different open times). It now fails with "a 5m push corrupted the 1H stream".

### 46.4 The gateway generalizes; it did not need forking

The operator asked directly whether one generalized gateway really works for every exchange. It
does, and the reason is that the service was already generic and only looked OKX-specific:

- Its routes are `/ticker` and `/order` — **not** `/api/v5/market/tickers`.
- Handlers call `s.client.GetTicker()` through an **interface** and return `domain.Ticker`.
- Every OKX mention in `internal/gateway`'s limiter and retry is a **comment**, not code.

So the gateway never knew it was calling OKX. It knows nine ACTIONS; translating those into a real
URL is the adapter's job. The single exchange-specific piece was `isRetryableOKXError`, hardcoded in
the call path — now a field, defaulting to OKX's so existing wiring is byte-identical.

Proved rather than claimed, since the opposite reading is plausible (the package is *named*
okx-gateway): `TestGateway_ServesANonOKXClient` runs the real service with a MEXC client and serves
a `BTC_USDT` ticker end to end. `TestGateway_UsesTheConfiguredRetryPredicate` guards the piece that
would silently regress — under OKX's predicate a MEXC rate-limit (code 510) is unrecognised and
tried once instead of three times, so the gateway would look healthy while giving up on exactly the
errors retrying exists for.

**Keeping one gateway also keeps the property §27.1 was built for**: credentials live in exactly one
process, no matter how many exchanges are added.

### 46.5 Fail-safe defaults, all tested

- An unknown margin mode maps to **isolated**, never cross — isolated bounds a liquidation to one
  position's margin (§27.2).
- An unmapped order state **errors** rather than defaulting to "live", which would make fill-timeout
  wait out its full timeout on a dead order.
- An ambiguous triggered stop reports **no** `ActualSide` rather than guessing; §37's fallback beats
  a fabricated close reason.
- Fees are **negated** at the boundary: MEXC reports charges positive, the domain expects negative,
  and §41 shows a sign error there credits the fee and overstates every trade by twice its cost.
- MEXC's retry predicate matches **structured codes**, never message text (§38.1).

### 46.6 Verified live, and what is still open

Public endpoints and both WebSocket channels were verified against the real MEXC API — ticker,
klines (column-array decode, seconds timestamps, OHLC coherence), contract specs
(`ctVal=0.0001`, `tickSz=0.1` — the values order sizing depends on, §33.3), and funding history
(reversed to oldest-first). The live kline test asserts **0 finalizations in a 20s window**, since
every push there belongs to one forming bar.

**Still open, by design** — the authenticated half (orders, positions, balance, exchange-side SL/TP)
is written and unit-tested but has never run against a real account, because that needs credentials.
Per the operator's rollout: WebSocket and public data first, API keys after. The numeric-code
mappings (order states, stop-order states) are the most likely to need correction, and are written
to fail loudly on an unknown value rather than guess. Nothing is wired into `cmd/` yet either — no
ingestor or trader runs against MEXC, so none of this can affect the live OKX path.

## 47. One unbuildable strategy row killed real trading for eight hours (2026-09-13)

Reported as a panel error when pausing real trading:

```
trader unreachable: Post "http://trader:8095/restart":
dial tcp: lookup trader on 127.0.0.11:53: no such host
```

**The DNS failure was a symptom, not the cause.** `cmd/trader` had been crash-looping since 20:18
the previous evening, and Docker's internal DNS cannot resolve a container that is not running. The
same report included "the max columns don't update", which turned out to be a *second*, independent
symptom of the same outage plus a third problem underneath it.

### 47.1 Cause

§45's V2 activation `INSERT` cross-joined every `(inst_id, bar)` pair present in
`strategy_assignments` — which includes **both modes**, not just paper. 120 V2 rows landed in
`real`. That work was reported as "paper only"; it was not, and the claim was wrong rather than
merely imprecise.

Those kinds then reached real's `paper_trading_config.active_kinds`, and `cmd/trader`'s deployed
binary predates the V2 strategies. `strategy.FromConfig` returned `unknown strategy kind
"bb_squeeze_breakout_v2"`, `loadRealTraderStrategyAssignments` returned that error, and `main`
called `os.Exit(1)`. On every restart. Forever.

### 47.2 The design flaw is the real finding

**One unbuildable strategy row aborted loading ALL assignments and killed a service managing real
capital.** A single stale database row — trivially recoverable, affecting one of fifteen strategies
— became a total outage with an open real position that had a pending manual-close request nothing
was alive to execute.

Both loaders now **skip** an unusable assignment and continue, logging at ERROR per row plus a
summary count. A strategy that cannot be built simply does not trade, which is the same outcome as
it being disabled — reached without an outage. Skipping silently would be its own failure, so the
log names the assignment id to fix.

`buildAssignmentStrategy` was extracted so the decision is testable without a live Postgres. The
regression test uses the real kind from this incident, so it fails for the same reason production
did rather than an approximation. A third test asserts **every** V2 kind is buildable by this
binary, so dropping one from the registry while assignments reference it fails at build time rather
than at 4am on a live account. Mutation-checked.

### 47.3 What went right, and it is worth stating

The open ZEC position stayed **protected on the exchange the entire eight hours** — `State: live`,
SL 1131.34 / TP 1157.6 — because §35 moved SL/TP onto the exchange rather than leaving it in this
process's tick monitor. That design was built for exactly this: "a crash, a deploy, an OOM, a
stalled Kafka consumer, or a network partition leaves real capital with no stop otherwise." It held.
The position closed profitably (+$0.3436) the moment the trader recovered, executing the operator's
pending request with the exchange's own figures (§41).

### 47.4 The third problem: a million-message Kafka backlog

After the restart the max columns *still* did not update. `pnl_max_pct` is written from the TICK
path, and the trader was consuming none: it was replaying an eight-hour backlog and discarding every
message as stale (`age=5h42m max=2m`). Consumer lag on `okx.tickers` measured **998,330 messages**,
draining at roughly one per second.

Every one of those messages was being dropped anyway, so resetting to the live end lost nothing:

```
docker stop okxbot-trader-1                      # a group member blocks the reset
kafka-consumer-groups.sh --group trader --reset-offsets --to-latest --all-topics --execute
```

`pnl_max_pct` went from 2.65% (the ~13 minutes recorded before the crash) to 15.54%, matching the
exchange's live +14.27%. **Worth knowing generally: any service down for hours will come back into a
stale backlog, and "it restarted successfully" does not mean it is processing live data.** Check
consumer lag, not just container status.

### 47.5 Why the max column looked broken but was not

`pnl_max_pct` is a monotonic high-water mark (`GREATEST(pnl_max_pct, $2)`), so it only moves when a
new peak is reached. Seeing the same value across two checks is correct behaviour, not a stall. What
was genuinely wrong was upstream: for eight hours nothing was writing to it at all.

## 48. Opening a position halted all real trading (2026-09-13)

Reported as real trading being down again, shortly after enabling all twelve V2 strategies for real
mode. Unlike §47 the service was **UP** — it had halted itself, and stayed halted for two hours
across every instrument.

```
WARN real trading halted, skipping new opens instId=BTC
     reason="reconcile: untracked open position on DOGE (exchange reports 45 short)"
```

### 48.1 It was not drift — it was a race with this system's own open

```
05:25:05.661  ERROR reconcile: exchange reports an open position this system has no record of
05:25:06.675  INFO  opened real order id=129 instId=DOGE
```

The order was **mid-flight**. `Exchange.PlaceOrder` is a blocking network call and the exchange
fills the position *during* it, so there is an unavoidable window — measured at ~1s here — where the
position exists remotely with no local row. The insert already happens as early as it possibly can
(immediately after `PlaceOrder` returns, a deliberate 2026-09-04 change so the panel shows the order
as `pending` for the in-flight window), and that is still too late, because the fill precedes the
return.

**The private WebSocket (§35.4) is what turned a rare race into a reliable one.** The fill itself
pushes an account-change event, which triggers a reconcile pass *immediately* — landing inside the
very window the fill opened. Before that socket existed, a 5s poll would usually miss a 1s gap.

### 48.2 The fix

`RealTrader.openInFlight` is set across the open sequence, under the `openMu` the open path already
holds, and reconcile defers the untracked-position halt while it is set.

Deferring is safe rather than a hole in the check: the open path always writes its row (or logs
loudly if it cannot), and the next pass — 5s later, or immediately on the next pushed event —
re-evaluates with the row present. A genuinely untracked position still halts, one pass later.

`shouldDeferUntrackedHalt` is extracted so the decision is testable without a repository or a live
exchange, and the regression test exercises the real predicate rather than a seam invented for the
test. Mutation-checked in **both** directions, which matters for a guard like this: removing it
reproduces the outage, making it unconditional lets genuine drift through unnoticed. Plus a `-race`
test for the flag, since the open path writes it on one goroutine while reconcile reads it on
another.

### 48.3 Two things that made a one-second race cost two hours

Worth recording separately, because neither is fixed by the above:

1. **`risk.Manager.Reset()` has no production caller.** Once halted, the only recovery is a process
   restart. A transient condition therefore latches permanently — the DOGE position that triggered
   this closed at 07:10, and trading was still halted at 07:25 with the exchange reporting flat and
   zero open rows. Nothing re-evaluates a halt.
2. **The halt is global across instruments.** A race on DOGE stopped BTC, ETH and eight others from
   trading. That is the right default for a margin-mode mismatch or a drawdown breach, but it makes
   a per-instrument false positive expensive.

Both deserve their own change: a halt that re-evaluates its own condition, or at minimum a panel
control to clear one after review.

### 48.4 What to check when real trading looks "down"

The last two incidents had the same symptom and completely different causes, so the order matters:

1. `docker ps` — is the container actually **restarting** (§47's crash loop) or **up** (this)?
2. If up, grep for `halted` — a self-halt looks identical to being down from the panel's side.
3. Check the exchange against the database. Both flat while halted means the halt is stale.
4. Check Kafka consumer lag — "restarted successfully" does not mean "processing live data" (§47.4).

## 49. Service health in the panel, and a halt reset gated on evidence (2026-09-13)

Prompted by §47 and §48 happening two days apart: two outages that looked **identical** from the
panel and needed **opposite** responses.

| | §47 | §48 |
|---|---|---|
| Container | **restarting** (crash loop) | **running** |
| Cause | unbuildable strategy row → `os.Exit(1)` | self-halted on a false drift signal |
| Panel showed | `dial tcp: lookup trader … no such host` | nothing — positions just stopped opening |
| Fix needed | code fix + rebuild | reset |

Telling them apart meant SSHing to the server both times.

### 49.1 What was built

`GET /api/health` reports every tracked service's Docker state **verbatim** — `restarting` is
deliberately not flattened into a generic "down", because that is precisely the crash-loop signal
separating the two cases. Plus the real-trading halt with the evidence behind it.

Shown on the Resources tab, above the Grafana link: when something is wrong this is what is being
looked for, and burying it under the least urgent content would be backwards.

### 49.2 The reset is gated on evidence, not a dialog

The operator's own sequencing: *know a problem occurred → understand why → reset only if safe.* A
confirmation dialog does not achieve that — it asks the operator to guess exactly when they have the
least information.

So `cmd/api` **re-derives the halt condition** from the same two sources the trader's own reconcile
compares — the exchange's open positions and this system's open rows — rather than reading a flag.
That answers the stronger question ("is the situation actually resolved") rather than ("does the
trader still have a flag set"), which is what §48 needed: the DOGE position that caused that halt
closed at 07:10 while trading stayed halted until 07:25 with both sides flat.

**Fails closed throughout.** An unreachable exchange, an unreadable database, or no exchange wired
all report "cannot verify" and keep the button locked. Unknown is not safe, and an unreachable
exchange is exactly when a stale reset would be most dangerous. Both fail-open mutations are checked.

The reset itself restarts `cmd/trader`: `risk.Manager`'s flag is in that process's memory with no
cross-process API, and its startup re-derives state from the exchange and database anyway — so a
restart both clears the flag and rebuilds the state it should have.

### 49.3 Why not Grafana

Considered, and it does not fit. Prometheus has 13 `okxbot` metrics and **none of them cover the
halt** — there is no metric to chart. More fundamentally, a halt is a *state with a reason*, not a
number, and "why did trading stop and is it safe to resume" is not a time series.

Embedding remains possible for historical CPU/RAM charts: Grafana currently sends
`X-Frame-Options: deny`, which `GF_SECURITY_ALLOW_EMBEDDING=true` plus anonymous auth would lift —
safe behind the VPN (§11), and the same no-auth posture the panel already has.

### 49.4 A latent gap this closed

`procstatus.go` shells out to a `docker` binary that `Dockerfile.api` never installed (§18 recorded
this and left it). Every one of those calls has silently failed since it was written. This dials the
Engine API over the already-mounted socket instead — read-only by construction, since that socket is
effectively root on the host.

### 49.4b `docker compose restart` does NOT pick up a newly built image

The panel showed none of this after a hard refresh, and the code was fine — the deploy was wrong.

`docker compose restart panel` restarts the EXISTING container with the image it already has. It
never adopts a newly built one. The image had the new bundle (`index-CStebppL.js`, containing
`safeToReset`), while the running container still served the old `index-1UAqHCKd.js` from image
`009617c5…` — a different image id from the freshly built `cbc65404…`.

This is a trap specific to this project because §18.2 established `docker compose restart panel` as
the fix for nginx caching a redeployed upstream's IP. That rule is still right, and it is NOT a
substitute for recreating the container when the panel's own image changed.

**The rule:**

| What changed | Command |
|---|---|
| Only an upstream (`api`, `rl-service`) redeployed | `docker compose restart panel` (§18.2, re-resolves DNS) |
| The panel's own source | `docker compose up -d panel` (recreates on the new image) |

Verify by comparing image ids, not by whether the container restarted:

```
docker inspect okxbot-panel-1 --format '{{.Image}}'   # what is running
docker images okxbot-panel --format '{{.ID}}'          # what was just built
```

A restarted container with an unchanged uptime-since-restart looks identical either way, which is
exactly why "it restarted successfully" is not evidence the new code is live (§47.4's lesson, in a
different disguise).

### 49.5 Two bugs that only the live deploy could find

Worth recording because both would have passed any test written from the same assumptions as the
code:

1. **`haltStatus` read the wrong table.** It called `Repo.ListPositions`, which reads `paper_orders`;
   real orders have had their own table since §34. Against production it reported "2 exchange
   positions, 0 tracked locally" while the database held both — permanent false drift and a
   permanently locked reset button. The gate, inverted.
2. **Docker's `Health` is an OBJECT**, `{"Status":"healthy","FailingStreak":0}`, not the string its
   name suggests. Typing it as a string failed the *whole* response, so every service's state came
   back empty at once. I typed it from a field **list** — and a field list is not a schema.

The second fix then landed in the wrong file (`health.go` instead of `dockerapi.go`, where the
decoding type actually lives), so the redeploy failed identically and had to be traced again.

Both are now pinned by tests using **verbatim bytes from the live Docker socket** rather than
fixtures written from my own assumptions, and the halt stub implements only `ListRealPositions`, so
reading the paper table panics loudly instead of quietly returning an empty slice that looks like
real drift.

## 50. Chart and balance caching in the panel (2026-09-13)

Requested because switching between open positions in the chart, and between the balance chart's
day/week/month ranges, felt slow — data was reloaded from scratch every time.

**Measured before building anything, and the measurement changed the design.** The backend is not
the bottleneck: `/api/candles` answers in **10ms** and `/api/account/history` in **19ms**. A
server-side cache would have solved nothing. The cost was entirely client-side — discarding a result
the browser already had, paying the round trip again, and remounting `CandleChart`, which also threw
away the user's pan/zoom on every switch.

`TokenChartModal` made this explicit: it called `setCandles(null)` whenever `(instId, bar)` changed,
so switching positions blanked the chart to "Loading candles…" by design.

### 50.1 `useCachedResource`

A stale-while-revalidate cache in a module-level `Map` — deliberately module-level, since a cache in
component state would be discarded exactly when the modal closes, which is right before it is
reopened.

- A cached value is returned **synchronously on first render**, so switching back to a recently
  viewed position draws in the same frame with no loading state. Reading in an effect instead would
  paint one empty frame first, which is the flicker this removes.
- Stale data stays on screen **while** revalidating. A chart 30 seconds old beats an empty box.
- `loading` is true only when there is genuinely nothing to show, so a background refresh can never
  blank a view.
- Concurrent callers for one key share a single request rather than racing.

**Two error paths, deliberately different:** a failed *refresh* keeps the stale value (a transient
error must not blank a chart someone is reading), while a failed *first* fetch leaves no entry, so
nothing poisons the cache with an empty value.

### 50.2 The subtle bug avoided

The balance chart's cache key contains a since-timestamp. A raw `Date.now()` there changes on every
render, which would make every lookup miss and the cache silently do nothing — working code, zero
benefit, no error anywhere. Bucketed to the minute so it is stable between renders.

Bounded at 60 entries, oldest-first: 10 instruments × 3 bars is 30, leaving room for balance ranges
and a second exchange (§46) without evicting anything on screen.

### 50.2b Two follow-on gaps, both reported from real use (same day)

The first pass left two holes, and the first is worth recording because it failed in a way that
*looked* like success.

**The balance chart was never actually cached.** Its key contained a bucketed since-timestamp, so a
new key appeared every 60 seconds and any revisit later than that was a guaranteed miss. The cache
worked when flipping ranges quickly and did nothing otherwise — exactly the reported symptom.

Flooring to the hour, tried next, has the *same shape* of bug: a new key whenever an hour boundary
is crossed. A test comparing keys across elapsed time caught that one before it shipped.

The key now carries only the **range**. "The last day of history" is the same question whenever it
is asked, so the key says that and `maxAgeMs` decides when to ask again. The `since` sent to the
server is still computed live inside the fetcher, so a revalidation covers up to the present — **the
key identifies the question, not the instant it was asked.** That is the general rule: a cache key
containing `Date.now()` in any form is a cache that silently does nothing.

**The green/red position zones waited on an uncached fetch.** Paper and Real are separate *routes*,
so switching tabs unmounts `PositionsPage` and `rows` restarts at `null` — until it refills the
chart has no positions and cannot draw its zones. These are the panel's largest payloads (measured:
**210KB** paper, **133KB** real), so the cost is the re-fetch and re-parse, not the ~50ms the server
spends. `listPositions` now uses the same cache, still revalidating every 5s so live PnL is as fresh
as before; the difference is that a cached page renders immediately while that happens.

### 50.3 Verification

`panel/` has **no test framework**, and adding one to prove a ~140-line hook would have been a
larger change than the hook. The cache algorithm was exercised directly instead — de-duplication,
staleness, both error paths, and the size bound — plus a second set checking that keys stay stable
across elapsed time, which is the specific property both §50.2b bugs violated and which neither a
typecheck nor a visual pass would reveal. 14 checks total, and they caught a real defect before it
shipped. Worth adding a real framework if the panel grows more logic of this kind.

Bundle grew 0.7KB.

## 51. A lagging "flat" reading closed a just-opened position and cancelled its stop (2026-09-13)

Reported as the panel's new halt-reset button refusing to work:

```
exchange reports 1 open position(s) but only 0 are tracked locally
— resolve the untracked position before resetting
```

**The gate was right.** It was refusing because a genuinely untracked position existed — which is
exactly what §49.2 built it for. Checked before acting: the exchange held a real PUMP position
(8 contracts long, $29 notional at 9x) with no local row and, worse, **no stop-loss**.

### 51.1 How it happened — the mirror of §48

From the trader's own logs:

```
15:35:02  opened real order id=148 instId=PUMP   (SL rested on the exchange)
15:35:05  reconcile: "exchange reports flat but local state shows an open position; closing locally"
15:35:06  cancelled the resting protective order; closed real order id=148
15:35:24  reconcile: "exchange reports an open position this system has no record of"
```

OKX's positions endpoint trailed its own fill by roughly **20 seconds**. Reconcile believed the
first flat reading, marked the order closed, **and cancelled its protective order** — leaving real
capital running with no stop and no record until it was flattened by hand.

This is the **exact mirror of §48**: there a lagging endpoint made an open position look
*untracked*; here it makes one look *already-closed*. Both are the same underlying fact — the
exchange's position view trails its own fills — and both needed the same answer: distrust it
briefly rather than act on the first reading.

### 51.2 The fix

A position younger than `staleCloseGrace` (60s, against the ~20s lag measured) is **deferred**
rather than closed. Safe because the next pass re-evaluates 5 seconds later, or immediately on a
pushed event; a position genuinely closed outside this system — a liquidation, a manual close in
OKX's app — is still caught, one pass later.

A grace **period** rather than a retry count on purpose: the quantity that actually varies is
elapsed time since the fill, and a count would mean something different on the 5s poll than during
a burst of WebSocket-pushed passes.

Mutation-checked both ways, which matters for a guard that can fail in either direction: removing it
reproduces the incident, making it unconditional leaves the database permanently drifted from the
exchange.

### 51.3 Resolution

Flattened by hand per the operator's decision (chosen over re-protecting it, since closing removes
the risk entirely): filled at 0.003624, realized **-$0.2205** net of a $0.0145 fee. Order 148's row
was updated in place with the exchange's own figures (§41) rather than a new row being invented —
there was only ever one position, and a second row would misrepresent the trade history. Its
`last_error` records what happened, so the anomaly stays legible in the data.

After that the reset button unlocked on its own (`exchange 0 / local 0`), which is the gate behaving
exactly as designed end to end: refuse while a real problem exists, unlock once it is resolved.

### 51.4 What this says about the halt design

Three incidents in two days (§47 crash loop, §48 false-positive halt, §51 real drift) all surfaced
as "real trading isn't working". The §49 health panel now distinguishes them, and this one proved
the gate's value in the direction that matters most: it **refused to clear a halt that was
protecting against a genuine, unprotected real position**. A confirmation dialog would have let it
through.

## 52. The Positions page said "Running" while trading was blocked (2026-09-13)

Reported from real use: the Positions page's status badge showed a green **Running** while the
Resources page had known something was wrong for a long time.

**Both were telling the truth about different things**, which is why neither looked broken:

| | Source | Answers |
|---|---|---|
| Positions badge | `paper_trading_config.trading_state` (Postgres) | "what did the operator ASK for" |
| Resources panel | `GET /api/health` (Docker + exchange) | "what is actually HAPPENING" |

`tradingState` is configured intent. It read "Running" throughout §47 (the trader crash-looping for
eight hours) and §48 (a self-halt lasting two hours), because neither of those touches that column.
The page most likely to be open while wondering *"why are no positions opening"* was the one page
that could not say.

### 52.1 Kept as two facts, not merged into one

Conflating them into a single badge would lose the distinction between "I asked it to run" and "it
is able to run" — both of which matter, and which need different responses. So the badge keeps its
meaning and `TradingHealthBanner` sits beside it, reporting anything that blocks trading with a link
through to Resources where the detail and the reset live.

It reads the **same** `GET /api/health` the Resources page does, so the two can never disagree; a
second source of truth here would only be a new way to drift. Shared through the §50 cache, so
opening Positions costs no extra request.

### 52.2 Silent unless it matters

A banner shown always is a banner nobody reads. It fires for a down **critical** service or a
genuinely blocked halt, and deliberately **not** for:

- a non-critical service — Grafana being down does not stop trading;
- a halt on the **Paper** tab — halts are real-only, and it would be a distraction nobody can act on;
- `safeToReset=false` with **no blockers**, which means "could not verify" (§49.2's fail-closed
  path). Raising the banner on an unreachable exchange would cry wolf on every transient blip.

"restarting" is named as **crash-looping** rather than folded into "not running", since that needs a
fix and a rebuild where a stopped container usually just needs starting.

### 52.3 Verified against a real failure

Not only in logic: `rl-service` was stopped on the server, the endpoint correctly reported it as a
down critical service, and it was restarted immediately (8/8 back). Plus 7 checks on the show/hide
decision covering both incidents and all four cases it must stay quiet for.

## 53. Token discovery, the database-backed roster, and a Home page (2026-09-13)

Requested as one feature — "I want more token options for trading: find the top tokens from every
exchange by volume, change, trend; then a Home page showing balances per exchange, paper/real
service status, and a sortable token table with icons and exchange marks, clickable to a chart."

Building it surfaced a blocker that reshaped the work, and it is the part worth remembering.

### 53.1 The roster had to move to the database first

The scan can find a promising token in seconds. Putting it to work was the problem: the tradeable
roster lived in `config.yaml` as `trading.inst_ids` plus a **hand-maintained** `trading.symbol_map`,
resolved ONCE at each service's startup. A discovered token has neither an entry nor an exec instId,
so nothing could subscribe to it. Reporting candidates a human then copies into YAML was offered and
rejected — correctly, since that is not "more token options", it is a list.

So migration `000031` adds `instruments` (the working set) and `market_tokens` (the ranked whole
market), and `usecase.RosterFor` is what every service now loads instead of config. `exec_inst_id`
replaces `symbol_map` entirely, which is a real improvement independent of discovery: OKX's X-Perp
ids embed a rolling expiry (§33.4) that a YAML map goes stale on silently, while the roster refreshes
it on every scan.

**Three independent enable flags, not one.** `enabled_ingest` / `enabled_paper` / `enabled_real`
encode the operator's own instruction: a discovered token joins the WebSocket subscriptions and paper
trading immediately — that is how it earns a track record — and is added to the real-mode list
**disabled**, for a person to turn on. One combined flag would make discovery and real-capital
exposure the same decision.

**The fallback boundary is deliberate in both directions**, and it is where this could have gone
wrong:

| roster state | behavior | why |
|---|---|---|
| table EMPTY | seed from `config.yaml` | the state of every existing deployment the moment this lands; the deploy must change nothing |
| exists, all rows DISABLED | honored as-is | falling back would resurrect tokens an operator turned off — migration 000030's exact bug |
| `cmd/trader` (real), empty | REFUSE to start | seeding real-money instruments from a config file on a service's own initiative is a different kind of act |

**Restart, don't reload.** `usecase.RosterWatcher` polls for a change and exits the process, letting
the restart policy relaunch it — the same mechanism §18/§22 use. A WS subscription is fixed for the
life of its socket, and a restart re-derives every piece of state from the database, where a partial
in-place reload is exactly how a service ends up half-subscribed with nothing reporting it. It treats
an unreadable **or empty** read as "don't act": a database blip must not restart a healthy trading
service, and restarting into an empty roster would crash-loop.

### 53.2 It is a job, not a service

Per the operator's own reasoning, and worth recording because the instinct here is wrong: this is not
a service. It serves nothing to any other service, runs a few times a day, and only searches for
tokens and hands them to the main services. It lives as a scheduled job inside `cmd/api` (which
already holds the exchange clients and the database). A container would add a deployment unit and a
memory footprint on a 3.9GB box (§35.7) and nothing else. Interval 8h.

The exchange list is `config.Scan.Exchanges`, shaped to move to a table later: everything downstream
reads `usecase.ExchangeSource`, never the config struct, so that move touches one wiring function. An
unknown exchange name is **refused at startup** rather than skipped — a typo would otherwise drop a
whole exchange from discovery while every log looked healthy (§9's precedent).

### 53.3 Scoring, and what it deliberately does not do

`0.55` volume / `0.25` change / `0.20` range, each normalized against the scan's own maximum rather
than an absolute constant ("high volume" only means anything relative to the rest of that day's
market).

Volume dominates because liquidity is the one property that gates whether this bot can trade a token
at all — §33.2's own finding was that the execution venue's thinness, not the signal, was the binding
constraint. Change is taken as an **absolute value**: a token down 30% is as tradeable as one up 30%
for a bot that trades both sides (§9), and signing it would rank the market by direction, which is a
prediction the scan has no business making. Range is the component that survives a token moving hard
both ways and coming back — the day a mean-reversion strategy has the most to work with, and one a
change-only ranking is blind to. A `$1M` 24h floor excludes markets too thin to absorb even this
project's small positions at any score.

`TopN` (20) bounds admission, not ranking: every admitted token costs a WS subscription, a candle
window per timeframe, and a share of the shared account through dynamic sizing (§32.4).

### 53.4 A bug that would have failed silently in production

Verifying `GetAllTickers` against the real API rather than a fixture is what caught it. **OKX returns
`""` — not `"0"`, not a missing field — for every price field of an instrument that has never
traded.** Because `/market/tickers` decodes as one array, that single row
(`TEST002-USD_UM_XPERP-310822`) failed the decode for **all 207 FUTURES instruments** — the instType
real trading executes against. The whole scan returned nothing, from one dead instrument.

Fixed with `okx.LooseDecimal` on a **separate wire type**. The trading-path `Ticker` stays strict on
purpose: there an empty price is a fault, and reading it as zero is how a close gets recorded at the
wrong number (§37). Tolerance is correct only where the alternative is discarding a whole exchange's
market data.

Each adapter also normalizes at its **own** boundary, per the instruction that OKX's peculiarities
must not be imposed on every exchange:

- OKX's `volCcy24h` is **base-currency** volume, multiplied by price here. `vol24h` is a contract
  count — live-verified 8,559,200 vs 855,920 on EDGE-USDT-SWAP, so ranking by it would order the
  market by contract size. Change is derived from `open24h`.
- MEXC reports `amount24` in dollars already ($1.97B for BTC) and `riseFallRate` directly, so its
  lack of an open price costs nothing.

### 53.5 Two tests that passed against the bug they were written for

Both found by mutation, not review, and both worth recording because the failure mode is the same:
asserting a *consequence* that another correct mechanism also produces.

1. **The dated-futures test.** OKX's FUTURES instType carries 179 X-Perp perpetuals alongside 28
   **dated** contracts, and admitting a dated one to a perpetual-futures bot would place real orders
   on an instrument that expires. My test claimed a `ContainsAny("-_")` guard was the defence. It is
   not — the **suffix patterns** reject those ids, and the test passed with the guard removed. Both
   the comment and the test were rewritten to say what is true, and the guard kept with its own test
   for the case it does cover (a too-loose pattern yielding a symbol with a separator in it).
2. **The roster fallback test.** Asserting that the disabled flags *survived* proved nothing, because
   `UpsertInstrument` never overwrites flags anyway — a roster wrongly re-seeded on every read looks
   identical from outside. It now counts writes, and fails with "wrote 2 instrument rows" when the
   `len(all) == 0` guard is removed.

The general rule: when the thing being tested is *redundant work* rather than *wrong data*, asserting
on the data cannot see it (§39 hit this exact shape with the equity-recording test).

### 53.6 Home page

`/` now lands on `/home`; Positions keeps its own URLs so existing bookmarks work.

Balances per exchange, paper and real service cards (equity, balance, open orders, 24h/7d/30d PnL),
and the market table — sortable by symbol, price, change, range, volume or score, with a symbol
filter and a traded-only toggle. Rows open the existing `TokenChartModal`, which already carries its
own token switcher, so the "menu to choose different tokens" is the component already built for it.

Icons load from a public CDN with a deterministic **letter-avatar fallback**, and the fallback is not
decoration: checked against the live roster, **5 of 11 tokens 404** (PEPE, TRUMP, HYPE, PUMP, WIF),
and discovery will surface more no icon set has heard of. A broken-image placeholder on a third of the
table would make the page unreadable. Avatar colors derive from the symbol's own characters, so a
token keeps its color across reloads and sort orders. Exchange marks are letter marks, not brand
logos — a logo is a trademark and two letters carry the same information.

One row per **token** with venues rolled up, since "which tokens look interesting" is a question about
tokens. Volume is **summed** (total tradeable liquidity is what decides whether a token is worth
trading) while price comes from the **deepest** venue — averaging a thin venue against a deep one
produces a number that exists nowhere.

Three pairs of states are kept distinct rather than collapsed, each one somewhere this project has
already been bitten: unconfigured vs. failed vs. a real zero balance (§46.6 — MEXC holds no working
credentials, and `$0.00` for it would be a plausible-looking lie); a live streamed price vs. the
scan's own last price (the ingestor only subscribes to the roster, so untracked tokens have no
stream); and paper-enabled vs. real-enabled per token, which is exactly what a freshly-discovered
token looks like.

### 53.7 Deployed and verified live (2026-09-13)

Not only built — driven end to end against the real server, and that found two genuine bugs plus
three artifacts of my own test.

**Deploy order caused a real bug.** `cmd/api` hosts the scan and restarted first, so the scan
populated `instruments` before the ingestor ever looked at it. The seed rule was "fill the table if it
is EMPTY", which was then unreachable — and the scan's finds are not the configured roster: TRUMP and
PEPE were both below the $1M floor that day, so the next ingestor restart would have silently stopped
collecting two tokens actively being traded. Fixed to seed per MISSING SYMBOL, which is
order-independent. Verified live: the ingestor came up subscribed to 21 tokens — the 10 configured
(TRUMP and PEPE present) plus 11 discovered — across all five timeframes, with ticks and candles
flowing for the new ones within seconds.

**`cmd/ingestor` had no `POSTGRES_DSN`.** It had no database dependency until the roster moved there,
so `docker-compose.yml` never set one and it fell back to `localhost` and crash-looped. The same gap
that left `cmd/trader` silently failing to write its equity timeline for months (§27.7) — caught in
under a minute here only because this service cannot start at all without it, where the trader's
version failed quietly every poll.

**Colour alone is not a label.** The roster's most consequential distinction — is this token enabled
for REAL MONEY — rendered the same word "real" in both states, differing only by CSS class. A headless
pass read both as identical, which is exactly how a glance misreads it. Now "real ✓" vs "real off".

Three things that looked like bugs and were not, worth recording so the next pass does not re-chase
them: `networkidle` never settles on this page (it holds a live-price WebSocket), reading the DOM
immediately after load shows empty stats and zero icon fallbacks (both arrive asynchronously — the
real fallback rate is **49 of 142**, matching the CDN check), and the 502 on `/api/*` was §18.2's
nginx-caches-the-upstream-IP rule, which applies to rebuilding `api` too, not just the panel.

Final state: 16/16 services up, no errors in any log, `market_tokens` ranking sensibly (BTC and ETH
top on volume; FIL at +23.9% and ZCAT with a 61% range surfacing on the change/range terms), and
**40 scanned tokens admitted with `enabled_real = 0` across the board** — the gate that matters.

One consequence to be aware of rather than a defect: paper equity is $11.10 and dynamic sizing is
equity ÷ active tokens (§32.4), so going from 10 to 21 tokens halves each new paper position to
~$0.53. `scan.top_n` (20) is the dial for that.

### 53.8 Paper trading cap raised to $200 (2026-09-13)

Operator decision, after the roster grew from 10 to 21 tokens and made the sizing problem obvious:
paper equity was $11.32 and dynamic sizing is equity ÷ active tokens (§32.4), so each new position was
opening at ~$0.54 — too small to produce a reward signal worth learning from.

**Checked before acting, and two of the alarming numbers were not what they looked like:**

- **The −$51.40 over 30 days spans TWO resets**, not one continuous drawdown (`reset_count` was 2,
  last on 2026-09-04). The §15.7 give-it-another-chance mechanic had already fired twice, so that
  figure is the sum across reloads rather than the depth of a single hole.
- **`completed_trades: 7` against a 5,117-entry replay buffer** looked like the reward path was
  broken — §15.12's exact failure mode, where the model is asked questions and never told how any
  answer turned out. It is not: `rl-service` had restarted 2 hours earlier, and that counter is
  per-process while the buffer is restored from snapshot (§15.11's design working as intended). Worth
  recording because the two numbers will look contradictory again after every restart.

**$200 rather than $500**, deliberately. It gives ~$9.52 per position across 21 tokens (~$95 notional
at 10x), which is close to the $5.67 average the model already has 1,920 trades of experience at — so
the observation's size-ratio feature stays in a distribution it has seen, rather than jumping into one
it has not. §19.1's own lesson applies here: a step change in a feature the model reads is a real cost,
not a free parameter.

**A correction to the reasoning I initially offered the operator**: a bigger cap does NOT buy
proportionally more runway. Losses scale with position size, so at $200 the per-trade loss roughly
doubles too and the account lasts a similar number of days. What the cap buys is *meaningful position
sizes*, not more time — and saying otherwise would have made the change look safer than it is.

**The underlying loss rate is untouched by this**, and is the real open question: 35% win rate
(674/1920) with a −$0.027 average per trade, and every one of the last eight days negative. More
capital per trade will lose it faster, not slower. The two candidate next steps stay open and
independent of this change: cutting `scan.top_n` so fewer tokens share the account, and reviewing the
strategy mix that is producing a 35% win rate.
