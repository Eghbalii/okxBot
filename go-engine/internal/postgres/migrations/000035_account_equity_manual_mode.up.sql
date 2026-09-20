-- Adds 'manual' to account_equity/account_equity_history's mode CHECK constraint, so manual
-- (discretionary) trading gets its own trading-cap bookkeeping the same way bot trading already
-- does — both share the SAME real exchange balance, so each needs its own row to track how much of
-- that shared balance it has been allocated (Account page, 2026-09-20 request).
ALTER TABLE account_equity DROP CONSTRAINT account_equity_mode_check;
ALTER TABLE account_equity ADD CONSTRAINT account_equity_mode_check CHECK (mode = ANY (ARRAY['paper','demo','bot','manual']));

ALTER TABLE account_equity_history DROP CONSTRAINT account_equity_history_mode_check;
ALTER TABLE account_equity_history ADD CONSTRAINT account_equity_history_mode_check CHECK (mode = ANY (ARRAY['paper','demo','bot','manual']));
