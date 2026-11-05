-- +goose Up
-- Payout schema (DB §8) and the plan 5 ledger constraints.

CREATE TABLE payouts (
    id                 uuid PRIMARY KEY,
    partner_id         uuid NOT NULL REFERENCES partners (id),
    amount             bigint NOT NULL CHECK (amount > 0),
    status             text NOT NULL CHECK (status IN ('requested','approved','paid','rejected')),
    -- Snapshot of the partner's bank account at request time; the payout is
    -- paid to this, whatever the partner sets afterwards.
    bank_bin           text NOT NULL CHECK (bank_bin ~ '^[0-9]{6}$'),
    bank_name          text NOT NULL,
    account_number     text NOT NULL CHECK (account_number ~ '^[0-9]{6,19}$'),
    account_holder     text NOT NULL CHECK (account_holder ~ '^[A-Z]+( [A-Z]+)*$'),
    bank_account_id    uuid NOT NULL REFERENCES partner_bank_accounts (id),
    -- Statement window over ledger seq. Filled in the creating tx right after
    -- payout_hold (the ledger entry needs the payout row to exist first).
    statement_seq_from bigint NULL,
    statement_seq_to   bigint NULL,
    opening_available  bigint NULL,
    closing_available  bigint NULL,
    requested_by       uuid NOT NULL REFERENCES users (id),
    requested_at       timestamptz NOT NULL,
    approved_by        uuid NULL REFERENCES users (id),
    approved_at        timestamptz NULL,
    paid_by            uuid NULL REFERENCES users (id),
    paid_at            timestamptz NULL,
    transfer_reference text NULL,
    proof_file_id      uuid NULL REFERENCES files (id),
    rejected_by        uuid NULL REFERENCES users (id),
    rejected_at        timestamptz NULL,
    reject_reason      text NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payouts_statement_set_check CHECK (
        (statement_seq_to IS NULL) = (statement_seq_from IS NULL)
        AND (opening_available IS NULL) = (statement_seq_from IS NULL)
        AND (closing_available IS NULL) = (statement_seq_from IS NULL)),
    CONSTRAINT payouts_statement_range_check CHECK (statement_seq_from <= statement_seq_to),
    CONSTRAINT payouts_approved_check CHECK (
        status NOT IN ('approved','paid') OR (approved_at IS NOT NULL AND approved_by IS NOT NULL)),
    CONSTRAINT payouts_paid_check CHECK ((status = 'paid') = (
        paid_at IS NOT NULL AND paid_by IS NOT NULL AND transfer_reference IS NOT NULL AND proof_file_id IS NOT NULL)),
    CONSTRAINT payouts_rejected_check CHECK ((status = 'rejected') = (
        rejected_at IS NOT NULL AND rejected_by IS NOT NULL AND reject_reason IS NOT NULL))
);

-- One open payout per partner.
CREATE UNIQUE INDEX payouts_one_open_idx ON payouts (partner_id) WHERE status IN ('requested','approved');
CREATE INDEX payouts_status_idx ON payouts (status, id DESC);
CREATE INDEX payouts_partner_idx ON payouts (partner_id, id DESC);

ALTER TABLE ledger_entries
    ADD CONSTRAINT ledger_entries_payout_fk FOREIGN KEY (payout_id) REFERENCES payouts (id),
    ADD CONSTRAINT ledger_entries_payout_hold_check
        CHECK (kind <> 'payout_hold' OR (bucket = 'available' AND amount < 0)),
    ADD CONSTRAINT ledger_entries_payout_reversal_check
        CHECK (kind <> 'payout_reversal' OR (bucket = 'available' AND amount > 0)),
    ADD CONSTRAINT ledger_entries_adjustment_check
        CHECK (kind <> 'adjustment' OR (bucket = 'available' AND payout_id IS NULL));

-- Who set the account; NULL on rows older than this migration.
ALTER TABLE partner_bank_accounts
    ADD COLUMN created_by uuid NULL REFERENCES users (id),
    ADD CONSTRAINT partner_bank_accounts_holder_check CHECK (account_holder ~ '^[A-Z]+( [A-Z]+)*$');

-- +goose Down
ALTER TABLE partner_bank_accounts
    DROP CONSTRAINT partner_bank_accounts_holder_check,
    DROP COLUMN created_by;
ALTER TABLE ledger_entries
    DROP CONSTRAINT ledger_entries_adjustment_check,
    DROP CONSTRAINT ledger_entries_payout_reversal_check,
    DROP CONSTRAINT ledger_entries_payout_hold_check,
    DROP CONSTRAINT ledger_entries_payout_fk;
DROP TABLE payouts;
