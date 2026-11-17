-- Align the phase-1a schema with spec §4.2, §4.5 and §4.11 after the
-- payment tables shipped in migrations 01–05. Every statement is
-- idempotent so a retry after a partial manual run is safe.

-- §4.2: a provider config starts disabled; enabling it is explicit.
ALTER TABLE payment_provider_configs ALTER COLUMN enabled SET DEFAULT FALSE;

-- §4.5 names the transaction dedupe index payment_transactions_provider_id.
ALTER INDEX IF EXISTS payment_transactions_provider_ref RENAME TO payment_transactions_provider_id;

-- §4.11: a VN upgrade has no Stripe checkout session, so the existing
-- unique index must ignore the '' placeholders instead of colliding on
-- them.
DROP INDEX IF EXISTS idx_license_upgrades_session;
CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_session
    ON license_upgrades (stripe_checkout_session_id)
    WHERE stripe_checkout_session_id <> '';

-- §4.11: a VN renewal is created at most once per payment order, which
-- makes replaying its settlement a no-op.
CREATE UNIQUE INDEX IF NOT EXISTS license_renewals_payment_order_once
    ON license_renewals (payment_order_id)
    WHERE payment_order_id IS NOT NULL;
