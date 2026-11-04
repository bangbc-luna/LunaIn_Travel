// Package partner owns partners, their members and enabled service types.
package partner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/partner/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	StatusActive    = "active"
	StatusSuspended = "suspended"

	roleOwner       = "owner"
	maxTextLen      = 200
	maxReasonLen    = 500
	defaultPageSize = 20
	maxPageSize     = 100
	auditTargetKind = "partner"
)

// UserCreator creates the partner owner's account inside the caller's tx.
// Implemented by *identity.Service.
type UserCreator interface {
	CreateInternalUser(ctx context.Context, tx pgx.Tx, email, password string, role auth.Role) (uuid.UUID, error)
}

// ServiceTypeChecker resolves a service type code inside tx (unknown codes are
// catalog.ErrInvalidServiceType). Implemented by *catalog.Service.
type ServiceTypeChecker interface {
	ServiceType(ctx context.Context, tx pgx.Tx, code string) (catalog.ServiceType, error)
}

// LocationValidator checks that a ward belongs to a province. Implemented by
// *geo.Service.
type LocationValidator interface {
	ValidateLocation(ctx context.Context, tx pgx.Tx, provinceCode, wardCode string) error
}

// FileChecker checks an uploaded file's owner and purpose. Implemented by
// *files.Service.
type FileChecker interface {
	Owned(ctx context.Context, tx pgx.Tx, id, uploadedBy uuid.UUID, purpose string) error
}

// Deps are the collaborators of Service.
type Deps struct {
	Pool    *pgxpool.Pool
	Clock   clock.Clock
	Users   UserCreator
	Catalog ServiceTypeChecker
	Geo     LocationValidator
	Files   FileChecker
}

// Service implements the partner use cases.
type Service struct {
	pool    *pgxpool.Pool
	clock   clock.Clock
	users   UserCreator
	catalog ServiceTypeChecker
	geo     LocationValidator
	files   FileChecker
}

// NewService builds a Service.
func NewService(d Deps) *Service {
	return &Service{pool: d.Pool, clock: d.Clock, users: d.Users, catalog: d.Catalog, geo: d.Geo, files: d.Files}
}

// CreateInput is what a Super Admin supplies to create a partner.
type CreateInput struct {
	LegalName, DisplayName, ContactName, ContactPhone, ContactEmail, OwnerEmail, OwnerPassword string
	ServiceTypes                                                                               []string
}

// Partner is a business selling on the platform. ServiceTypes lists only the
// currently enabled types, in service_types.sort_order.
type Partner struct {
	ID                    uuid.UUID  `json:"id"`
	LegalName             string     `json:"legal_name"`
	DisplayName           string     `json:"display_name"`
	TaxCode               *string    `json:"tax_code"`
	BusinessLicenseFileID *uuid.UUID `json:"business_license_file_id"`
	Address               *string    `json:"address"`
	ProvinceCode          *string    `json:"province_code"`
	WardCode              *string    `json:"ward_code"`
	ContactName           string     `json:"contact_name"`
	ContactPhone          string     `json:"contact_phone"`
	ContactEmail          string     `json:"contact_email"`
	Status                string     `json:"status"`
	SuspendedAt           *time.Time `json:"suspended_at"`
	SuspendedReason       *string    `json:"suspended_reason"`
	ServiceTypes          []string   `json:"service_types"`
	OwnerUserID           uuid.UUID  `json:"owner_user_id"`
	CreatedAt             time.Time  `json:"created_at"`
}

// ListFilter selects a page of partners, newest first. Query matches
// legal_name or display_name case-insensitively.
type ListFilter struct {
	Status *string
	Query  string
	Limit  int
	Cursor *uuid.UUID
}

// normalizeText trims s and enforces 1..max characters (0..max if optional).
func normalizeText(s, field string, required bool, max int) (string, error) {
	s = strings.TrimSpace(s)
	if required && s == "" {
		return "", invalidPartner(field + " không được để trống")
	}
	if utf8.RuneCountInString(s) > max {
		return "", invalidPartner(fmt.Sprintf("%s tối đa %d ký tự", field, max))
	}
	return s, nil
}

