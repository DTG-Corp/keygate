ALTER TABLE license_upgrades ALTER COLUMN stripe_checkout_session_id DROP DEFAULT;
ALTER TABLE license_upgrades DROP COLUMN IF EXISTS payment_order_id;
ALTER TABLE license_renewals DROP COLUMN IF EXISTS payment_order_id;
ALTER TABLE products DROP COLUMN IF EXISTS price_includes_tax;
ALTER TABLE products DROP COLUMN IF EXISTS tax_rate_bp;
