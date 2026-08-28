-- Reverting narrows the allowed set, so any rows already closed as 'rl_early' would violate the
-- restored constraint. They are relabelled 'manual' first — lossy, but a down migration that fails
-- on real data is worse than one that degrades a label.
UPDATE paper_orders SET close_reason = 'manual' WHERE close_reason = 'rl_early';

ALTER TABLE paper_orders
    DROP CONSTRAINT IF EXISTS paper_orders_close_reason_check;

ALTER TABLE paper_orders
    ADD CONSTRAINT paper_orders_close_reason_check
    CHECK (close_reason IN ('sl', 'tp', 'manual', 'timeout'));