func (in CreateInput) normalize() (CreateInput, error) {
	out := in
	var err error
	if out.LegalName, err = normalizeText(in.LegalName, "legal_name", true, maxTextLen); err != nil {
		return out, err
	}
	if out.DisplayName, err = normalizeText(in.DisplayName, "display_name", true, maxTextLen); err != nil {
		return out, err
	}
	if out.ContactName, err = normalizeText(in.ContactName, "contact_name", false, maxTextLen); err != nil {
		return out, err
	}
	if out.ContactPhone, err = normalizeText(in.ContactPhone, "contact_phone", false, maxTextLen); err != nil {
		return out, err
	}
	if out.ContactEmail, err = normalizeText(in.ContactEmail, "contact_email", false, maxTextLen); err != nil {
		return out, err
	}
	if out.ContactEmail != "" {
		if out.ContactEmail, err = identity.NormalizeEmail(out.ContactEmail); err != nil {
			return out, invalidPartner("contact_email không hợp lệ")
		}
	}
	// The owner email is validated by identity; normalized here only so the
	// audit entry records the stored form.
	if e, err := identity.NormalizeEmail(in.OwnerEmail); err == nil {
		out.OwnerEmail = e
	}
	seen := make(map[string]bool, len(in.ServiceTypes))
	out.ServiceTypes = make([]string, 0, len(in.ServiceTypes))
	for _, c := range in.ServiceTypes {
		if !seen[c] {
			seen[c] = true
			out.ServiceTypes = append(out.ServiceTypes, c)
		}
	}
	return out, nil
}

// createAudit is the partner.create audit payload. It never holds the password.
type createAudit struct {
	Partner
	OwnerEmail string `json:"owner_email"`
}

// Create creates the owner account (role partner, verified), the partner, its
// owner membership and enabled service types in one transaction, in lock
// order users → partners. Any failure leaves no rows behind.
func (s *Service) Create(ctx context.Context, actor uuid.UUID, in CreateInput) (Partner, error) {
	in, err := in.normalize()
	if err != nil {
		return Partner{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Partner{}, fmt.Errorf("partner id: %w", err)
	}
	now := s.clock.Now()
	var p Partner
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		ownerID, err := s.users.CreateInternalUser(ctx, tx, in.OwnerEmail, in.OwnerPassword, auth.RolePartner)
		if err != nil {
			return err
		}
		q := store.New(tx)
		if err := q.InsertPartner(ctx, store.InsertPartnerParams{
			ID: id, LegalName: in.LegalName, DisplayName: in.DisplayName,
			ContactName: in.ContactName, ContactPhone: in.ContactPhone, ContactEmail: in.ContactEmail,
			CreatedBy: actor, Now: now,
		}); err != nil {
			return fmt.Errorf("insert partner: %w", err)
		}
		if err := q.InsertMember(ctx, store.InsertMemberParams{PartnerID: id, UserID: ownerID, Role: roleOwner, Now: now}); err != nil {
			return fmt.Errorf("insert partner member: %w", err)
		}
		for _, code := range in.ServiceTypes {
			if _, err := s.catalog.ServiceType(ctx, tx, code); err != nil {
				return err
			}
			if _, err := q.EnableServiceType(ctx, store.EnableServiceTypeParams{PartnerID: id, ServiceType: code, Actor: actor, Now: now}); err != nil {
				return fmt.Errorf("enable service type: %w", err)
			}
		}
		if p, err = get(ctx, q, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor,
			Action:      "partner.create",
			TargetKind:  auditTargetKind,
			TargetID:    &id,
			After:       createAudit{Partner: p, OwnerEmail: in.OwnerEmail},
		})
	})
	if err != nil {
		return Partner{}, err
	}
	return p, nil
}

// Get returns one partner; a missing one is ErrPartnerNotFound.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Partner, error) {
	return get(ctx, store.New(s.pool), id)
}

func get(ctx context.Context, q *store.Queries, id uuid.UUID) (Partner, error) {
	row, err := q.GetPartner(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Partner{}, ErrPartnerNotFound
	}
	if err != nil {
		return Partner{}, fmt.Errorf("get partner: %w", err)
	}
	ps, err := hydrate(ctx, q, []store.Partner{row})
	if err != nil {
		return Partner{}, err
	}
	return ps[0], nil
}

