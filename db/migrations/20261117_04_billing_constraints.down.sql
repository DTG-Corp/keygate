DROP INDEX IF EXISTS idx_license_upgrades_payment_order;

DROP INDEX IF EXISTS idx_license_upgrades_session;
CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_session
    ON license_upgrades (stripe_checkout_session_id);
