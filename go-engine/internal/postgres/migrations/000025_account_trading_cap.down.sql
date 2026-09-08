-- Relabel any 'cap' rows before narrowing the constraint back, so the down migration can't fail on
-- data the up migration legitimately created (the same pattern migration 000008 used for rl_early).
UPDATE account_equity_history SET reason = 'reset' WHERE reason = 'cap';
ALTER TABLE account_equity_history DROP CONSTRAINT account_equity_history_reason_check;
ALTER TABLE account_equity_history ADD CONSTRAINT account_equity_history_reason_check
    CHECK (reason = ANY (ARRAY['trade'::text, 'reset'::text, 'seed'::text]));

ALTER TABLE account_equity DROP COLUMN trading_cap_usd;
