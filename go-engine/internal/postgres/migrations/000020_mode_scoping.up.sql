-- Real-trading readiness plan, 2026-09-04: strategy_assignments and paper_trading_config were
-- global — both cmd/paper-trader and cmd/trader read the identical rows, so a strategy tuned or a
-- token disabled for paper trading silently took effect on real trading too. This adds mode
-- scoping to both, so paper and real trading each maintain fully independent configuration.

-- strategy_assignments: add mode, defaulting every existing row to 'paper' (matches every current
-- reader/writer — real trading has no assignments configured yet). The old UNIQUE(strategy_id,
-- inst_id, bar) is replaced with a 4-column version so the same strategy+token+timeframe can be
-- independently assigned under both modes without colliding.
ALTER TABLE strategy_assignments ADD COLUMN mode TEXT NOT NULL DEFAULT 'paper' CHECK (mode IN ('paper', 'real'));

ALTER TABLE strategy_assignments DROP CONSTRAINT IF EXISTS strategy_assignments_strategy_id_inst_id_bar_key;
ALTER TABLE strategy_assignments ADD CONSTRAINT strategy_assignments_strategy_inst_bar_mode_key
    UNIQUE (strategy_id, inst_id, bar, mode);

-- paper_trading_config: convert the id=1 singleton (migration 000015) into one row per mode.
ALTER TABLE paper_trading_config DROP CONSTRAINT IF EXISTS paper_trading_config_pkey;
ALTER TABLE paper_trading_config ADD COLUMN mode TEXT NOT NULL DEFAULT 'paper' CHECK (mode IN ('paper', 'real'));
UPDATE paper_trading_config SET mode = 'paper' WHERE id = 1;
ALTER TABLE paper_trading_config ADD PRIMARY KEY (mode);
ALTER TABLE paper_trading_config DROP COLUMN id;

-- Seed the real row with trading_state='stopped' — a deliberate fail-safe default distinct from
-- paper's existing 'running' default: a fresh real-mode config must never silently default to
-- "running" the moment this migration runs on the server, before an operator has ever reviewed
-- real-mode settings.
INSERT INTO paper_trading_config (mode, trading_state, active_kinds, disabled_inst_ids, active_bars)
VALUES ('real', 'stopped', '{}', '{}', '{}')
ON CONFLICT (mode) DO NOTHING;
