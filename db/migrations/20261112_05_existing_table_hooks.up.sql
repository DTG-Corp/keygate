-- Hooks on tables that existed before the payment rail. VN upgrades and
-- renewals carry payment_orders.id instead of abusing the Stripe columns,
-- which keep their '' defaults.
ALTER TABLE products ADD COLUMN IF NOT EXISTS tax_rate_bp INT NOT NULL DEFAULT 0;
ALTER TABLE products ADD COLUMN IF NOT EXISTS price_includes_tax BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE license_renewals ADD COLUMN IF NOT EXISTS payment_order_id TEXT NULL REFERENCES payment_orders(id) ON DELETE SET NULL;
ALTER TABLE license_upgrades ADD COLUMN IF NOT EXISTS payment_order_id TEXT NULL REFERENCES payment_orders(id) ON DELETE SET NULL;
ALTER TABLE license_upgrades ALTER COLUMN stripe_checkout_session_id SET DEFAULT '';
