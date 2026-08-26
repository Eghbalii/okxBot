DROP INDEX IF EXISTS idx_paper_orders_mode;
ALTER TABLE paper_orders DROP COLUMN IF EXISTS mode;

DROP INDEX IF EXISTS idx_strategy_assignments_inst_bar;
DROP TABLE IF EXISTS strategy_assignments;

ALTER TABLE strategies DROP COLUMN IF EXISTS is_origin;
