-- Allow close_reason = 'rl_early': a position the RL model closed before either level was touched
-- (CLAUDE.md §15.12's early-close capability).
--
-- Recorded as its own reason rather than folded into 'manual' for two reasons. First, 'manual'
-- means an operator acted, and conflating a model decision with a human one makes the paper_orders
-- log unable to answer "what did the model actually do". Second, early closes need to stay
-- comparable against trades that ran to SL/TP — that comparison is the evidence for whether the
-- capability helps at all, the same logic behind the shadow forks (§15.4), and it is impossible if
-- the two are indistinguishable in the data.
--
-- Postgres cannot extend a CHECK constraint in place, so it is dropped and recreated. The old name
-- is Postgres's generated default from 000001's inline column CHECK.
ALTER TABLE paper_orders
    DROP CONSTRAINT IF EXISTS paper_orders_close_reason_check;

ALTER TABLE paper_orders
    ADD CONSTRAINT paper_orders_close_reason_check
    CHECK (close_reason IN ('sl', 'tp', 'manual', 'timeout', 'rl_early'));
