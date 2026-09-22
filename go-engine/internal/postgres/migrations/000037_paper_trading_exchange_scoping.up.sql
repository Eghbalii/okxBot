-- Adds an `exchange` dimension to the paper-trading-relevant tables (2026-09-22), so a second,
-- fully independent cmd/paper-trader process can run against a different exchange (MEXC) or a
-- different experiment config, in parallel with the existing OKX instance, with completely
-- separate stats/PnL/account-balance/config — while `mode` stays "paper" for both and every
-- existing mode-based query/whitelist in internal/api is untouched.
--
-- This mirrors the EXISTING pattern migration 000031 already established for the discovery/roster
-- layer (instruments/market_tokens, keyed by (exchange, symbol)) — extended here into the
-- trading/stats layer, which previously had no `exchange` column at all.
--
-- DEFAULT 'okx' on every column below is what makes this a zero-behavior-change migration for the
-- existing single-exchange deployment: every pre-existing row, and every existing caller that has
-- not yet been taught about a second exchange, keeps working exactly as before.

-- paper_orders: exchange is informational + the critical isolation point for ListOpenPaperOrders
-- (CLAUDE.md — two engines must never see each other's open positions even if instID collides,
-- e.g. both exchanges trading a token spelled "BTC").
ALTER TABLE paper_orders ADD COLUMN exchange TEXT NOT NULL DEFAULT 'okx';
CREATE INDEX IF NOT EXISTS idx_paper_orders_exchange_inst_open
    ON paper_orders (exchange, inst_id) WHERE closed_at IS NULL;

-- account_equity: widen the key from `mode` to `(mode, exchange)` so "paper" can hold one balance
-- row for OKX and a completely separate one for MEXC.
ALTER TABLE account_equity ADD COLUMN exchange TEXT NOT NULL DEFAULT 'okx';
ALTER TABLE account_equity DROP CONSTRAINT account_equity_pkey;
ALTER TABLE account_equity ADD PRIMARY KEY (mode, exchange);

-- account_equity_history: same widening, plus an index mirroring the existing mode/created_at one
-- so the panel's per-(mode,exchange) chart query stays index-backed.
ALTER TABLE account_equity_history ADD COLUMN exchange TEXT NOT NULL DEFAULT 'okx';
CREATE INDEX IF NOT EXISTS idx_account_equity_history_mode_exchange_created
    ON account_equity_history (mode, exchange, created_at DESC);

-- paper_trading_config: widen the key from `mode` to `(mode, exchange)` — a second exchange's
-- paper-trader process gets its own pause/stop/direction/kind/token/bar control-box row,
-- independent of the existing OKX one.
ALTER TABLE paper_trading_config ADD COLUMN exchange TEXT NOT NULL DEFAULT 'okx';
ALTER TABLE paper_trading_config DROP CONSTRAINT paper_trading_config_pkey;
ALTER TABLE paper_trading_config ADD PRIMARY KEY (mode, exchange);

-- strategy_assignments: widen the unique constraint so the same strategy+token+timeframe+mode can
-- be independently assigned under two different exchanges without colliding.
ALTER TABLE strategy_assignments ADD COLUMN exchange TEXT NOT NULL DEFAULT 'okx';
ALTER TABLE strategy_assignments DROP CONSTRAINT strategy_assignments_strategy_inst_bar_mode_key;
ALTER TABLE strategy_assignments ADD CONSTRAINT strategy_assignments_strategy_inst_bar_mode_exchange_key
    UNIQUE (strategy_id, inst_id, bar, mode, exchange);
