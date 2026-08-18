# okxBot

RL-driven OKX futures/perpetual-swap trading bot. Go handles realtime market data, order
execution and hard risk limits; Python trains and serves the RL agent that decides position
sizing and leverage.

See [CLAUDE.md](CLAUDE.md) for full architecture, API notes, and roadmap — read it before making
changes.

## Layout

- `go-engine/` — Go module: WebSocket ingestor, OKX REST client, trading engine, risk manager.
- `rl-service/` — Python: Gymnasium environment, PPO training (Stable-Baselines3), FastAPI
  inference server.
- `docker-compose.yml` — Redis + TimescaleDB + both services for local development.

## Quickstart (development, OKX demo trading only)

```bash
cp go-engine/configs/config.example.yaml go-engine/configs/config.yaml
cp rl-service/configs/config.example.yaml rl-service/configs/config.yaml
cp .env.example .env   # fill in OKX demo API key/secret/passphrase

docker compose up -d redis timescaledb

# Go: start the market data ingestor
cd go-engine && go run ./cmd/ingestor

# Python: train (offline, historical data) then serve
cd rl-service && pip install -r requirements.txt
python -m rl_service.train
uvicorn rl_service.serve.api:app --port 8000

# Go: start the trading engine (talks to the RL service + OKX)
cd go-engine && go run ./cmd/trader
```

**Always run against OKX demo trading (`x-simulated-trading: 1`) until the strategy has been
backtested and evaluated.** Live trading with real funds is a config change, not a code change —
treat it with matching caution.
