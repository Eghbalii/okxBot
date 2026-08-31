DROP TABLE IF EXISTS tester_optimizer_state;
ALTER TABLE tester_strategy_versions DROP COLUMN IF EXISTS source;
ALTER TABLE tester_strategy_versions DROP COLUMN IF EXISTS trial_id;
