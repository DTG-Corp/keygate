-- Phase 1b plan upgrades (billing plan Tasks 8 and 10).
--
-- An immediate upgrade charges the prorated target price less the unused
-- time the customer already paid for. The credit is frozen on the order so
-- settlement, the invoice and an operator reading the order later all see
-- the same number; it is never re-derived from a price list that may have
-- moved since.
ALTER TABLE payment_orders ADD COLUMN IF NOT EXISTS credit_minor BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN payment_orders.credit_minor IS
    'Upgrade credit applied to this order, in minor units; 0 for every order that is not an immediate plan upgrade.';
