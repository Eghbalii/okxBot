-- Adds an `exchange` dimension to `candles` (2026-09-22), so a second ingestor instance can collect
-- and persist candle data for a different exchange (MEXC) without colliding with OKX's own rows.
--
-- Before this migration, `candles` was keyed by (inst_id, bar, ts) alone. Both exchanges use the
-- same short internal symbols ("BTC", "ETH", CLAUDE.md §27's symbol design), so a second ingestor
-- publishing MEXC's own BTC/5m/<ts> candle would silently UPSERT over OKX's row for that exact
-- key — confirmed this session: a MEXC paper-trader pointed at the existing `candles` table was
-- found reading OKX's own BTC/ETH candle history, not MEXC's, because nothing distinguished them.
--
-- Mirrors the EXISTING pattern migration 000037 established for paper_orders/account_equity/
-- account_equity_history/paper_trading_config/strategy_assignments — DEFAULT 'okx' on the new
-- column is what makes this a zero-behavior-change migration for the existing single-exchange
-- deployment: every pre-existing row, and every existing caller that has not yet been taught about
-- a second exchange, keeps working exactly as before (every existing caller in this codebase pins
-- to 'okx' explicitly in Go, not just via this default — see internal/postgres/candles.go).

ALTER TABLE candles ADD COLUMN exchange TEXT NOT NULL DEFAULT 'okx';

-- Widen the primary key from (inst_id, bar, ts) to (exchange, inst_id, bar, ts). This is the
-- column ON CONFLICT must match exactly — a mismatch here is what turned a similar migration into
-- a crash loop once already (CLAUDE.md §34: a PK-widening migration deployed before the Go code
-- reading it was updated caused an ON CONFLICT SQLSTATE 42P10 crash-loop on cmd/trader). This
-- migration and internal/postgres/candles.go's SaveCandle fix land in the same commit specifically
-- to avoid repeating that.
ALTER TABLE candles DROP CONSTRAINT candles_pkey;
ALTER TABLE candles ADD PRIMARY KEY (exchange, inst_id, bar, ts);
