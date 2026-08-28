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
- `docker-compose.yml` — Kafka + Redis + TimescaleDB + Prometheus + Grafana + Loki/Promtail + all
  Go/Python services.

## Monitoring & logs

Prometheus scrapes `/metrics` from the ingestor (`:9101`), paper trader (`:9102`), and the
strategy optimizer (`:9103`) — see
`prometheus.yml`. Metrics include strategy signals, paper orders opened/closed (by SL/TP/manual
reason), open-order count, and cumulative realized PnL (CLAUDE.md §11 has the full list). Grafana
is at `http://localhost:3000` (default admin/admin, per `docker-compose.yml`) with **both
Prometheus and Loki auto-provisioned as data sources** (`grafana/provisioning/datasources/`) — no
manual setup needed. Dashboards aren't pre-built yet, add them once there's real paper-trading
data to look at.

**Logs** are aggregated into Loki (`loki-config.yml`) by Promtail (`promtail-config.yml`), which
tails every container's Docker `json-file` log off disk directly — not live container discovery,
which was tried first and found (via a real crash while building this) to silently miss any
container that exits before Promtail's next discovery cycle. Query in Grafana's **Explore** tab
against the Loki data source, e.g.:
- `{level="ERROR"}` — every error, across every service, in one query
- `{container_id="<id>"}` — one container's full log (get the id from `docker ps -a`)
- `|= "failed"` — full-text filter across every container's logs, no label needed
Retention is 14 days by default (`loki-config.yml`); logs from non-Go containers (Python services,
Postgres, Redis, Kafka) are captured too, just without a parsed `level` label since they don't
emit `logfmt`. See CLAUDE.md §11.7 for the full design/rationale, including why the discovery-based
approach was replaced.

## Quickstart (development, OKX demo trading only)

```bash
cp go-engine/configs/config.example.yaml go-engine/configs/config.yaml
cp rl-service/configs/config.example.yaml rl-service/configs/config.yaml
cp .env.example .env   # fill in OKX demo API key/secret/passphrase

docker compose up -d kafka redis timescaledb prometheus grafana loki promtail

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
| Loki + Promtail (log aggregation)       | 1 vCPU    | 512 MB–1 GB | 10 GB+ SSD | no  |
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

## VPS deployment

The stack is deployed on a small (2 vCPU / 3.8 GB RAM / 25 GB disk) Ubuntu 24.04 VPS — comfortably
enough per the resource measurements in CLAUDE.md §15.1 (idle full stack + monitoring is ~585 MB
RAM, ~30% of one core). This section is the reproducible procedure for redoing that deployment
(a fresh VPS, a second environment, disaster recovery) — **no credentials are stored here or in
the repo**; treat the steps below as a runbook, not stored secrets.

### 1. SSH access

Add an entry to `~/.ssh/config` (local machine, not committed) pointing at the server, e.g.:

```
Host okx
  HostName <server-ip>
  Port <ssh-port>
  User root
  ServerAliveInterval 30
  ServerAliveCountMax 5
  TCPKeepAlive yes
