-- Reverses 20261112_06. Safe to run after the up on the same database;
-- each step drops before it creates and renames only the post-up name,
-- so running this on a database that is already back at the 05 schema
-- is a no-op.

-- Drop the §4.11 renewal uniqueness.
DROP INDEX IF EXISTS license_renewals_payment_order_once;

-- Restore the plain Stripe session unique index from 20261023.
DROP INDEX IF EXISTS idx_license_upgrades_session;
CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_session
    ON license_upgrades (stripe_checkout_session_id);

-- Restore the pre-06 transaction index name.
ALTER INDEX IF EXISTS payment_transactions_provider_id RENAME TO payment_transactions_provider_ref;

-- Restore the enabled default from migration 04.
ALTER TABLE payment_provider_configs ALTER COLUMN enabled SET DEFAULT TRUE;
