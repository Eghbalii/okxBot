ALTER TABLE bot_orders DROP CONSTRAINT bot_orders_status_check;
ALTER TABLE bot_orders ADD CONSTRAINT bot_orders_status_check
    CHECK (status = ANY (ARRAY['pending','opening','partial','filled','closing','canceled']));