```

Copy your public key over (one-time, needs the initial root password) so all further access is
key-based, then confirm passwordless login works and disable password auth if desired:

```bash
ssh-copy-id -p <ssh-port> root@<server-ip>
ssh okx 'echo ok'   # should connect with no password prompt
```

### 2. Install Docker

```bash
ssh okx 'curl -fsSL https://get.docker.com | sh'
ssh okx 'docker compose version'   # Compose plugin ships with the convenience script
```

### 3. Ship the code

From the repo root, package only what's tracked in git (this naturally excludes `.env`,
`configs/config.yaml`, model artifacts, and anything else gitignored — see CLAUDE.md §6):

```bash
git archive --format=zip -o /tmp/okxBot.zip HEAD
ssh okx 'mkdir -p /opt/okxBot'
scp /tmp/okxBot.zip okx:/opt/okxBot/okxBot.zip
ssh okx 'cd /opt/okxBot && unzip -oq okxBot.zip && rm okxBot.zip'
```

To push a code change after editing locally, re-run the same three commands — `unzip -o`
overwrites in place. There's no rsync/git-pull wiring on the server; this project intentionally
keeps deploys as an explicit, reviewable step rather than auto-syncing.

### 4. Configuration

Not committed to git (CLAUDE.md §6) — create these on the server from the `.example` templates,
same shape as local dev:

```bash
ssh okx 'cd /opt/okxBot && cp go-engine/configs/config.example.yaml go-engine/configs/config.yaml'
ssh okx 'cd /opt/okxBot && cp rl-service/configs/config.example.yaml rl-service/configs/config.yaml'
ssh okx 'cd /opt/okxBot && cp .env.example .env'
```

Then edit `go-engine/configs/config.yaml` for the instrument roster you actually want live
(`trading.inst_ids`, `ingestion.bars`/`paper_trading.bars`), and `.env` for OKX API credentials
**once you have them** — paper-trading and the RL/optimizer services run with no exchange
credentials at all (`.env`'s `OKX_API_KEY`/`SECRET`/`PASSPHRASE` stay blank); only `cmd/trader`
(OKX demo or real order placement) needs them. Always use OKX **demo trading** keys
(`OKX_SIMULATED_TRADING=1`) until there's a paper-trading track record worth trusting — see
CLAUDE.md §15.6's paper → demo → real progression.

**Keep your local copies of these files in sync by hand** — the server's `config.yaml`/`.env` are
real (gitignored) config, not tracked, so a change made directly on the server (e.g. via `ssh okx`
+ an editor) won't appear back in your local checkout automatically, and vice versa. There's no
sync tooling for this on purpose (CLAUDE.md §6: secrets/local config are deliberately kept out of
git) — if you edit config on the server, mirror the change into your local
`go-engine/configs/config.yaml`/`rl-service/configs/config.yaml` (or vice versa) so the two
environments don't quietly drift apart.

### 5. Bring the stack up

Infra first (so Postgres/Kafka/Redis are ready before anything tries to migrate against them),
then the app services, then monitoring:

```bash
ssh okx 'cd /opt/okxBot && docker compose up -d redis kafka timescaledb'
ssh okx 'cd /opt/okxBot && docker compose up -d --build ingestor paper-trader rl-service optimizer-service strategy-optimizer api'
ssh okx 'cd /opt/okxBot && docker compose up -d prometheus grafana loki promtail'
```

**Known one-time quirks, both harmless and already documented in CLAUDE.md — don't "fix" them:**
- The very first Kafka publish after topics are auto-created can log `Unknown Topic Or Partition`
  a few times before succeeding (CLAUDE.md §12) — self-resolving, not fatal (every publisher logs
  a warning and continues).
- If `paper-trader`, `api`, and `strategy-optimizer` are started in the same `docker compose up`
  invocation, whichever's `Repository.Migrate` call runs first can win a race against the others,
  which then exit 1 on a duplicate-migration-row conflict. Just re-run
  `docker compose up -d api strategy-optimizer` — migrations are already applied, so the retry
  starts clean. (This is a real gap worth fixing in code — a migration lock or `depends_on` +
  healthcheck ordering across app services — not yet done.)
- Building `paper-trader`/`api`/`ingestor`/`strategy-optimizer` in parallel on a 2 vCPU box briefly
  saturates the CPU (load average ~18 during the build) and can knock Kafka's healthcheck to
  `unhealthy` for a few minutes — it recovers on its own once the builds finish; this is a resource
  contention artifact of the build, not a Kafka problem.

`panel` (the React dashboard) and `trader` (live/demo order placement) are **not** started by the
commands above — `trader` needs real OKX demo credentials in `.env` first, and `panel` is optional
until there's a reason to browse the dashboard over the OpenVPN tunnel (CLAUDE.md §11: it's never
meant to be reachable outside that network, so don't publish its port to the open internet). Start
them once ready with `docker compose up -d --build trader panel`.

### 6. Seed candle history (once, before the first warm-start)

A fresh database has no candle history, and warm-start training needs some (CLAUDE.md §17). OKX
serves it directly — no need to wait days for the live ingestor to accumulate it:

```bash
ssh okx 'cd /opt/okxBot && docker compose run --rm paper-trader -backfill'
```

Note the argument goes directly after `paper-trader`, **not** repeated as
`paper-trader /app/paper-trader -backfill` — `docker compose run` appends whatever you pass after
the service name to the image's existing `ENTRYPOINT`, so repeating the binary path there makes
`os.Args[1]` a literal path string instead of a flag and silently falls through to the normal
trading loop instead of backfilling. Takes about 90 seconds per CLAUDE.md §17's own measurement
(1,500 candles per instrument+timeframe pair, paced to stay well inside OKX's rate limit).

### 7. Verify

```bash
ssh okx 'cd /opt/okxBot && docker compose ps'                       # every service Up (kafka: healthy)
ssh okx 'curl -s http://localhost:8000/health'                       # rl-service: model_loaded should be false until trained
ssh okx 'docker exec okxbot-api-1 wget -qO- http://127.0.0.1:8090/api/account'   # api works, but only reachable from inside its own container/network — see below
```

`cmd/api` and `cmd/strategy-optimizer` bind to `127.0.0.1` **inside their containers**
(`api.addr`/`optimizer.addr` in config), which is correct and deliberate (CLAUDE.md §11: no auth
in v1, reachability is meant to be gated by network access, not by the app) — it means
`curl localhost:8090` from the **host** will get a connection reset, which is expected, not a
misconfiguration. Reach them either via `docker exec <container> wget -qO- http://127.0.0.1:<port>/...`
for a quick check, or by putting the host behind an OpenVPN tunnel (CLAUDE.md §11's intended
long-term access model) and rebinding to the VPN-facing interface.

