-- Phase 1b billing constraints (Lead-owned patch P0 of the Module A plan).
--
-- 1. license_upgrades.idx_license_upgrades_session was a NON-partial unique
--    index on stripe_checkout_session_id. VN upgrade rows keep the column at
--    its '' default, so the second VN upgrade collided with the first. Make it
--    partial, like license_renewals already is (20261112_06).
-- 2. A VN upgrade order may be settled only once: one upgrade row per order.
--    license_renewals already has this index; license_upgrades did not.

DROP INDEX IF EXISTS idx_license_upgrades_session;

CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_session
    ON license_upgrades (stripe_checkout_session_id)
    WHERE stripe_checkout_session_id <> '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_payment_order
    ON license_upgrades (payment_order_id)
    WHERE payment_order_id IS NOT NULL;
