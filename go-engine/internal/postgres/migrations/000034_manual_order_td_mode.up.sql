-- Threads a per-request margin mode through manual orders (2026-09-19 Trade page fixes).
--
-- domain.OrderRequest/LeverageChange already carry TdMode/PosSide PER CALL -- OKX accepts/rejects
-- them per-order, so this is a pure per-request choice, not an account-wide exchange call the way
-- position mode (net vs. hedge) is. ManualTrader previously read a single static m.TdMode set once
-- at cmd/trader startup for every manual order regardless of what the operator picked in the
-- panel's own Cross/Isolated pill -- this column is what lets each order actually carry its own
-- choice end to end (intent -> ManualTrader -> the exchange call).
ALTER TABLE manual_order_intents ADD COLUMN IF NOT EXISTS td_mode TEXT NOT NULL DEFAULT 'cross';
ALTER TABLE manual_orders ADD COLUMN IF NOT EXISTS td_mode TEXT NOT NULL DEFAULT 'cross';
