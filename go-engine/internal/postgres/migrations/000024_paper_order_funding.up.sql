-- funding_usd is the accrued funding cost/credit deducted from this order's realized_pnl at close
-- (2026-09-06), kept separate from fees_usd (the trading fee) rather than combined into one number
-- — the two have different signs and different causes (a fixed percentage of notional vs. a
-- market-driven rate that can be a CREDIT), and collapsing them would hide which one actually
-- moved a given trade's PnL. Positive = a cost paid (long paid, or short paid at a negative rate);
-- negative = a credit received. NULL for orders closed before this column existed and for
-- still-open positions.
ALTER TABLE paper_orders ADD COLUMN funding_usd NUMERIC;