Grafana is reachable directly at `http://<server-ip>:3000` (default `admin`/`admin`, per
`docker-compose.yml`) — both Prometheus and Loki are auto-provisioned as data sources, no manual
setup. Prometheus itself is at `:9090`, Loki at `:3100`.

### 8. Next steps (per CLAUDE.md §14's "NEXT UP" roadmap item 4)

The infrastructure is running, but the RL model is still untrained/no-op (`rl-service`'s
`/health` reports `model_loaded: false`) and every RL-driven behavior
(`rl_sizing`/`rl_sltp_adjust`/`learning_enabled`) is off in `config.yaml` — this is the correct,
safe starting state, not something left unfinished. In order:

1. **Warm-start training** against the backfilled candle history:
   `docker compose run --rm rl-service python -m rl_service.train --warm-start` — produces
   `models/sac_global.zip`, which `rl-service`'s existing volume mount
   (`./rl-service/models:/app/models`) makes visible to the running `rl-service` container
   immediately (restart it, or it'll pick up the file on its next load path — check
   `rl_service/serve/api.py` for whether a restart is required).
2. Enable `serve.learning_enabled: true` in `rl-service/configs/config.yaml` (paper mode only —
   CLAUDE.md §15.11 is explicit this must stay off for real money) and restart `rl-service`.
3. Enable `paper_trading.rl_sizing: true` in `go-engine/configs/config.yaml` and restart
   `paper-trader`.
4. Enable `paper_trading.rl_sltp_adjust: true`, same file, same restart.
5. Leave `paper_trading.rl_early_close` off — it's the one lifecycle action that destroys the
   counterfactual (CLAUDE.md §15.12).

Doing these one at a time, in this order, is deliberate: if the reward curve looks wrong after step
2, it's attributable to `learning_enabled` alone, not tangled up with sizing or SL/TP-adjust
changes happening at the same time.

Separately, whenever OKX demo API credentials are available: fill in `.env`'s `OKX_API_KEY`/
`OKX_API_SECRET`/`OKX_API_PASSPHRASE`, leave `OKX_SIMULATED_TRADING=1`, and bring up `cmd/trader`
(`docker compose up -d --build trader`) for the OKX demo-trading dry run that's still the one open
Phase 2 item in CLAUDE.md §14 requiring a real server to run on.
