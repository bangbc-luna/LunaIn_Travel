-- +goose Up
-- Payment operations: daily reconciliation, review workflow, refund reminders.

CREATE TABLE payment_reconciliations (
    id               uuid PRIMARY KEY,
    run_date         date NOT NULL UNIQUE,
    status           text NOT NULL CHECK (status IN ('ok','mismatch')),
    checked_payments int NOT NULL CHECK (checked_payments >= 0),
    mismatches       jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(mismatches) = 'array'),
    started_at       timestamptz NOT NULL,
    finished_at      timestamptz NOT NULL,
    -- NULL: run by the scheduled job.
    run_by           uuid NULL REFERENCES users (id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Amount of the transfer HandlePaid processed for this event.
ALTER TABLE payment_events ADD COLUMN amount bigint NULL CHECK (amount > 0);

-- Only a positive integer of at most 18 digits is cast (the CASE guards the
-- cast itself: WHERE conditions have no evaluation order); anything else
-- (fraction, zero, negative, huge) stays NULL instead of aborting.
UPDATE payment_events
SET amount = CASE WHEN jsonb_typeof(payload->'data'->'amount') = 'number'
                   AND (payload->'data'->>'amount') ~ '^[1-9][0-9]{0,17}$'
                  THEN (payload->'data'->>'amount')::bigint END
WHERE processed_at IS NOT NULL
  AND result NOT IN ('unknown_order','no_amount','awaiting_status','already_processed')
  AND jsonb_typeof(payload->'data'->'amount') = 'number'
  AND (payload->'data'->>'amount') ~ '^[1-9][0-9]{0,17}$';

CREATE INDEX payment_events_awaiting_idx ON payment_events (result, received_at)
    WHERE result = 'awaiting_status';

-- Review of needs_review payments: an admin first sees it, then resolves it.
ALTER TABLE payments
    ADD COLUMN review_seen_at     timestamptz NULL,
    ADD COLUMN review_seen_by     uuid NULL REFERENCES users (id),
    ADD COLUMN review_resolved_at timestamptz NULL,
    ADD COLUMN review_resolved_by uuid NULL REFERENCES users (id),
    ADD COLUMN review_note        text NULL,
    ADD CONSTRAINT payments_review_seen_check CHECK ((review_seen_at IS NULL) = (review_seen_by IS NULL)),
    ADD CONSTRAINT payments_review_resolved_check CHECK ((review_resolved_at IS NULL) = (review_resolved_by IS NULL)),
    ADD CONSTRAINT payments_review_order_check CHECK (review_resolved_at IS NULL OR review_seen_at IS NOT NULL),
    ADD CONSTRAINT payments_review_status_check CHECK (
        (review_seen_at IS NULL AND review_resolved_at IS NULL) OR status = 'needs_review');

CREATE INDEX payments_review_open_idx ON payments (id)
    WHERE status = 'needs_review' AND review_resolved_at IS NULL;

-- Reminders for refunds still waiting for the customer's bank info.
ALTER TABLE refunds
    ADD COLUMN bank_info_reminder_count int NOT NULL DEFAULT 0 CHECK (bank_info_reminder_count >= 0),
    ADD COLUMN bank_info_reminded_at timestamptz NULL,
    ADD CONSTRAINT refunds_holder_check CHECK (account_holder IS NULL OR account_holder ~ '^[A-Z]+( [A-Z]+)*$');

CREATE INDEX refunds_awaiting_bank_info_idx ON refunds (status, created_at) WHERE status = 'awaiting_bank_info';

ALTER TABLE customer_refund_accounts
    ADD CONSTRAINT customer_refund_accounts_holder_check CHECK (account_holder ~ '^[A-Z]+( [A-Z]+)*$');

-- +goose Down
ALTER TABLE customer_refund_accounts DROP CONSTRAINT customer_refund_accounts_holder_check;
DROP INDEX refunds_awaiting_bank_info_idx;
ALTER TABLE refunds
    DROP CONSTRAINT refunds_holder_check,
    DROP COLUMN bank_info_reminded_at,
    DROP COLUMN bank_info_reminder_count;
DROP INDEX payments_review_open_idx;
ALTER TABLE payments
    DROP CONSTRAINT payments_review_status_check,
    DROP CONSTRAINT payments_review_order_check,
    DROP CONSTRAINT payments_review_resolved_check,
    DROP CONSTRAINT payments_review_seen_check,
    DROP COLUMN review_note,
    DROP COLUMN review_resolved_by,
    DROP COLUMN review_resolved_at,
    DROP COLUMN review_seen_by,
    DROP COLUMN review_seen_at;
DROP INDEX payment_events_awaiting_idx;
ALTER TABLE payment_events DROP COLUMN amount;
DROP TABLE payment_reconciliations;
