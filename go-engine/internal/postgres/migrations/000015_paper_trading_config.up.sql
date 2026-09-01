-- Panel control box for cmd/paper-trader (2026-09-01 request): a single-row config the operator
-- edits from the panel to pause/stop trading, disable long or short signals, restrict which
-- strategy kinds/timeframes/tokens are active, without editing config.yaml directly. Restart-
-- required to apply — same posture as tester_config (migration 000010/000012), which cmd/paper-
-- trader's own main() reads fresh from Postgres at every start, same crash-recovery pattern as
-- loadStrategyAssignments.
CREATE TABLE IF NOT EXISTS paper_trading_config (
    id INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    trading_state TEXT NOT NULL DEFAULT 'running'
        CHECK (trading_state IN ('running', 'paused', 'stopped')),
    disable_long BOOLEAN NOT NULL DEFAULT false,
    disable_short BOOLEAN NOT NULL DEFAULT false,
    -- NULL/empty = no per-kind restriction; strategy_assignments.enabled is used as-is. Non-empty
    -- bulk-toggles strategy_assignments so only the listed kinds' assignments are enabled.
    active_kinds TEXT[],
    -- NULL/empty = no token disabled. A disabled token's PaperTrader still runs (so its existing
    -- open positions keep closing normally via SL/TP/timeout) but stops opening new ones.
    disabled_inst_ids TEXT[],
    -- NULL/empty = use paper_trading.bars from config.yaml as-is. Non-empty overrides it.
    active_bars TEXT[],
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO paper_trading_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
