-- Renames the "real"-money automated-strategy trader's tables/mode value to "bot" (the trader was
-- renamed usecase.RealTrader -> usecase.BotTrader for clarity, alongside the pre-existing
-- usecase.PaperTrader and the newer usecase.ManualTrader). This mirrors, exactly, the manual SQL
-- already applied out-of-band against the live production database on 2026-09-19 (that database was
-- migrated directly rather than through a versioned migration, per an explicit operator decision
-- since the project was still in active development) — this migration exists so a FRESH database
-- built from scratch ends up with the identical schema, not so production applies it again.
--
-- Table/constraint/index/sequence names below are exactly what production now has; verify against
-- \d bot_orders / \d bot_order_adjustments on the server before changing this file.

ALTER TABLE real_orders RENAME TO bot_orders;
ALTER TABLE real_order_adjustments RENAME TO bot_order_adjustments;

ALTER INDEX IF EXISTS real_orders_pkey RENAME TO bot_orders_pkey;
ALTER INDEX IF EXISTS real_orders_inst_opened_idx RENAME TO bot_orders_inst_opened_idx;
ALTER INDEX IF EXISTS real_orders_open_idx RENAME TO bot_orders_open_idx;
ALTER INDEX IF EXISTS real_orders_status_idx RENAME TO bot_orders_status_idx;
ALTER INDEX IF EXISTS idx_real_order_adjustments_order RENAME TO idx_bot_order_adjustments_order;
ALTER INDEX IF EXISTS real_order_adjustments_pkey RENAME TO bot_order_adjustments_pkey;

ALTER TABLE bot_orders RENAME CONSTRAINT real_orders_close_reason_check TO bot_orders_close_reason_check;
ALTER TABLE bot_orders RENAME CONSTRAINT real_orders_side_check TO bot_orders_side_check;
ALTER TABLE bot_orders RENAME CONSTRAINT real_orders_status_check TO bot_orders_status_check;
ALTER TABLE bot_orders RENAME CONSTRAINT real_orders_strategy_id_fkey TO bot_orders_strategy_id_fkey;
ALTER TABLE bot_order_adjustments RENAME CONSTRAINT real_order_adjustments_field_check TO bot_order_adjustments_field_check;
ALTER TABLE bot_order_adjustments RENAME CONSTRAINT real_order_adjustments_order_id_fkey TO bot_order_adjustments_order_id_fkey;
ALTER TABLE bot_order_adjustments RENAME CONSTRAINT real_order_adjustments_source_check TO bot_order_adjustments_source_check;

ALTER SEQUENCE IF EXISTS real_orders_id_seq RENAME TO bot_orders_id_seq;
ALTER SEQUENCE IF EXISTS real_order_adjustments_id_seq RENAME TO bot_order_adjustments_id_seq;

-- mode='real' -> mode='bot' across every mode-bearing table. CHECK constraints are dropped before
-- the data UPDATE (the old constraint does not allow 'bot' yet) and recreated after.
ALTER TABLE account_equity DROP CONSTRAINT account_equity_mode_check;
UPDATE account_equity SET mode = 'bot' WHERE mode = 'real';
ALTER TABLE account_equity ADD CONSTRAINT account_equity_mode_check CHECK (mode = ANY (ARRAY['paper','demo','bot']));

ALTER TABLE account_equity_history DROP CONSTRAINT account_equity_history_mode_check;
UPDATE account_equity_history SET mode = 'bot' WHERE mode = 'real';
ALTER TABLE account_equity_history ADD CONSTRAINT account_equity_history_mode_check CHECK (mode = ANY (ARRAY['paper','demo','bot']));

ALTER TABLE paper_orders DROP CONSTRAINT paper_orders_mode_check;
UPDATE paper_orders SET mode = 'bot' WHERE mode = 'real';
ALTER TABLE paper_orders ADD CONSTRAINT paper_orders_mode_check CHECK (mode = ANY (ARRAY['paper','demo','bot']));

ALTER TABLE strategy_assignments DROP CONSTRAINT strategy_assignments_mode_check;
UPDATE strategy_assignments SET mode = 'bot' WHERE mode = 'real';
ALTER TABLE strategy_assignments ADD CONSTRAINT strategy_assignments_mode_check CHECK (mode = ANY (ARRAY['paper','bot']));

ALTER TABLE paper_trading_config DROP CONSTRAINT paper_trading_config_mode_check;
UPDATE paper_trading_config SET mode = 'bot' WHERE mode = 'real';
ALTER TABLE paper_trading_config ADD CONSTRAINT paper_trading_config_mode_check CHECK (mode = ANY (ARRAY['paper','bot']));
