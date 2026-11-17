-- Phase 1b billing engine (spec §3.6). The proration policy lives on
-- the plan; the scan index makes the hourly renewal sweep cheap once
-- there are many subscriptions. Additive; 1a rows get the default.

ALTER TABLE plans
    ADD COLUMN IF NOT EXISTS proration_mode TEXT NOT NULL DEFAULT 'immediate'
        CHECK (proration_mode IN ('immediate','next_cycle'));

CREATE INDEX IF NOT EXISTS subscriptions_billing_scan
    ON subscriptions (current_period_end)
    WHERE status IN ('active','trialing','past_due');
