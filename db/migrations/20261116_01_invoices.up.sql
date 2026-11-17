-- Invoices and their per-year numbering (spec §4.8). Phase 1b: every
-- paid order gets an invoice record; PDF and e-invoice are produced
-- later in this phase. Additive: 1a never wrote this table.

CREATE TABLE IF NOT EXISTS invoice_counters (
    year     INT PRIMARY KEY,
    last_seq BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS invoices (
    id                 TEXT PRIMARY KEY,
    number             TEXT NOT NULL UNIQUE,
    order_id           TEXT NOT NULL UNIQUE REFERENCES payment_orders(id) ON DELETE RESTRICT,
    license_id         TEXT NULL REFERENCES licenses(id) ON DELETE SET NULL,
    buyer              JSONB NOT NULL DEFAULT '{}'::jsonb,
    items              JSONB NOT NULL DEFAULT '[]'::jsonb,
    subtotal_minor     BIGINT NOT NULL CHECK (subtotal_minor >= 0),
    discount_minor     BIGINT NOT NULL DEFAULT 0 CHECK (discount_minor >= 0),
    tax_minor          BIGINT NOT NULL DEFAULT 0 CHECK (tax_minor >= 0),
    total_minor        BIGINT NOT NULL CHECK (total_minor >= 0),
    currency           CHAR(3) NOT NULL,
    currency_exponent  SMALLINT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('draft','issued','pending_issue','void')),
    pdf_path           TEXT NOT NULL DEFAULT '',
    e_invoice_provider TEXT NOT NULL DEFAULT '',
    e_invoice_ref      TEXT NOT NULL DEFAULT '',
    e_invoice_status   TEXT NOT NULL DEFAULT '',
    issued_at          TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Invoice lists are newest-first; status and licence filters are the
-- two the admin and portal screens actually use.
CREATE INDEX IF NOT EXISTS invoices_created_idx ON invoices (created_at DESC);
CREATE INDEX IF NOT EXISTS invoices_status_idx ON invoices (status);
CREATE INDEX IF NOT EXISTS invoices_license_idx ON invoices (license_id) WHERE license_id IS NOT NULL;
