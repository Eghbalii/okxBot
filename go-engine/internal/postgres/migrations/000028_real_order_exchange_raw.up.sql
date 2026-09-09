-- OKX's own order records, stored once rather than re-fetched on every view (2026-09-09 request:
-- "don't call it each time — when the system reads it, save the JSON there").
--
-- The engine already fetches both records as part of the ordinary lifecycle: waitForFill polls the
-- open order until it reaches a terminal state, and the close path does the same for the flatten.
-- At those two moments the exchange's record is final and complete, so capturing it costs nothing
-- extra and every later view is a plain row read instead of a live API call against a rate-limit
-- budget shared with real trading.
--
-- JSONB rather than TEXT: it is genuinely JSON, and JSONB lets a later query reach into a field
-- (say, fee currency or fill timestamp) without parsing every row in application code. Nullable
-- because an order that never filled has no record worth storing, and a lookup that failed must
-- leave the column empty rather than storing a partial or fabricated object.
ALTER TABLE real_orders ADD COLUMN exchange_open_raw JSONB;
ALTER TABLE real_orders ADD COLUMN exchange_close_raw JSONB;

COMMENT ON COLUMN real_orders.exchange_open_raw IS
    'OKX''s full order record for the opening order, captured verbatim when the fill was '
    'confirmed. NULL means it was never captured (order never filled, or the lookup failed).';
