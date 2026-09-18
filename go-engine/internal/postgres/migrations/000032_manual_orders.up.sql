-- Manual/discretionary real-money orders get their own table, fully independent of both
-- paper_orders and real_orders, per the operator's own explicit requirement (docs/MANUAL_TRADE_PLAN.md
-- §2.1): a manual order has no strategy signal, no conductor category, and no observation vector,
-- so it must never be picked up by anything that iterates real_orders expecting those things (the
-- RL reward pipeline, StrategyStatsFor, the SL/TP-adjustment A/B comparison,
-- conductor.SignalConductor's update cadence). strategy_id being nullable on real_orders would have
-- made a manual row technically representable there, but every one of those readers would then need
-- auditing for "does a NULL strategy_id row break this" — a clean second table with its own
-- narrower contract is safer than that audit.
--
-- No `mode` column, matching real_orders' own precedent: this table only ever holds real-money
-- manual orders. Paper-mode manual trading is not requested; if wanted later it is a second,
-- identically-shaped table, not a mode column here.
CREATE TABLE IF NOT EXISTS manual_orders (
    id                      BIGSERIAL PRIMARY KEY,
    inst_id                 TEXT NOT NULL,
    exec_inst_id            TEXT NOT NULL DEFAULT '',
    -- Matches real_orders.side's own "buy"/"sell" convention, not PosSide's separate "long"/"short"
    -- vocabulary (domain.OrderRequest.PosSide is a different, hedge-mode-only concept).
    side                    TEXT NOT NULL CHECK (side IN ('buy', 'sell')),
    -- Both order types are in scope from day one (docs/MANUAL_TRADE_PLAN.md §8.1) -- unlike every
    -- other automated order in this codebase, which is market-only.
    order_type              TEXT NOT NULL DEFAULT 'market' CHECK (order_type IN ('market', 'limit')),
    limit_px                NUMERIC,
    -- 'resting' = a limit order accepted by the exchange but not yet filled -- distinct from
    -- 'pending' (this process hasn't finished submitting it yet). Mirrors real_orders' own fill-
    -- lifecycle status column, widened for the limit-order case real_orders has never needed.
    status                  TEXT NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'opening', 'resting', 'partial', 'filled', 'closing', 'closed', 'canceled')),
    entry_px                NUMERIC,
    sl_px                   NUMERIC,
    tp_px                   NUMERIC,
    size                    NUMERIC NOT NULL, -- USD notional requested (§8.2)
    leverage                NUMERIC NOT NULL,
    contracts               NUMERIC,
    -- True when RealTrader already held a protective algo order on this token at open time, so
    -- ManualTrader deliberately did not place a second one (§8.4: a manual order and a strategy
    -- position can share one net exchange position in net mode, and OKX's conditional orders for a
    -- position don't stack cleanly). exchange_algo_order_id stays NULL in that case.
    protected_by_strategy   BOOLEAN NOT NULL DEFAULT false,
    opened_at               TIMESTAMPTZ,
    closed_at               TIMESTAMPTZ,
    close_reason            TEXT CHECK (close_reason IN ('sl', 'tp', 'manual', 'liquidation', 'canceled')),
    close_px                NUMERIC,
    realized_pnl            NUMERIC,
    exchange_order_id       TEXT,
    exchange_algo_order_id  TEXT,
    exchange_close_order_id TEXT,
    exchange_fee            NUMERIC,
    manual_close_requested  BOOLEAN NOT NULL DEFAULT false,
    last_error              TEXT,
    last_error_at           TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS manual_orders_inst_opened_idx ON manual_orders (inst_id, opened_at);
CREATE INDEX IF NOT EXISTS manual_orders_open_idx ON manual_orders (inst_id) WHERE closed_at IS NULL;
CREATE INDEX IF NOT EXISTS manual_orders_status_idx ON manual_orders (status);

-- Exact mirror of real_order_adjustments (migration 000019), FK into manual_orders instead. No
-- `source` column: every adjustment on a manual order is manual by construction.
CREATE TABLE IF NOT EXISTS manual_order_adjustments (
    id BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL REFERENCES manual_orders(id),
    field TEXT NOT NULL CHECK (field IN ('sl', 'tp')),
    old_value NUMERIC,
    new_value NUMERIC,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_manual_order_adjustments_order
    ON manual_order_adjustments (order_id, created_at);

-- The open-order handshake table (docs/MANUAL_TRADE_PLAN.md §2.3, §4): cmd/api writes a row here on
-- POST /api/manual/orders; cmd/trader's ManualTrader is the ONLY thing that ever claims one and
-- calls PlaceOrder for it. This is what keeps "only one process holds credentials and talks to the
-- exchange" (CLAUDE.md §27.1) intact for manual trading, and is what avoids RealTrader's reconcile
-- loop halting real trading on what would otherwise look like an untracked exchange position
-- (CLAUDE.md §48) -- ManualTrader knows about its own order from the moment it claims the intent,
-- before it ever reaches the exchange.
--
-- A separate table from manual_orders itself (rather than writing manual_orders rows with
-- status='pending' directly) keeps "requested" and "actually happened" cleanly separable for audit:
-- an intent that fails validation (bad instrument, leverage too high) never needs a manual_orders
-- row invented for it.
CREATE TABLE IF NOT EXISTS manual_order_intents (
    id              BIGSERIAL PRIMARY KEY,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    inst_id         TEXT NOT NULL,
    side            TEXT NOT NULL CHECK (side IN ('buy', 'sell')),
    order_type      TEXT NOT NULL DEFAULT 'market' CHECK (order_type IN ('market', 'limit')),
    limit_px        NUMERIC,
    size_usd        NUMERIC NOT NULL,
    leverage        NUMERIC NOT NULL,
    sl_px           NUMERIC,
    tp_px           NUMERIC,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'claimed', 'done', 'failed')),
    manual_order_id BIGINT REFERENCES manual_orders(id),
    error           TEXT,
    claimed_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS manual_order_intents_pending_idx
    ON manual_order_intents (requested_at) WHERE status = 'pending';
