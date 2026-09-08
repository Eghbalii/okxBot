-- Real trading needs two independent numbers the operator can see side by side (2026-09-08
-- request): the exchange's own total balance, and the slice of it this engine is allowed to trade
-- with. Profit and loss accrue to the tradable slice; the untraded remainder is a reserve that
-- stays put until the operator moves the boundary.
--
-- Before this, the boundary lived in config as trading.safe_money_usd — the RESERVE side of the
-- same relationship (tradable = balance - reserve). That is the same arithmetic, but it is the
-- wrong side to type in: the number the operator actually reasons about is "trade with $20", not
-- "hold $20.38 back", and the reserve has to be recomputed by hand every time the balance moves.
--
-- trading_cap_usd stores the TRADABLE side instead, per mode, so the panel writes the number the
-- operator means and the reserve is derived (reserve = balance - cap). NULL means "no cap set" —
-- trade the whole balance, which is the pre-existing behavior for any account that never sets one.
ALTER TABLE account_equity ADD COLUMN trading_cap_usd NUMERIC;

COMMENT ON COLUMN account_equity.trading_cap_usd IS
    'Operator-chosen tradable slice of the real balance; NULL = trade the full balance. '
    'Reserve is derived as account_balance_usd - trading_cap_usd, never stored separately.';

-- A cap change writes its own history point so the panel's chart can explain a step in tradable
-- equity that has no matching move in the total. It is deliberately NOT reason='reset': a reset
-- means "the account was re-baselined" (and both LastResetAt and the chart's default window anchor
-- to it), whereas a cap change re-splits a balance that did not itself move. Reusing 'reset' would
-- make every cap change silently truncate the chart's default window to that moment.
ALTER TABLE account_equity_history DROP CONSTRAINT account_equity_history_reason_check;
ALTER TABLE account_equity_history ADD CONSTRAINT account_equity_history_reason_check
    CHECK (reason = ANY (ARRAY['trade'::text, 'reset'::text, 'seed'::text, 'cap'::text]));