// hydrate converts rows and attaches owners and enabled service types with one
// query each.
func hydrate(ctx context.Context, q *store.Queries, rows []store.Partner) ([]Partner, error) {
	out := make([]Partner, len(rows))
	ids := make([]uuid.UUID, len(rows))
	idx := make(map[uuid.UUID]int, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
		idx[r.ID] = i
		out[i] = fromRow(r)
	}
	if len(rows) == 0 {
		return out, nil
	}
	owners, err := q.OwnersOf(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("partner owners: %w", err)
	}
	for _, o := range owners {
		out[idx[o.PartnerID]].OwnerUserID = o.UserID
	}
	types, err := q.EnabledServiceTypesOf(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("partner service types: %w", err)
	}
	for _, t := range types {
		i := idx[t.PartnerID]
		out[i].ServiceTypes = append(out[i].ServiceTypes, t.ServiceType)
	}
	return out, nil
}

func fromRow(r store.Partner) Partner {
	p := Partner{
		ID: r.ID, LegalName: r.LegalName, DisplayName: r.DisplayName, TaxCode: r.TaxCode,
		Address: r.Address, ProvinceCode: r.ProvinceCode, WardCode: r.WardCode,
		ContactName: r.ContactName, ContactPhone: r.ContactPhone, ContactEmail: r.ContactEmail,
		Status: r.Status, SuspendedAt: r.SuspendedAt, SuspendedReason: r.SuspendedReason,
		ServiceTypes: []string{}, CreatedAt: r.CreatedAt,
	}
	if r.BusinessLicenseFileID.Valid {
		id := uuid.UUID(r.BusinessLicenseFileID.Bytes)
		p.BusinessLicenseFileID = &id
	}
	return p
}

// likeEscaper makes LIKE wildcards in user input literal (ESCAPE '\').
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// List returns a page of partners, newest first (uuid v7 ids are time
// ordered), and the cursor of the next page (nil on the last page).
func (s *Service) List(ctx context.Context, f ListFilter) ([]Partner, *uuid.UUID, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)
	if f.Status != nil && *f.Status != StatusActive && *f.Status != StatusSuspended {
		return nil, nil, httpx.ErrInvalidRequest
	}
	params := store.ListPartnersParams{Status: f.Status, Lim: int32(limit + 1)}
	if q := strings.TrimSpace(f.Query); q != "" {
		pattern := "%" + likeEscaper.Replace(q) + "%"
		params.Pattern = &pattern
	}
	if f.Cursor != nil {
		params.Cursor = pgtype.UUID{Bytes: *f.Cursor, Valid: true}
	}
	q := store.New(s.pool)
	rows, err := q.ListPartners(ctx, params)
	if err != nil {
		return nil, nil, fmt.Errorf("list partners: %w", err)
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1].ID
		next = &last
	}
	out, err := hydrate(ctx, q, rows)
	if err != nil {
		return nil, nil, err
	}
	return out, next, nil
}

// lockPartner takes FOR UPDATE on the partners row and returns its status.
func lockPartner(ctx context.Context, q *store.Queries, id uuid.UUID) (string, error) {
	st, err := q.LockPartner(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPartnerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock partner: %w", err)
	}
	return st, nil
}

type serviceTypeAudit struct {
	ServiceType string `json:"service_type"`
}

// EnableServiceType enables code for the partner, re-enabling a disabled one.
// Enabling an already enabled type is a no-op (no audit row).
func (s *Service) EnableServiceType(ctx context.Context, actor, partnerID uuid.UUID, code string) error {
	now := s.clock.Now()
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := lockPartner(ctx, q, partnerID); err != nil {
			return err
		}
		if _, err := s.catalog.ServiceType(ctx, tx, code); err != nil {
			return err
		}
		n, err := q.EnableServiceType(ctx, store.EnableServiceTypeParams{PartnerID: partnerID, ServiceType: code, Actor: actor, Now: now})
		if err != nil {
			return fmt.Errorf("enable service type: %w", err)
		}
		if n == 0 {
			return nil
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "partner.service_type.enable", TargetKind: auditTargetKind,
			TargetID: &partnerID, After: serviceTypeAudit{ServiceType: code},
		})
	})
}

