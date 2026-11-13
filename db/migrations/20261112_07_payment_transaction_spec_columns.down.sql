-- Reverses 07: restores the phase-1 provider CHECK when it can, then
-- drops the spec §4.5 transaction columns in reverse order. Dropping the
-- columns also drops their foreign keys and the direction CHECK.

DO $$
DECLARE
    has_constraint boolean;
    bad_rows       bigint;
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'payment_provider_configs'::regclass
          AND conname = 'payment_provider_configs_provider_check'
    ) INTO has_constraint;
    IF has_constraint THEN
        RETURN;
    END IF;

    SELECT count(*) INTO bad_rows
    FROM payment_provider_configs
    WHERE provider NOT IN ('sepay', 'payos');

    IF bad_rows = 0 THEN
        ALTER TABLE payment_provider_configs
            ADD CONSTRAINT payment_provider_configs_provider_check
            CHECK (provider IN ('sepay', 'payos'));
    ELSE
        RAISE WARNING 'payment_provider_configs has % row(s) outside sepay/payos; provider CHECK not restored', bad_rows;
    END IF;
END
$$;

ALTER TABLE payment_transactions DROP COLUMN IF EXISTS matched_at;
ALTER TABLE payment_transactions DROP COLUMN IF EXISTS matched_by;
ALTER TABLE payment_transactions DROP COLUMN IF EXISTS content;
ALTER TABLE payment_transactions DROP COLUMN IF EXISTS reference;
ALTER TABLE payment_transactions DROP COLUMN IF EXISTS provider_account;
ALTER TABLE payment_transactions DROP COLUMN IF EXISTS direction;
ALTER TABLE payment_transactions DROP COLUMN IF EXISTS attempt_id;
