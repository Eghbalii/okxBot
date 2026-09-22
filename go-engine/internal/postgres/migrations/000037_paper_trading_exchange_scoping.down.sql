ALTER TABLE strategy_assignments DROP CONSTRAINT strategy_assignments_strategy_inst_bar_mode_exchange_key;
ALTER TABLE strategy_assignments ADD CONSTRAINT strategy_assignments_strategy_inst_bar_mode_key
    UNIQUE (strategy_id, inst_id, bar, mode);
ALTER TABLE strategy_assignments DROP COLUMN exchange;

ALTER TABLE paper_trading_config DROP CONSTRAINT paper_trading_config_pkey;
ALTER TABLE paper_trading_config ADD PRIMARY KEY (mode);
ALTER TABLE paper_trading_config DROP COLUMN exchange;

DROP INDEX IF EXISTS idx_account_equity_history_mode_exchange_created;
ALTER TABLE account_equity_history DROP COLUMN exchange;

ALTER TABLE account_equity DROP CONSTRAINT account_equity_pkey;
ALTER TABLE account_equity ADD PRIMARY KEY (mode);
ALTER TABLE account_equity DROP COLUMN exchange;

DROP INDEX IF EXISTS idx_paper_orders_exchange_inst_open;
ALTER TABLE paper_orders DROP COLUMN exchange;