// DisableServiceType disables code for the partner. A type that was never
// enabled is ErrServiceTypeNotEnabled; disabling a disabled type is a no-op.
func (s *Service) DisableServiceType(ctx context.Context, actor, partnerID uuid.UUID, code string) error {
	now := s.clock.Now()
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := lockPartner(ctx, q, partnerID); err != nil {
			return err
		}
		if _, err := s.catalog.ServiceType(ctx, tx, code); err != nil {
			return err
		}
		n, err := q.DisableServiceType(ctx, store.DisableServiceTypeParams{PartnerID: partnerID, ServiceType: code, Actor: actor, Now: now})
		if err != nil {
			return fmt.Errorf("disable service type: %w", err)
		}
		if n == 0 {
			exists, err := q.ServiceTypeRowExists(ctx, store.ServiceTypeRowExistsParams{PartnerID: partnerID, ServiceType: code})
			if err != nil {
				return fmt.Errorf("service type row exists: %w", err)
			}
			if !exists {
				return ErrServiceTypeNotEnabled
			}
			return nil
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "partner.service_type.disable", TargetKind: auditTargetKind,
			TargetID: &partnerID, Before: serviceTypeAudit{ServiceType: code},
		})
	})
}

type statusAudit struct {
	Status      string     `json:"status"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
}

// Suspend suspends an active partner. reason is required (≤ 500 characters).
// The partner's users can still log in (spec §5.2).
func (s *Service) Suspend(ctx context.Context, actor, partnerID uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxReasonLen {
		return ErrInvalidSuspendReason
	}
	now := s.clock.Now()
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		st, err := lockPartner(ctx, q, partnerID)
		if err != nil {
			return err
		}
		if st == StatusSuspended {
			return ErrAlreadySuspended
		}
		if err := q.SetSuspended(ctx, store.SetSuspendedParams{ID: partnerID, Now: now, Reason: reason}); err != nil {
			return fmt.Errorf("suspend partner: %w", err)
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "partner.suspend", TargetKind: auditTargetKind, TargetID: &partnerID,
			Before: statusAudit{Status: st}, After: statusAudit{Status: StatusSuspended, SuspendedAt: &now},
			Reason: reason,
		})
	})
}

// Unsuspend reactivates a suspended partner; an active one is ErrNotSuspended.
func (s *Service) Unsuspend(ctx context.Context, actor, partnerID uuid.UUID) error {
	now := s.clock.Now()
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		st, err := lockPartner(ctx, q, partnerID)
		if err != nil {
			return err
		}
		if st != StatusSuspended {
			return ErrNotSuspended
		}
		if err := q.SetActive(ctx, store.SetActiveParams{ID: partnerID, Now: now}); err != nil {
			return fmt.Errorf("unsuspend partner: %w", err)
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "partner.unsuspend", TargetKind: auditTargetKind, TargetID: &partnerID,
			Before: statusAudit{Status: st}, After: statusAudit{Status: StatusActive},
		})
	})
}

// IsServiceTypeEnabled reports, inside tx, whether code is currently enabled
// for the partner.
func (s *Service) IsServiceTypeEnabled(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, code string) (bool, error) {
	ok, err := store.New(tx).IsServiceTypeEnabled(ctx, store.IsServiceTypeEnabledParams{PartnerID: partnerID, ServiceType: code})
	if err != nil {
		return false, fmt.Errorf("is service type enabled: %w", err)
	}
	return ok, nil
}

// Status returns the partner's status inside tx. With lock it reads the row
// FOR SHARE (DB §1.1) so a concurrent Suspend waits for the caller's tx.
func (s *Service) Status(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, lock bool) (string, error) {
	q := store.New(tx)
	var st string
	var err error
	if lock {
		st, err = q.SharePartnerStatus(ctx, partnerID)
	} else {
		st, err = q.PartnerStatus(ctx, partnerID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPartnerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("partner status: %w", err)
	}
	return st, nil
}
