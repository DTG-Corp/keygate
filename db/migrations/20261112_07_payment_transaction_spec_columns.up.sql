-- Restores the reconciliation and audit columns spec §4.5 defines for
-- payment_transactions but migration 03 left out. T11's reconciler stores
-- SePay TransferType == "out" rows (refunds and payouts), the unmatched
-- queue displays the bank reference and transfer content, and a manual
-- match records who matched the row and when.
--
-- Also drops the phase-1 provider CHECK on payment_provider_configs. The
-- spec leaves the provider set open (§4.2): an adapter is a row, and a
-- hard-coded list turns every future adapter into a breaking migration.

ALTER TABLE payment_transactions
    ADD COLUMN IF NOT EXISTS attempt_id TEXT NULL REFERENCES payment_attempts(id) ON DELETE SET NULL;

ALTER TABLE payment_transactions
    ADD COLUMN IF NOT EXISTS direction TEXT NOT NULL DEFAULT 'in';

-- The CHECK is separate from ADD COLUMN so a database that already has
-- the column (but not the constraint) still gets it. The default 'in'
-- backfills existing rows, so the constraint can be added immediately.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'payment_transactions'::regclass
          AND conname = 'payment_transactions_direction_check'
    ) THEN
        ALTER TABLE payment_transactions
            ADD CONSTRAINT payment_transactions_direction_check
            CHECK (direction IN ('in', 'out'));
    END IF;
END
$$;

ALTER TABLE payment_transactions ADD COLUMN IF NOT EXISTS provider_account TEXT NOT NULL DEFAULT '';
ALTER TABLE payment_transactions ADD COLUMN IF NOT EXISTS reference TEXT NOT NULL DEFAULT '';
ALTER TABLE payment_transactions ADD COLUMN IF NOT EXISTS content TEXT NOT NULL DEFAULT '';
ALTER TABLE payment_transactions ADD COLUMN IF NOT EXISTS matched_by TEXT NULL REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE payment_transactions ADD COLUMN IF NOT EXISTS matched_at TIMESTAMPTZ NULL;

-- Verified against pg_constraint on the test database: the generated name
-- is payment_provider_configs_provider_check (Postgres names a column
-- CHECK <table>_<column>_check), so the plain DROP is exact.
ALTER TABLE payment_provider_configs
    DROP CONSTRAINT IF EXISTS payment_provider_configs_provider_check;
