-- Collapse the in-flight states back onto the nearest pre-existing one before narrowing the
-- constraint, so the down migration cannot fail on rows the up migration legitimately created.
UPDATE real_orders SET status = 'pending' WHERE status = 'opening';
UPDATE real_orders SET status = 'filled' WHERE status = 'closing';

ALTER TABLE real_orders DROP CONSTRAINT real_orders_status_check;
ALTER TABLE real_orders ADD CONSTRAINT real_orders_status_check
    CHECK (status = ANY (ARRAY['pending','partial','filled','canceled']));

ALTER TABLE real_orders DROP COLUMN exchange_close_order_id;
ALTER TABLE real_orders DROP COLUMN exchange_realized_pnl;
ALTER TABLE real_orders DROP COLUMN exchange_fee;
ALTER TABLE real_orders DROP COLUMN exchange_close_px;
ALTER TABLE real_orders DROP COLUMN last_error;
ALTER TABLE real_orders DROP COLUMN last_error_at;
