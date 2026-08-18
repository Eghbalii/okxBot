CREATE EXTENSION IF NOT EXISTS timescaledb;

CREATE TABLE IF NOT EXISTS candles (
    inst_id TEXT NOT NULL,
    bar TEXT NOT NULL,
    ts TIMESTAMPTZ NOT NULL,
    open DOUBLE PRECISION NOT NULL,
    high DOUBLE PRECISION NOT NULL,
    low DOUBLE PRECISION NOT NULL,
    close DOUBLE PRECISION NOT NULL,
    volume DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (inst_id, bar, ts)
);
SELECT create_hypertable('candles', 'ts', if_not_exists => TRUE);

CREATE TABLE IF NOT EXISTS strategies (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    inst_ids TEXT[] NOT NULL DEFAULT '{}',
    kind TEXT NOT NULL,
    config JSONB NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    cloned_from INTEGER REFERENCES strategies(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Paper (forward-test/virtual) trades: the core training-data source, see CLAUDE.md §8.
CREATE TABLE IF NOT EXISTS paper_orders (
    id BIGSERIAL PRIMARY KEY,
    inst_id TEXT NOT NULL,
    strategy_id INTEGER REFERENCES strategies(id),
    side TEXT NOT NULL CHECK (side IN ('buy', 'sell')),
    entry_px DOUBLE PRECISION NOT NULL,
    sl_px DOUBLE PRECISION,
    tp_px DOUBLE PRECISION,
    size DOUBLE PRECISION NOT NULL,
    leverage DOUBLE PRECISION NOT NULL DEFAULT 1,
    opened_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at TIMESTAMPTZ,
    close_reason TEXT CHECK (close_reason IN ('sl', 'tp', 'manual', 'timeout')),
    close_px DOUBLE PRECISION,
    realized_pnl DOUBLE PRECISION,
    features_json JSONB
);

CREATE INDEX IF NOT EXISTS idx_paper_orders_inst_opened ON paper_orders (inst_id, opened_at);
CREATE INDEX IF NOT EXISTS idx_paper_orders_open ON paper_orders (inst_id) WHERE closed_at IS NULL;
