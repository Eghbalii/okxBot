-- fees_usd is the trading fee + funding cost deducted from this order's realized_pnl at close
-- (2026-09-06). Stored explicitly, not just folded silently into realized_pnl, so the panel's
-- closed-positions view can show it as its own column rather than requiring the operator to
-- recompute it from entry/exit prices by hand. NULL for orders closed before this column existed
-- and for still-open positions.
ALTER TABLE paper_orders ADD COLUMN fees_usd NUMERIC;
