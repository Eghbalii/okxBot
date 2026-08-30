-- Allow tester_orders.close_reason = 'timeout' (2026-08-30 request): cmd/strategy-tester
-- deliberately has no in-trade update mechanic, but a position can still sit open indefinitely if
-- price never reaches either level. A 6h force-close (paper_trading's own equivalent, CLAUDE.md
-- §15.14, was added the same day for the same reason) needs its own reason value distinct from
-- 'sl'/'tp' so a human reading tester_orders can tell a genuine level touch from a housekeeping
-- close that cut a stale position short.
--
-- Postgres cannot extend a CHECK constraint in place, so it is dropped and recreated. The old name
-- is Postgres's generated default from 000010's inline column CHECK.
ALTER TABLE tester_orders
    DROP CONSTRAINT IF EXISTS tester_orders_close_reason_check;

ALTER TABLE tester_orders
    ADD CONSTRAINT tester_orders_close_reason_check
    CHECK (close_reason IN ('sl', 'tp', 'timeout'));
