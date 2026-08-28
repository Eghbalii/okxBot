DROP INDEX IF EXISTS idx_account_equity_history_mode_created;

DROP TABLE IF EXISTS account_equity_history;

DROP TABLE IF EXISTS account_equity;

-- Recreated as it was in 000004 so a down-migration lands on that schema exactly. Balances are not
-- restored (they were dropped by the up-migration); this table reseeds from config on first read,
-- which is how GetTokenBudget always treated a missing row.
CREATE TABLE IF NOT EXISTS token_budgets (
    inst_id TEXT PRIMARY KEY,
    budget_usd DOUBLE PRECISION NOT NULL,
    equity_usd DOUBLE PRECISION NOT NULL,
    reset_count INTEGER NOT NULL DEFAULT 0,
    last_reset_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
