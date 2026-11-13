-- Per-plan, per-provider prices. Phase 1 writes provider 'sepay' or
-- 'payos' and keeps external_price_id for a future Stripe adapter.
-- Money is minor units; currency_exponent says how many make one unit.
CREATE TABLE IF NOT EXISTS plan_prices (
    id                TEXT PRIMARY KEY,
    plan_id           TEXT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    provider          TEXT NOT NULL,
    currency          CHAR(3) NOT NULL,
    amount_minor      BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency_exponent SMALLINT NOT NULL DEFAULT 2,
    billing_interval  TEXT NOT NULL DEFAULT '',
    external_price_id TEXT NOT NULL DEFAULT '',
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS plan_prices_unique_active
    ON plan_prices (plan_id, provider, currency, billing_interval) WHERE active;
