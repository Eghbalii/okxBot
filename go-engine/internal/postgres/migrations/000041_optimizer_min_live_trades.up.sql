-- Gates auto-promotion (CLAUDE.md, migration 000039's own auto-promote comment): a new candidate
-- clearing backtest validation must NOT replace a lineage's currently-active candidate until that
-- active candidate has accumulated at least this many REAL closed live paper trades of its own.
-- Explicit operator decision (2026-09-28): a backtest pass alone is not evidence the active
-- candidate is actually worse — until real trading data exists for it, there is no basis for
-- replacing it, and the previous auto-promote behavior (replace the instant a new candidate passes
-- backtest, regardless of how little live data the active one had) is what this closes.
--
-- Default 30, matching min_trades' own existing default and the removed cmd/strategy-tester's own
-- historical MinTradesToScore precedent for "enough live trades to make a real comparison."
ALTER TABLE strategy_optimizer_config
    ADD COLUMN IF NOT EXISTS min_live_trades_before_replace INT NOT NULL DEFAULT 30;
