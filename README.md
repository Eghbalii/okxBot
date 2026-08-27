# okxBot

RL-driven OKX futures/perpetual-swap trading bot. Go handles realtime market data, order
execution and hard risk limits; Python trains and serves the RL agent that decides position
sizing and leverage.

See [CLAUDE.md](CLAUDE.md) for full architecture, API notes, and roadmap — read it before making
changes.

## Layout

- `go-engine/` — Go module: WebSocket ingestor, OKX REST client, paper-trading engine, strategy
  engine, trading engine, strategy parameter optimizer (`cmd/strategy-optimizer`, CLAUDE.md §16),
  risk manager, Postgres persistence, Prometheus metrics.
- `rl-service/` — Python: Gymnasium environment (dev sanity-check only), PPO training
  (Stable-Baselines3), FastAPI inference server.
- `optimizer-service/` — Python: minimal FastAPI + Optuna sidecar that proposes candidate strategy
  parameter sets for `cmd/strategy-optimizer` (CLAUDE.md §16.2) — no ML model, no torch.
- `docker-compose.yml` — Kafka + Redis + TimescaleDB + Prometheus + Grafana + all Go/Python
  services.

## Monitoring

Prometheus scrapes `/metrics` from the ingestor (`:9101`), paper trader (`:9102`), and the
strategy optimizer (`:9103`) — see
`prometheus.yml`. Metrics include strategy signals, paper orders opened/closed (by SL/TP/manual
reason), open-order count, and cumulative realized PnL (CLAUDE.md §11 has the full list). Grafana
is at `http://localhost:3000` (default admin/admin, per `docker-compose.yml`) with Prometheus
(`http://prometheus:9090`) as a data source — dashboards aren't pre-built yet, add them once
there's real paper-trading data to look at.

## Quickstart (development, OKX demo trading only)

```bash
cp go-engine/configs/config.example.yaml go-engine/configs/config.yaml
cp rl-service/configs/config.example.yaml rl-service/configs/config.yaml
cp .env.example .env   # fill in OKX demo API key/secret/passphrase

docker compose up -d kafka redis timescaledb prometheus grafana

# Go: start the market data ingestor (WS -> Kafka: ticks + candles)
cd go-engine && go run ./cmd/ingestor

# Go: start the Paper Trading Engine (virtual orders, the RL training data source, see CLAUDE.md §8)
cd go-engine && go run ./cmd/paper-trader

# Python: serve inference (training is driven by the paper-trading trade log, see CLAUDE.md §8)
cd rl-service && pip install -r requirements.txt
uvicorn rl_service.serve.api:app --port 8000
python -m rl_service.train   # optional: run once a paper-trading trade log exists

# Go: start the trading engine (talks to the RL service + OKX)
cd go-engine && go run ./cmd/trader

# Optional: strategy parameter optimizer (CLAUDE.md §16) — tunes existing strategies' own
# parameters against real market data, independent of the RL agent. Requires optimizer-service.
cd optimizer-service && pip install -r requirements.txt
uvicorn optimizer_service.api:app --port 8001
cd go-engine && go run ./cmd/strategy-optimizer   # POST /optimize, GET /status on :8091
```

**Always run against OKX demo trading (`x-simulated-trading: 1`) until the strategy has been
backtested and evaluated.** Live trading with real funds is a config change, not a code change —
treat it with matching caution.

## Infrastructure & resource requirements

**No GPU is required.** The RL policy (Stable-Baselines3 PPO, `MlpPolicy`) trains on engineered
feature vectors, not images/sequences — small MLPs train faster on CPU than GPU at this scale,
since GPU transfer overhead dominates for networks this size. Revisit only if the model later
moves to CNN/image inputs (e.g. candlestick chart images) or large transformer feature extractors.

Minimum viable single-node setup for development / early paper-trading (no live capital yet):

| Component                              | CPU        | RAM     | Disk           | GPU |
|-----------------------------------------|-----------|---------|----------------|-----|
| RL training (`rl_service/train.py`)     | 4 vCPU    | 8 GB    | —              | no  |
| RL inference (`rl_service/serve/api.py`)| 1–2 vCPU  | 1–2 GB  | —              | no  |
| go-engine (ingestor + trader + api)     | 1–2 vCPU  | 1 GB    | —              | no  |
| Kafka (event bus, single Kraft-mode broker) | 1 vCPU | 1 GB   | 10 GB+ SSD     | no  |
| Redis (strategy-optimizer trial state)  | 1 vCPU    | 512 MB–1 GB | —          | no  |
| TimescaleDB/Postgres                    | 2 vCPU    | 4 GB    | 50 GB+ SSD     | no  |
| **Total minimum, all-in-one box**       | **8 vCPU**| **16 GB** | **100 GB SSD** | **no** |

Notes:
- 8 vCPU / 16 GB / 100 GB SSD is achievable on a mid-tier cloud VM (e.g. a DigitalOcean/Linode
  8 vCPU–16 GB droplet, or an equivalent AWS/GCP instance) — no specialized hardware needed to get
  started.
- Scale RAM/CPU up if running parallel training environments (`SubprocVecEnv` with `n_envs > 1`)
  for faster wall-clock training, or if storing raw tick-level data for many instruments (prefer
  aggregating to 1s/1m bars for long-term storage; keep raw tick retention short via a Timescale
  retention policy).
- **Kafka is the internal event bus** (ticks, candles, paper-order open/close events) — a
  single-broker Kraft-mode deployment (no separate Zookeeper), which is enough at this project's
  scale; a multi-broker cluster would only make sense at real production traffic volumes this
  project doesn't have. Consumed by `cmd/paper-trader`, `cmd/strategy-optimizer`, and `cmd/api`'s
  WebSocket bridge (which pushes real-time position-open/SL/TP-hit events to the panel — see
  CLAUDE.md §11.4/§12). Worth noting directly: this project started on Redis Streams, which worked
  fine at this scale — Kafka was adopted specifically for the architecture/ops experience it
  demonstrates in a public repository, not because Redis Streams hit a real limit. Redis itself
  stays in the stack for `cmd/strategy-optimizer`'s disposable trial-parameter state (CLAUDE.md
  §16.3), unrelated to the event bus.

### Dependencies to run the services

- **Go 1.23+** (see `go-engine/go.mod`), Kafka (Kraft-mode, `apache/kafka` image), Redis 7+,
  Postgres 16 + TimescaleDB extension (via `docker-compose.yml`).
- **Python 3.11+** recommended (repo is also tested against 3.9); see
  `rl-service/requirements.txt` for the pinned library set (Gymnasium, Stable-Baselines3, PyTorch
  CPU build, FastAPI, pandas/numpy).
- Docker + Docker Compose if running the full stack locally instead of each service natively.
