ALTER TABLE tester_orders
    DROP CONSTRAINT IF EXISTS tester_orders_close_reason_check;

ALTER TABLE tester_orders
    ADD CONSTRAINT tester_orders_close_reason_check
    CHECK (close_reason IN ('sl', 'tp'));
