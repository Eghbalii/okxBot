-- Separates "Account Balance" (the real, continuous running total, never reset) from "Total
-- Equity" (account_equity.equity_usd, which resets whenever the operator explicitly chooses a new
-- trading cap via SetAccountCap, or automatically on a drain-to-zero) — CLAUDE.md §31.2.
--
-- Before this, equity_usd played both roles at once: it was the only running total tracked, so an
-- operator choosing to "trade with $40 from now on" had no way to express that without losing the
-- real cumulative history entirely. account_balance_usd is that missing continuous ledger: it is
-- updated by exactly the same trade events as equity_usd (same delta, same moment, same
-- transaction) but is NEVER touched by a reset of any kind, real or operator-triggered.
ALTER TABLE account_equity ADD COLUMN account_balance_usd NUMERIC NOT NULL DEFAULT 0;

-- Seed it from the current equity_usd so existing rows don't suddenly report a $0 balance the
-- moment this migration runs — this is the best available approximation of "the real total so
-- far" for a row that predates the column, which is exactly the same lossy backfill any
-- newly-added running total has to make once.
UPDATE account_equity SET account_balance_usd = equity_usd;
