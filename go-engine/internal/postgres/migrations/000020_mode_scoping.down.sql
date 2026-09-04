DELETE FROM paper_trading_config WHERE mode = 'real';

ALTER TABLE paper_trading_config DROP CONSTRAINT paper_trading_config_pkey;
ALTER TABLE paper_trading_config ADD COLUMN id INT NOT NULL DEFAULT 1;
ALTER TABLE paper_trading_config ADD CONSTRAINT paper_trading_config_pkey PRIMARY KEY (id);
ALTER TABLE paper_trading_config ADD CONSTRAINT paper_trading_config_id_check CHECK (id = 1);
ALTER TABLE paper_trading_config DROP COLUMN mode;

ALTER TABLE strategy_assignments DROP CONSTRAINT strategy_assignments_strategy_inst_bar_mode_key;
ALTER TABLE strategy_assignments ADD CONSTRAINT strategy_assignments_strategy_id_inst_id_bar_key
    UNIQUE (strategy_id, inst_id, bar);
ALTER TABLE strategy_assignments DROP COLUMN mode;
