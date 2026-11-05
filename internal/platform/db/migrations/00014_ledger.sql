-- +goose Up
-- Ledger schema (DB §8). Payouts arrive in plan 5.

-- Cache of the partner's balances, updated in the same tx as each entry.
-- Both buckets may go negative. last_seq hands out ledger_entries.seq while
-- this row is locked.
CREATE TABLE partner_balances (
    partner_id uuid PRIMARY KEY REFERENCES partners (id),
    pending    bigint NOT NULL DEFAULT 0,
    available  bigint NOT NULL DEFAULT 0,
    last_seq   bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Append-only: a trigger below rejects UPDATE, DELETE and TRUNCATE.
CREATE TABLE ledger_entries (
    id              uuid PRIMARY KEY,
    partner_id      uuid NOT NULL REFERENCES partners (id),
    -- Gapless per partner, = partner_balances.last_seq + 1 under its row lock.
    -- Statements and reconciliation follow seq, never created_at.
    seq             bigint NOT NULL CHECK (seq > 0),
    -- partner_balances.available right after this entry.
    available_after bigint NOT NULL,
    booking_id      uuid NULL REFERENCES bookings (id),
    -- TODO(plan 5): REFERENCES payouts (id) once the payouts table exists.
    payout_id       uuid NULL,
    kind            text NOT NULL CHECK (kind IN
                        ('booking_earning','refund_debit','commission_waiver','release',
                         'payout_hold','payout_reversal','adjustment')),
    bucket          text NOT NULL CHECK (bucket IN ('pending','available')),
    -- Signed; a zero amount is never written.
    amount          bigint NOT NULL CHECK (amount <> 0),
    reason          text NULL,
    created_by      uuid NULL REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (partner_id, seq),
    -- The booking must belong to this partner (DB §1.2).
    FOREIGN KEY (booking_id, partner_id) REFERENCES bookings (id, partner_id),
    CHECK (kind NOT IN ('booking_earning','refund_debit','commission_waiver','release')
           OR (booking_id IS NOT NULL AND payout_id IS NULL)),
    CHECK (kind NOT IN ('payout_hold','payout_reversal')
           OR (payout_id IS NOT NULL AND booking_id IS NULL)),
    CHECK (kind <> 'adjustment' OR (reason IS NOT NULL AND created_by IS NOT NULL))
);

-- Each booking kind is written at most once per bucket (release touches both
-- buckets once). Adjustments are deliberately outside this index.
CREATE UNIQUE INDEX ledger_entries_booking_kind_bucket_idx ON ledger_entries (booking_id, kind, bucket)
    WHERE kind IN ('booking_earning','refund_debit','commission_waiver','release');
-- A payout is held or reversed at most once.
CREATE UNIQUE INDEX ledger_entries_payout_kind_idx ON ledger_entries (payout_id, kind)
    WHERE kind IN ('payout_hold','payout_reversal');
CREATE INDEX ledger_entries_booking_idx ON ledger_entries (booking_id) WHERE booking_id IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION ledger_entries_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_immutable();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_immutable();

-- Money of each booking still sitting in pending; the release job scans
-- release_due_at. Lock only after the bookings row (DB §1.1).
CREATE TABLE booking_settlements (
    booking_id     uuid PRIMARY KEY,
    partner_id     uuid NOT NULL REFERENCES partners (id),
    net_pending    bigint NOT NULL,
    release_due_at timestamptz NULL,
    released_at    timestamptz NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (booking_id, partner_id) REFERENCES bookings (id, partner_id)
);

CREATE INDEX booking_settlements_release_due_idx ON booking_settlements (release_due_at)
    WHERE released_at IS NULL;

-- Platform revenue: commission (positive) and its waiver (negative).
CREATE TABLE platform_commission_entries (
    id           uuid PRIMARY KEY,
    booking_id   uuid NOT NULL REFERENCES bookings (id),
    service_type text NOT NULL REFERENCES service_types (code),
    kind         text NOT NULL CHECK (kind IN ('commission','waiver')),
    amount       bigint NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (booking_id, kind),
    CHECK ((kind = 'commission' AND amount > 0) OR (kind = 'waiver' AND amount < 0))
);

-- +goose Down
DROP TABLE platform_commission_entries;
DROP TABLE booking_settlements;
DROP TRIGGER ledger_entries_no_truncate ON ledger_entries;
DROP TRIGGER ledger_entries_immutable ON ledger_entries;
DROP TABLE ledger_entries;
DROP FUNCTION ledger_entries_immutable();
DROP TABLE partner_balances;
