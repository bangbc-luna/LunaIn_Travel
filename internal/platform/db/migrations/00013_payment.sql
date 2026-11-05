-- +goose Up
-- Payment schema (DB §7).

-- PayOS orderCode: a system-generated integer, 1-1 with a payment. PayOS caps
-- it at 2^53 - 1 (JavaScript safe integer).
CREATE SEQUENCE payment_order_code_seq AS bigint START 100000 MINVALUE 100000 MAXVALUE 9007199254740991;

CREATE TABLE payments (
    id                  uuid PRIMARY KEY,
    checkout_id         uuid NOT NULL REFERENCES checkouts (id),
    provider            text NOT NULL DEFAULT 'payos' CHECK (provider IN ('payos')),
    provider_order_code bigint NOT NULL UNIQUE DEFAULT nextval('payment_order_code_seq'),
    amount              bigint NOT NULL CHECK (amount > 0),
    -- needs_review: the amount paid did not match (spec §8).
    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending','paid','expired','cancelled','needs_review')),
    -- Filled once the PayOS link is created (outside the reserving tx).
    checkout_url        text NULL,
    qr_code             text NULL,
    expires_at          timestamptz NOT NULL,
    paid_at             timestamptz NULL,
    provider_reference  text NULL,
    raw                 jsonb NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (status <> 'paid' OR paid_at IS NOT NULL),
    -- Target of refunds (payment_id, checkout_id).
    UNIQUE (id, checkout_id)
);

ALTER SEQUENCE payment_order_code_seq OWNED BY payments.provider_order_code;

-- One pending link per checkout.
CREATE UNIQUE INDEX payments_checkout_pending_idx ON payments (checkout_id) WHERE status = 'pending';
CREATE INDEX payments_checkout_idx ON payments (checkout_id);

-- Every webhook received; a repeated webhook is stopped by dedupe_key.
CREATE TABLE payment_events (
    id              uuid PRIMARY KEY,
    payment_id      uuid NULL REFERENCES payments (id),
    provider        text NOT NULL CHECK (provider IN ('payos')),
    dedupe_key      text NOT NULL UNIQUE,
    signature_valid bool NOT NULL,
    payload         jsonb NOT NULL,
    received_at     timestamptz NOT NULL,
    processed_at    timestamptz NULL,
    result          text NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payment_events_payment_idx ON payment_events (payment_id) WHERE payment_id IS NOT NULL;

-- A refund belongs to a payment; booking_id is NULL for payment-level refunds
-- (over-, under- or duplicate payment), which are keyed by the incoming
-- transfer's provider_reference. checkout_id ties the payment and the booking
-- to the same checkout.
CREATE TABLE refunds (
    id                 uuid PRIMARY KEY,
    payment_id         uuid NOT NULL,
    checkout_id        uuid NOT NULL,
    booking_id         uuid NULL,
    -- Reference of the incoming transfer this refund returns.
    provider_reference text NULL,
    amount             bigint NOT NULL CHECK (amount > 0),
    reason             text NOT NULL CHECK (reason IN
                           ('customer_cancel','admin_cancel','late_payment','payment_mismatch','duplicate_payment')),
    status             text NOT NULL CHECK (status IN ('awaiting_bank_info','pending','completed')),
    bank_bin           text NULL CHECK (bank_bin ~ '^[0-9]{6}$'),
    account_number     text NULL CHECK (account_number ~ '^[0-9]{6,19}$'),
    account_holder     text NULL CHECK (btrim(account_holder) <> ''),
    transfer_reference text NULL,
    proof_file_id      uuid NULL REFERENCES files (id),
    completed_by       uuid NULL REFERENCES users (id),
    completed_at       timestamptz NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (payment_id, checkout_id) REFERENCES payments (id, checkout_id),
    FOREIGN KEY (booking_id, checkout_id) REFERENCES bookings (id, checkout_id),
    CHECK (booking_id IS NOT NULL OR provider_reference IS NOT NULL),
    CHECK (status <> 'completed' OR transfer_reference IS NOT NULL),
    CHECK (status = 'awaiting_bank_info'
           OR (bank_bin IS NOT NULL AND account_number IS NOT NULL AND account_holder IS NOT NULL)),
    CHECK ((status = 'completed') = (completed_at IS NOT NULL)),
    CHECK ((completed_at IS NULL) = (completed_by IS NULL))
);

-- At most one refund per booking, so the webhook and the reconcile job can
-- never both create one; at most one payment-level refund per incoming
-- transfer (a duplicate payment is a second transfer on the same payment).
CREATE UNIQUE INDEX refunds_booking_idx ON refunds (booking_id) WHERE booking_id IS NOT NULL;
CREATE UNIQUE INDEX refunds_payment_transfer_idx ON refunds (payment_id, provider_reference)
    WHERE booking_id IS NULL;
CREATE INDEX refunds_status_idx ON refunds (status);
CREATE INDEX refunds_payment_idx ON refunds (payment_id);

-- Bank account a customer saved for refunds (spec §6.3).
CREATE TABLE customer_refund_accounts (
    user_id        uuid PRIMARY KEY REFERENCES users (id),
    bank_bin       text NOT NULL CHECK (bank_bin ~ '^[0-9]{6}$'),
    account_number text NOT NULL CHECK (account_number ~ '^[0-9]{6,19}$'),
    account_holder text NOT NULL CHECK (btrim(account_holder) <> ''),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE customer_refund_accounts;
DROP TABLE refunds;
DROP TABLE payment_events;
DROP TABLE payments;
