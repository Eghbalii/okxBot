-- Real-money orders get their own table, separate from paper_orders (CLAUDE.md, real-trading
-- readiness plan, 2026-09-04). This is a deliberate reversal of §27.3/§27.7's earlier decision to
-- share paper_orders with mode='real' — see the dated CLAUDE.md addendum for why. The concrete
-- driver: a real order now needs a fill-lifecycle `status` (pending/partial/filled/canceled) that
-- has no equivalent concept in paper trading (a paper order is always instantly and fully filled),
-- so bolting it onto paper_orders would mean every paper row carries a column that means nothing
-- for it. `status` is deliberately independent of closed_at/close_reason, which keep their existing
-- meaning (a filled position later closed by SL/TP/manual/timeout) unchanged from paper_orders.
--
-- No `mode` column: every row in this table IS mode='real' by construction — the table itself is
-- the discriminator, not a column value.
--
-- Deliberately excludes parent_order_id/variant (paper-only shadow-fork columns, real trading has
-- no forking per §27.3) but keeps features_json/pnl_max_pct/pnl_min_pct, which are just as useful
-- for auditing/training a real decision as a paper one.
CREATE TABLE IF NOT EXISTS real_orders (
    id              BIGSERIAL PRIMARY KEY,
    inst_id         TEXT NOT NULL,
    strategy_id     BIGINT REFERENCES strategies(id),
    bar             TEXT NOT NULL DEFAULT '',
    side            TEXT NOT NULL CHECK (side IN ('buy', 'sell')),
    entry_px        NUMERIC NOT NULL,
    sl_px           NUMERIC,
    tp_px           NUMERIC,
    size            NUMERIC NOT NULL,
    leverage        NUMERIC NOT NULL,
    opened_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at       TIMESTAMPTZ,
    close_reason    TEXT CHECK (close_reason IN ('sl', 'tp', 'manual', 'timeout', 'rl_early')),
    close_px        NUMERIC,
    realized_pnl    NUMERIC,
    features_json   JSONB,
    -- Fill lifecycle, independent of closed_at/close_reason above. 'pending' is written the
    -- instant an order is accepted by the exchange (before its fill is confirmed) so an in-flight
    -- order is visible on the panel; 'canceled' means the entry never filled before the 60s
    -- timeout (row stays, so a timed-out attempt is still visible, not silently dropped).
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'partial', 'filled', 'canceled')),
    pnl_max_pct     NUMERIC NOT NULL DEFAULT 0,
    pnl_min_pct     NUMERIC NOT NULL DEFAULT 0,
    manual_close_requested BOOLEAN NOT NULL DEFAULT false,
    exchange_order_id       TEXT,
    exchange_algo_order_id  TEXT
);

CREATE INDEX IF NOT EXISTS real_orders_inst_opened_idx ON real_orders (inst_id, opened_at);
CREATE INDEX IF NOT EXISTS real_orders_open_idx ON real_orders (inst_id) WHERE closed_at IS NULL;
CREATE INDEX IF NOT EXISTS real_orders_status_idx ON real_orders (status);

-- Exact mirror of paper_order_adjustments (migration 000016), FK into real_orders instead.
CREATE TABLE IF NOT EXISTS real_order_adjustments (
    id BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL REFERENCES real_orders(id),
    field TEXT NOT NULL CHECK (field IN ('sl', 'tp')),
    old_value NUMERIC,
    new_value NUMERIC,
    source TEXT NOT NULL DEFAULT 'model' CHECK (source IN ('model', 'optimizer', 'manual')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_real_order_adjustments_order
    ON real_order_adjustments (order_id, created_at);
