-- The number of contracts actually filled on the exchange, so a flatten can close exactly what was
-- opened rather than re-deriving a size from a price (2026-09-09 incident).
--
-- Re-deriving is what broke: closeRealWith sized the flattening order from the stored margin at the
-- CURRENT price, but the position was opened at the ENTRY price. When price moves in the position's
-- favour the same margin buys fewer contracts, so the flatten under-closed by one every time —
-- BTC closed 1 of 2, ETH 5 of 6, DOGE 20 of 21. Each left a live remainder on OKX while the
-- database recorded a complete close, and the resulting untracked positions halted real trading
-- for over three hours.
--
-- Contracts are what the exchange actually trades in; margin and notional are both derived views
-- of it, and either can drift from the position as price moves. Storing the contract count removes
-- the derivation from the close path entirely.
ALTER TABLE real_orders ADD COLUMN contracts NUMERIC;

COMMENT ON COLUMN real_orders.contracts IS
    'Contracts actually filled on the exchange. The flatten closes exactly this, never a count '
    're-derived from a price. NULL for rows predating this column.';
