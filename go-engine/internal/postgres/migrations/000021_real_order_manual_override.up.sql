-- manual_override marks a real order whose SL/TP an operator has edited directly via the panel's
-- Update button (handleAdjustPosition). Once set, RealTrader.runUpdates skips the order entirely —
-- the model is never even asked about it, so it can neither move the levels again nor close it
-- early (rl_early_close). Explicit operator request, 2026-09-06: a manual correction must stick,
-- not be silently overwritten or second-guessed by the next model call.
--
-- Paper orders deliberately do NOT get this column: the operator draws a distinction between the
-- two modes here (paper stays fully model-driven), and it may be added later if that changes.
ALTER TABLE real_orders ADD COLUMN manual_override BOOLEAN NOT NULL DEFAULT false;
