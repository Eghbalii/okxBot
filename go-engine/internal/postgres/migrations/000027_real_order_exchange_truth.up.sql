-- Real order lifecycle must be confirmed against the exchange at every step, and that confirmation
-- has to be visible to a trader watching the panel (2026-09-08 request, prompted by real order 3:
-- it was recorded closed while nothing had verified the exchange agreed — and it was closed on a
-- stale price, so "we think it's closed" and "OKX thinks it's closed" could easily have diverged).
--
-- Two new lifecycle states, both meaning "a request is in flight with the exchange and we have not
-- yet been told the outcome":
--   opening — the open order has been accepted by OKX but not yet confirmed filled
--   closing — the flattening order has been sent but not yet confirmed filled
-- Neither existed before: an order sat at 'pending' while opening, and during a close it kept
-- whatever status it had, so the panel could not distinguish "closing right now" from "just
-- sitting there".
ALTER TABLE real_orders DROP CONSTRAINT real_orders_status_check;
ALTER TABLE real_orders ADD CONSTRAINT real_orders_status_check
    CHECK (status = ANY (ARRAY['pending','opening','partial','filled','closing','canceled']));

-- The flattening order's own id, so a close can be verified against OKX after the fact exactly the
-- way an open already can via exchange_order_id. Its absence is why order 3's close could not be
-- audited at all: the row said close_reason='tp' and nothing recorded which exchange order (if
-- any) actually did the closing.
ALTER TABLE real_orders ADD COLUMN exchange_close_order_id TEXT;

-- The exchange's OWN numbers for the close, rather than ours. Realized PnL and fees are reported
-- by OKX per position/order; computing them locally from entry/exit prices reproduces a number
-- that can silently disagree with what the account actually moved by (fees, funding, partial
-- fills, and the exact fill price all differ from an idealised calculation). Nullable because a
-- close that never reached the exchange, or one whose report could not be fetched, legitimately
-- has none — a NULL here means "the exchange did not tell us", which must stay distinguishable
-- from a genuine zero.
ALTER TABLE real_orders ADD COLUMN exchange_realized_pnl NUMERIC;
ALTER TABLE real_orders ADD COLUMN exchange_fee NUMERIC;
ALTER TABLE real_orders ADD COLUMN exchange_close_px NUMERIC;

-- The last error the exchange returned for this order, surfaced to the trader as a panel popup so
-- a failed open or a failed close is acted on by a human rather than discovered later in a log.
-- Cleared on the next successful transition, so a stale error never keeps alarming after the
-- condition has resolved.
ALTER TABLE real_orders ADD COLUMN last_error TEXT;
ALTER TABLE real_orders ADD COLUMN last_error_at TIMESTAMPTZ;

COMMENT ON COLUMN real_orders.exchange_realized_pnl IS
    'OKX''s own reported realized PnL for this position. NULL = not reported by the exchange, '
    'which is deliberately distinct from zero. Prefer this over any locally computed value.';
