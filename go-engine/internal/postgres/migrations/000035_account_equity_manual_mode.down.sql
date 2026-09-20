DELETE FROM account_equity_history WHERE mode = 'manual';
DELETE FROM account_equity WHERE mode = 'manual';

ALTER TABLE account_equity DROP CONSTRAINT account_equity_mode_check;
ALTER TABLE account_equity ADD CONSTRAINT account_equity_mode_check CHECK (mode = ANY (ARRAY['paper','demo','bot']));

ALTER TABLE account_equity_history DROP CONSTRAINT account_equity_history_mode_check;
ALTER TABLE account_equity_history ADD CONSTRAINT account_equity_history_mode_check CHECK (mode = ANY (ARRAY['paper','demo','bot']));
