-- Per-customer invoice/buyer details collected by the portal (spec
-- §3.8, §6.4). One row per user; Keygate never needs more, and a
-- missing row means "no saved profile".

CREATE TABLE IF NOT EXISTS customer_billing_profiles (
    user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    buyer      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
