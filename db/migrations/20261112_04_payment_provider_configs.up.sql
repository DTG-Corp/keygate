-- Per-provider credentials and options. product_id NULL is the
-- install-wide default; a product row overrides it. Secrets are sealed
-- by internal/crypto before they reach credentials_encrypted or
-- webhook_secret_encrypted and are never returned to a client.
CREATE TABLE IF NOT EXISTS payment_provider_configs (
    id                     TEXT PRIMARY KEY,
    provider               TEXT NOT NULL CHECK (provider IN ('sepay','payos')),
    product_id             TEXT NULL REFERENCES products(id) ON DELETE CASCADE,
    mode                   TEXT NOT NULL CHECK (mode IN ('test','live')),
    enabled                BOOLEAN NOT NULL DEFAULT TRUE,
    config                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    credentials_encrypted  BYTEA NOT NULL DEFAULT ''::bytea,
    webhook_secret_encrypted BYTEA NOT NULL DEFAULT ''::bytea,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS payment_provider_configs_unique
    ON payment_provider_configs (provider, product_id, mode) NULLS NOT DISTINCT;
