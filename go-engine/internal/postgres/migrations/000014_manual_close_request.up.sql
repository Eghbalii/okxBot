-- Manual close from the panel (2026-08-31 request): the panel is a separate process (cmd/api)
-- from the PaperTrader that actually owns an instrument's Kafka consumer/conductor state and is
-- the only thing that can safely run the real close path (usecase.PaperTrader.closeOrder) — the
-- one place that durably closes the order, reports the terminal call to the model, and updates the
-- account balance, all atomically with respect to the SL/TP touch check. cmd/api cannot run that
-- path itself, so it just flags intent here; PaperTrader.monitorOpenOrders checks this flag on its
-- next tick (same cadence as the SL/TP check) and closes through the normal path at the live
-- price, close_reason='manual'.
ALTER TABLE paper_orders ADD COLUMN IF NOT EXISTS manual_close_requested BOOLEAN NOT NULL DEFAULT false;
