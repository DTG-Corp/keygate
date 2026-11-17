-- Coupon definitions and their product/plan scope. Redemption logic is
-- Plan 1b; the tables exist now because payment_order_discounts and
-- coupon_redemptions carry foreign keys to them.
--
-- coupon_redemptions is created by 20261112_03_payment_core.up.sql, after
-- payment_orders exists: it references both coupons and orders, and this
-- migration runs first so payment_order_discounts can reference coupons.
-- That is the dependency order the spec's §4.9 note calls for.
CREATE TABLE IF NOT EXISTS coupons (
    id                 TEXT PRIMARY KEY,
    code               TEXT NOT NULL UNIQUE,
    description        TEXT NOT NULL DEFAULT '',
    type               TEXT NOT NULL CHECK (type IN ('percent','fixed','trial')),
    value              BIGINT NOT NULL DEFAULT 0 CHECK (value >= 0),
    trial_days         INT NOT NULL DEFAULT 0 CHECK (trial_days >= 0),
    currency           CHAR(3) NULL,
    stack_group        TEXT NOT NULL DEFAULT 'general'
                         CHECK (stack_group IN ('general','campaign','private')),
    stackable          BOOLEAN NOT NULL DEFAULT TRUE,
    priority           INT NOT NULL DEFAULT 100,
    assigned_email     TEXT NOT NULL DEFAULT '',
    max_redemptions    INT NOT NULL DEFAULT 0,   -- 0 = unlimited
    redeemed_count     INT NOT NULL DEFAULT 0,
    per_customer_limit INT NOT NULL DEFAULT 0,   -- 0 = unlimited
    valid_from         TIMESTAMPTZ,
    valid_to           TIMESTAMPTZ,
    active             BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (type <> 'trial' OR trial_days > 0),
    CHECK (type = 'trial' OR value > 0)
);

-- Empty scope set = all products. Otherwise every listed target is covered.
CREATE TABLE IF NOT EXISTS coupon_scopes (
    coupon_id   TEXT NOT NULL REFERENCES coupons(id) ON DELETE CASCADE,
    scope_type  TEXT NOT NULL CHECK (scope_type IN ('product','plan')),
    scope_id    TEXT NOT NULL,
    PRIMARY KEY (coupon_id, scope_type, scope_id)
);
CREATE INDEX IF NOT EXISTS coupon_scopes_target_idx ON coupon_scopes (scope_type, scope_id);
