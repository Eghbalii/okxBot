-- Splits SL and TP into two separate resting algo orders (2026-09-22 operator instruction).
--
-- WHY: PlaceAlgoOrder's combined OCO form (one conditional order carrying both slTriggerPx and
-- tpTriggerPx) was found silently dropping the TP side on this account's X-Perp instruments —
-- confirmed live: the resting algo order came back with slTriggerPx set and tpTriggerPx="0" even
-- though both were sent and both decoded correctly on the Go side before the request left this
-- process. Every real order this session (including automated ones from cmd/trader) has this
-- shape: protected on the downside, unprotected on the upside, with no error anywhere.
--
-- The fix places SL and TP as two independent algo orders. That reintroduces the exact risk the
-- original combined-OCO design existed to prevent (bot_protection.go's own doc comment): if only
-- one order existed and it triggered, the position closes and OKX auto-cancels nothing else — but
-- with two independent orders, triggering one leaves the other resting on a now-flat account,
-- where (net_mode) it could later fire against an unrelated future position. The reconciliation
-- poll (already-existing 5s cycle, CLAUDE.md §39) is extended to detect this and cancel the
-- stranded side — see ExchangeTPAlgoOrderID's own comment for how the two IDs are used together.
ALTER TABLE bot_orders
    ADD COLUMN IF NOT EXISTS exchange_tp_algo_order_id TEXT;

COMMENT ON COLUMN bot_orders.exchange_algo_order_id IS
    'The resting STOP-LOSS algo order''s OKX-assigned ID (2026-09-22: previously held the combined SL/TP OCO order''s ID before that form was found to silently drop TP on this account''s X-Perp instruments).';
COMMENT ON COLUMN bot_orders.exchange_tp_algo_order_id IS
    'The resting TAKE-PROFIT algo order''s OKX-assigned ID, placed as a SEPARATE order from the stop-loss (exchange_algo_order_id) — 2026-09-22. Nullable: a position can be open with only its SL placed yet (TP is added once fill is confirmed) or with no TP at all.';
