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
- Training happens **offline against historical data** (backtest environment). Live inference is
  a frozen policy served over HTTP; periodic re-training happens offline and the model artifact
  is swapped (no online/live learning in v1 — too risky with real money).

## 3. Repository layout

```
okxBot/
├── CLAUDE.md                  # this file
├── README.md                  # human quickstart
├── docker-compose.yml         # redis + timescaledb + go services + python service
├── go-engine/                 # Go module: data ingestion + order execution + risk
│   ├── cmd/
│   │   ├── ingestor/          # connects OKX public WS, publishes ticks/candles to Redis
│   │   └── trader/            # main trading loop: reads state, calls RL service, executes orders
│   ├── internal/
│   │   ├── config/            # env/yaml config loading
│   │   ├── okx/
│   │   │   ├── rest/          # signed REST client: orders, leverage, positions, balance
│   │   │   └── ws/            # public + private websocket clients (reconnect, heartbeat)
│   │   ├── stream/            # Redis pub/sub + stream helpers
│   │   ├── risk/              # hard risk limits (circuit breakers) independent of the RL model
│   │   ├── engine/            # trading loop orchestration
│   │   └── rlclient/          # HTTP client for the Python inference API
│   └── configs/config.example.yaml
├── rl-service/                 # Python: env, training, inference
│   ├── requirements.txt
│   ├── rl_service/
│   │   ├── env/                # Gymnasium OKX futures trading environment
│   │   ├── data/                # historical data loading + feature engineering
│   │   ├── train.py             # SB3 PPO training entrypoint
│   │   └── serve/               # FastAPI inference app
│   └── configs/config.example.yaml
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

## 7. Roadmap / status

- [x] Architecture defined (this doc)
- [ ] Go OKX REST client (auth + trade + account)
- [ ] Go OKX WebSocket ingestor → Redis
- [ ] Go trading engine + risk manager + RL client
- [ ] Python Gymnasium env for OKX futures backtesting
- [ ] Python PPO training pipeline
- [ ] Python FastAPI inference server
- [ ] docker-compose wiring + local end-to-end dry run (demo trading)
- [ ] Backtest evaluation harness / metrics (Sharpe, max DD, win rate) before any live capital

Update the checklist above as work progresses.
