-- funding_rates stores OKX's own reported funding rate per instrument, polled hourly (2026-09-06).
-- Needed because paper trading's realizedPnL previously used a single config constant
-- (funding_rate_per_8h) that measured 4-5x too small against the real X-Perp instruments this
-- project trades (live-checked against BTC-USD_UM_XPERP: real rate ~-0.021% to -0.027% per 8h,
-- sign NEGATIVE — shorts pay longs on this product, opposite of the standard BTC-USD-SWAP market
-- — versus the configured +0.01%), and real market funding swings roughly 60x between calm and
-- volatile periods (0.005% to 0.3% per 8h), which a fixed constant cannot track either.
CREATE TABLE funding_rates (
    inst_id      TEXT NOT NULL,
    funding_time TIMESTAMPTZ NOT NULL,
    funding_rate NUMERIC NOT NULL,
    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (inst_id, funding_time)
);

-- Closed-position lookups need "every funding_time between opened_at and closed_at for this
-- inst_id" — this index is what makes that a range scan instead of a full-table scan as history
-- accumulates.
CREATE INDEX idx_funding_rates_inst_time ON funding_rates (inst_id, funding_time);
