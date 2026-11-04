-- name: InsertPartner :exec
INSERT INTO partners (id, legal_name, display_name, contact_name, contact_phone, contact_email, created_by, created_at, updated_at)
VALUES (@id, @legal_name, @display_name, @contact_name, @contact_phone, @contact_email, @created_by, @now, @now);

-- name: InsertMember :exec
INSERT INTO partner_members (partner_id, user_id, role, created_at, updated_at)
VALUES (@partner_id, @user_id, @role, @now, @now);

-- name: GetPartner :one
SELECT * FROM partners WHERE id = @id;

-- Writers lock the partners row FOR NO KEY UPDATE (never FOR UPDATE, DB §1.1):
-- inserts into tables with an FK to partners take KEY SHARE on it implicitly,
-- which FOR UPDATE would block; FOR NO KEY UPDATE still conflicts with the
-- FOR SHARE taken by Status(lock=true).
-- name: LockPartner :one
SELECT status FROM partners WHERE id = @id FOR NO KEY UPDATE;

-- Status(lock=true): blocks Suspend/Disable (FOR NO KEY UPDATE) until the
-- caller's tx ends, but not other readers or FK inserts.
-- name: SharePartnerStatus :one
SELECT status FROM partners WHERE id = @id FOR SHARE;

-- name: PartnerStatus :one
SELECT status FROM partners WHERE id = @id;

-- name: ListPartners :many
SELECT * FROM partners
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(pattern)::text IS NULL
       OR legal_name ILIKE sqlc.narg(pattern) ESCAPE '\'
       OR display_name ILIKE sqlc.narg(pattern) ESCAPE '\')
  AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor))
ORDER BY id DESC
LIMIT @lim;

-- name: OwnersOf :many
SELECT DISTINCT ON (partner_id) partner_id, user_id FROM partner_members
WHERE partner_id = ANY(@ids::uuid[]) AND role = 'owner'
ORDER BY partner_id, created_at, user_id;

-- The join on catalog's service_types is an accepted read-only cross-module
-- join (Ruling 12, DB §11): it is used only to order the codes by
-- service_types.sort_order; partner never writes or locks service_types.
-- name: EnabledServiceTypesOf :many
SELECT pst.partner_id, pst.service_type FROM partner_service_types pst
JOIN service_types st ON st.code = pst.service_type
WHERE pst.partner_id = ANY(@ids::uuid[]) AND pst.disabled_at IS NULL
ORDER BY pst.partner_id, st.sort_order;

-- name: EnableServiceType :execrows
INSERT INTO partner_service_types (partner_id, service_type, enabled_by, enabled_at, created_at, updated_at)
VALUES (@partner_id, @service_type, @actor, @now, @now, @now)
ON CONFLICT (partner_id, service_type) DO UPDATE
SET enabled_by = EXCLUDED.enabled_by,
    enabled_at = EXCLUDED.enabled_at,
    disabled_at = NULL,
    disabled_by = NULL,
    updated_at = EXCLUDED.updated_at
WHERE partner_service_types.disabled_at IS NOT NULL;

-- name: DisableServiceType :execrows
UPDATE partner_service_types
SET disabled_at = @now::timestamptz, disabled_by = @actor::uuid, updated_at = @now
WHERE partner_id = @partner_id AND service_type = @service_type AND disabled_at IS NULL;

-- name: ServiceTypeRowExists :one
SELECT EXISTS (SELECT 1 FROM partner_service_types WHERE partner_id = @partner_id AND service_type = @service_type);

-- name: IsServiceTypeEnabled :one
SELECT EXISTS (
    SELECT 1 FROM partner_service_types
    WHERE partner_id = @partner_id AND service_type = @service_type AND disabled_at IS NULL
);

-- name: SetSuspended :exec
UPDATE partners
SET status = 'suspended', suspended_at = @now::timestamptz, suspended_reason = @reason::text, updated_at = @now
WHERE id = @id;

-- name: SetActive :exec
UPDATE partners
SET status = 'active', suspended_at = NULL, suspended_reason = NULL, updated_at = @now
WHERE id = @id;

-- name: MemberPartnerID :one
SELECT partner_id FROM partner_members WHERE user_id = @user_id;

-- Locks FOR NO KEY UPDATE like LockPartner (DB §1.1).
-- name: GetPartnerForUpdate :one
SELECT * FROM partners WHERE id = @id FOR NO KEY UPDATE;

-- name: UpdateProfile :exec
UPDATE partners
SET display_name = @display_name,
    tax_code = sqlc.narg(tax_code),
    business_license_file_id = sqlc.narg(business_license_file_id),
    address = sqlc.narg(address),
    province_code = sqlc.narg(province_code),
    ward_code = sqlc.narg(ward_code),
    contact_name = @contact_name,
    contact_phone = @contact_phone,
    contact_email = @contact_email,
    updated_at = @now
WHERE id = @id;

-- name: ClearCurrentBankAccount :exec
UPDATE partner_bank_accounts SET is_current = false, updated_at = @now
WHERE partner_id = @partner_id AND is_current;

-- name: InsertBankAccount :one
INSERT INTO partner_bank_accounts (id, partner_id, bank_bin, bank_name, account_number, account_holder, is_current, created_at, updated_at)
VALUES (@id, @partner_id, @bank_bin, @bank_name, @account_number, @account_holder, true, @now, @now)
RETURNING *;

-- name: ListBankAccounts :many
SELECT * FROM partner_bank_accounts WHERE partner_id = @partner_id
ORDER BY created_at DESC, id DESC;

-- name: CurrentBankAccount :one
SELECT * FROM partner_bank_accounts WHERE partner_id = @partner_id AND is_current;
