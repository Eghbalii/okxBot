ALTER TABLE account_equity DROP CONSTRAINT account_equity_mode_check;
UPDATE account_equity SET mode = 'real' WHERE mode = 'bot';
ALTER TABLE account_equity ADD CONSTRAINT account_equity_mode_check CHECK (mode = ANY (ARRAY['paper','demo','real']));

ALTER TABLE account_equity_history DROP CONSTRAINT account_equity_history_mode_check;
UPDATE account_equity_history SET mode = 'real' WHERE mode = 'bot';
ALTER TABLE account_equity_history ADD CONSTRAINT account_equity_history_mode_check CHECK (mode = ANY (ARRAY['paper','demo','real']));

ALTER TABLE paper_orders DROP CONSTRAINT paper_orders_mode_check;
UPDATE paper_orders SET mode = 'real' WHERE mode = 'bot';
ALTER TABLE paper_orders ADD CONSTRAINT paper_orders_mode_check CHECK (mode = ANY (ARRAY['paper','demo','real']));

ALTER TABLE strategy_assignments DROP CONSTRAINT strategy_assignments_mode_check;
UPDATE strategy_assignments SET mode = 'real' WHERE mode = 'bot';
ALTER TABLE strategy_assignments ADD CONSTRAINT strategy_assignments_mode_check CHECK (mode = ANY (ARRAY['paper','real']));

ALTER TABLE paper_trading_config DROP CONSTRAINT paper_trading_config_mode_check;
UPDATE paper_trading_config SET mode = 'real' WHERE mode = 'bot';
ALTER TABLE paper_trading_config ADD CONSTRAINT paper_trading_config_mode_check CHECK (mode = ANY (ARRAY['paper','real']));

ALTER SEQUENCE IF EXISTS bot_order_adjustments_id_seq RENAME TO real_order_adjustments_id_seq;
ALTER SEQUENCE IF EXISTS bot_orders_id_seq RENAME TO real_orders_id_seq;

ALTER TABLE bot_order_adjustments RENAME CONSTRAINT bot_order_adjustments_source_check TO real_order_adjustments_source_check;
ALTER TABLE bot_order_adjustments RENAME CONSTRAINT bot_order_adjustments_order_id_fkey TO real_order_adjustments_order_id_fkey;
ALTER TABLE bot_order_adjustments RENAME CONSTRAINT bot_order_adjustments_field_check TO real_order_adjustments_field_check;
ALTER TABLE bot_orders RENAME CONSTRAINT bot_orders_strategy_id_fkey TO real_orders_strategy_id_fkey;
ALTER TABLE bot_orders RENAME CONSTRAINT bot_orders_status_check TO real_orders_status_check;
ALTER TABLE bot_orders RENAME CONSTRAINT bot_orders_side_check TO real_orders_side_check;
ALTER TABLE bot_orders RENAME CONSTRAINT bot_orders_close_reason_check TO real_orders_close_reason_check;

ALTER INDEX IF EXISTS bot_order_adjustments_pkey RENAME TO real_order_adjustments_pkey;
ALTER INDEX IF EXISTS idx_bot_order_adjustments_order RENAME TO idx_real_order_adjustments_order;
ALTER INDEX IF EXISTS bot_orders_status_idx RENAME TO real_orders_status_idx;
ALTER INDEX IF EXISTS bot_orders_open_idx RENAME TO real_orders_open_idx;
ALTER INDEX IF EXISTS bot_orders_inst_opened_idx RENAME TO real_orders_inst_opened_idx;
ALTER INDEX IF EXISTS bot_orders_pkey RENAME TO real_orders_pkey;

ALTER TABLE bot_order_adjustments RENAME TO real_order_adjustments;
ALTER TABLE bot_orders RENAME TO real_orders;
