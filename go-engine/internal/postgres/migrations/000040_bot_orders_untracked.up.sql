-- Untracked exchange positions must be visible on the panel, not just logged and halted
-- (2026-09-28 request, prompted by a real incident: a PUMP position existed on the exchange with
-- zero local record, and reconcile only logged an error and halted risk management — the position
-- itself never reached the panel where the operator could actually see and act on it).
--
-- 'untracked': a position reconcile discovered ALREADY OPEN on the exchange with no corresponding
-- bot_orders row — this system did not open it (or has lost all record of having done so) and does
-- not know its true entry time/reason. It is written so the operator sees it on the Positions page
-- and can decide whether to close it directly on the exchange, exactly as they would any other open
-- position — never silently reconciled away or left invisible.
ALTER TABLE bot_orders DROP CONSTRAINT bot_orders_status_check;
ALTER TABLE bot_orders ADD CONSTRAINT bot_orders_status_check
    CHECK (status = ANY (ARRAY['pending','opening','partial','filled','closing','canceled','untracked']));
