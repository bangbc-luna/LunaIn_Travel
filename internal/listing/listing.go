package listing

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/text"
)

const (
	ReviewDraft         = "draft"
	ReviewPendingReview = "pending_review"
	ReviewApproved      = "approved"
	ReviewRejected      = "rejected"

	maxTitleLen       = 200
	maxSummaryLen     = 500
	maxDescriptionLen = 20000
	maxAddressLen     = 300

	defaultPageSize = 20
	maxPageSize     = 100

	slugSuffixLen   = 6
	slugBaseMaxLen  = 80
	slugAlphabet    = "abcdefghijklmnopqrstuvwxyz0123456789"
	slugInsertTries = 5

	actionCreate = "create"
	actionUpdate = "update"
)

// Listing is the common layer of a sellable service.
type Listing struct {
	ID              uuid.UUID  `json:"id"`
	PartnerID       uuid.UUID  `json:"partner_id"`
	ServiceType     string     `json:"service_type"`
	Title           string     `json:"title"`
	Slug            string     `json:"slug"`
	Summary         *string    `json:"summary"`
	Description     *string    `json:"description"`
	Address         *string    `json:"address"`
	ProvinceCode    *string    `json:"province_code"`
	WardCode        *string    `json:"ward_code"`
	Latitude        *float64   `json:"latitude"`
	Longitude       *float64   `json:"longitude"`
	CoverFileID     *uuid.UUID `json:"cover_file_id"`
	ReviewStatus    string     `json:"review_status"`
	Visibility      string     `json:"visibility"`
	RejectionReason *string    `json:"rejection_reason"`
	HiddenReason    *string    `json:"hidden_reason"`
	SubmittedAt     *time.Time `json:"submitted_at"`
	ReviewedAt      *time.Time `json:"reviewed_at"`
	RatingSum       int        `json:"rating_sum"`
	RatingCount     int        `json:"rating_count"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// CreateInput is what a partner supplies to create a listing. The JSON tags
// are the POST /v1/partner/listings body; unknown fields such as partner_id
// are rejected by httpx.DecodeJSON.
type CreateInput struct {
	ServiceType  string   `json:"service_type"`
	Title        string   `json:"title"`
	Summary      *string  `json:"summary"`
	Description  *string  `json:"description"`
	Address      *string  `json:"address"`
	ProvinceCode *string  `json:"province_code"`
	WardCode     *string  `json:"ward_code"`
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
}

// UpdateInput is a partial update (PATCH body): nil keeps a field, "" clears
// an optional text field. Title is required and cannot be cleared. Province
// and ward travel together. The service type cannot change.
type UpdateInput struct {
	Title        *string  `json:"title"`
	Summary      *string  `json:"summary"`
	Description  *string  `json:"description"`
	Address      *string  `json:"address"`
	ProvinceCode *string  `json:"province_code"`
	WardCode     *string  `json:"ward_code"`
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
}

// OwnFilter selects a page of the partner's own listings, newest first.
type OwnFilter struct {
	ReviewStatus *string
	Limit        int
	Cursor       *uuid.UUID
}

// content is the partner-editable part of a listing, already normalized.
type content struct {
	Title        string
	Summary      *string
	Description  *string
	Address      *string
	ProvinceCode *string
	WardCode     *string
	Latitude     *float64
	Longitude    *float64
}

func contentOf(r store.Listing) content {
	return content{
		Title: r.Title, Summary: r.Summary, Description: r.Description, Address: r.Address,
		ProvinceCode: r.ProvinceCode, WardCode: r.WardCode, Latitude: r.Latitude, Longitude: r.Longitude,
	}
}

// normalizeText trims s and enforces 1..max runes (0..max if optional).
func normalizeText(s, field string, required bool, max int) (string, error) {
	s = strings.TrimSpace(s)
	if required && s == "" {
		return "", invalidListing(field + " không được để trống")
	}
	if utf8.RuneCountInString(s) > max {
		return "", invalidListing(fmt.Sprintf("%s tối đa %d ký tự", field, max))
	}
	return s, nil
}

// optionalText normalizes an optional nullable column: "" becomes NULL.
func optionalText(s *string, field string, max int) (*string, error) {
	if s == nil {
		return nil, nil
	}
	v, err := normalizeText(*s, field, false, max)
	if err != nil || v == "" {
		return nil, err
	}
	return &v, nil
}

// location normalizes a province/ward pair given together: both nil or both
// "" mean no location; exactly one of them set is geo.ErrInvalidLocation.
func location(province, ward *string) (*string, *string, error) {
	var p, w string
	if province != nil {
		p = strings.TrimSpace(*province)
	}
	if ward != nil {
		w = strings.TrimSpace(*ward)
	}
	if (p == "") != (w == "") {
		return nil, nil, geo.ErrInvalidLocation
	}
	if p == "" {
		return nil, nil, nil
	}
	return &p, &w, nil
}

// coordinate checks v against ±limit and rounds it to the 6 decimals of
// numeric(9,6), so that a stored value compares equal to its input.
func coordinate(v *float64, field string, limit float64) (*float64, error) {
	if v == nil {
		return nil, nil
	}
	if math.IsNaN(*v) || *v < -limit || *v > limit {
		return nil, invalidListing(fmt.Sprintf("%s phải nằm trong khoảng -%g..%g", field, limit, limit))
	}
	r := math.Round(*v*1e6) / 1e6
	return &r, nil
}

// checkCoordinates requires latitude and longitude to be set together.
func (c content) checkCoordinates() error {
	if (c.Latitude == nil) != (c.Longitude == nil) {
		return invalidListing("latitude và longitude phải được nhập cùng nhau")
	}
	return nil
}

func (in CreateInput) normalize() (content, error) {
	var c content
	var err error
	if c.Title, err = normalizeText(in.Title, "title", true, maxTitleLen); err != nil {
		return c, err
	}
	if c.Summary, err = optionalText(in.Summary, "summary", maxSummaryLen); err != nil {
		return c, err
	}
	if c.Description, err = optionalText(in.Description, "description", maxDescriptionLen); err != nil {
		return c, err
	}
	if c.Address, err = optionalText(in.Address, "address", maxAddressLen); err != nil {
		return c, err
	}
	if c.Latitude, err = coordinate(in.Latitude, "latitude", 90); err != nil {
		return c, err
	}
	if c.Longitude, err = coordinate(in.Longitude, "longitude", 180); err != nil {
		return c, err
	}
	if err = c.checkCoordinates(); err != nil {
		return c, err
	}
	if c.ProvinceCode, c.WardCode, err = location(in.ProvinceCode, in.WardCode); err != nil {
		return c, err
	}
	return c, nil
}

// apply validates u and merges it into cur.
func (u UpdateInput) apply(cur content) (content, error) {
	c := cur
	var err error
	if u.Title != nil {
		if c.Title, err = normalizeText(*u.Title, "title", true, maxTitleLen); err != nil {
			return c, err
		}
	}
	if u.Summary != nil {
		if c.Summary, err = optionalText(u.Summary, "summary", maxSummaryLen); err != nil {
			return c, err
		}
	}
	if u.Description != nil {
		if c.Description, err = optionalText(u.Description, "description", maxDescriptionLen); err != nil {
			return c, err
		}
	}
	if u.Address != nil {
		if c.Address, err = optionalText(u.Address, "address", maxAddressLen); err != nil {
			return c, err
		}
	}
	if u.Latitude != nil {
		if c.Latitude, err = coordinate(u.Latitude, "latitude", 90); err != nil {
			return c, err
		}
	}
	if u.Longitude != nil {
		if c.Longitude, err = coordinate(u.Longitude, "longitude", 180); err != nil {
			return c, err
		}
	}
	if err = c.checkCoordinates(); err != nil {
		return c, err
	}
	// Province and ward travel together so the stored pair stays consistent.
	if (u.ProvinceCode == nil) != (u.WardCode == nil) {
		return c, geo.ErrInvalidLocation
	}
	if u.ProvinceCode != nil {
		if c.ProvinceCode, c.WardCode, err = location(u.ProvinceCode, u.WardCode); err != nil {
			return c, err
		}
	}
	return c, nil
}

// change is one entry of listing_audit_log.changes.
type change struct {
	Old any `json:"old"`
	New any `json:"new"`
}

func val[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func eq[T comparable](a, b *T) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// diff returns {field: {old, new}} for every field that differs.
func diff(old, cur content) map[string]change {
	out := map[string]change{}
	if old.Title != cur.Title {
		out["title"] = change{old.Title, cur.Title}
	}
	strs := []struct {
		name     string
		old, cur *string
	}{
		{"summary", old.Summary, cur.Summary},
		{"description", old.Description, cur.Description},
		{"address", old.Address, cur.Address},
		{"province_code", old.ProvinceCode, cur.ProvinceCode},
		{"ward_code", old.WardCode, cur.WardCode},
	}
	for _, f := range strs {
		if !eq(f.old, f.cur) {
			out[f.name] = change{val(f.old), val(f.cur)}
		}
	}
	if !eq(old.Latitude, cur.Latitude) {
		out["latitude"] = change{val(old.Latitude), val(cur.Latitude)}
	}
	if !eq(old.Longitude, cur.Longitude) {
		out["longitude"] = change{val(old.Longitude), val(cur.Longitude)}
	}
	return out
}

// logChange writes one listing_audit_log row inside tx.
func (s *Service) logChange(ctx context.Context, tx pgx.Tx, listingID, actor uuid.UUID, action string, changes map[string]change, now time.Time) error {
	raw, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("marshal listing changes: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("listing audit id: %w", err)
	}
	if err := store.New(tx).InsertAuditLog(ctx, store.InsertAuditLogParams{
		ID: id, ListingID: listingID, ActorUserID: actor, Action: action, Changes: raw, Now: now,
	}); err != nil {
		return fmt.Errorf("insert listing audit log: %w", err)
	}
	return nil
}

// requireActivePartner reads the partner's status FOR SHARE (DB §1.1) so a
// concurrent Suspend or DisableServiceType waits for this tx; a suspended
// partner is ErrPartnerSuspended.
func (s *Service) requireActivePartner(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID) error {
	st, err := s.partners.Status(ctx, tx, partnerID, true)
	if err != nil {
		return err
	}
	if st == partner.StatusSuspended {
		return ErrPartnerSuspended
	}
	return nil
}

// slugBase is text.Slug(title) cut to slugBaseMaxLen, or "listing" if empty.
func slugBase(title string) string {
	b := text.Slug(title)
	if len(b) > slugBaseMaxLen {
		b = strings.TrimRight(b[:slugBaseMaxLen], "-")
	}
	if b == "" {
		return "listing"
	}
	return b
}

// slugSuffix returns slugSuffixLen random characters of [a-z0-9].
func slugSuffix() (string, error) {
	out := make([]byte, 0, slugSuffixLen)
	var buf [16]byte
	for len(out) < slugSuffixLen {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("slug suffix: %w", err)
		}
		for _, b := range buf {
			// 252 = 7 × 36: rejecting larger bytes keeps the choice uniform.
			if b < 252 && len(out) < slugSuffixLen {
				out = append(out, slugAlphabet[int(b)%len(slugAlphabet)])
			}
		}
	}
	return string(out), nil
}

// Create creates a draft listing for the partner. The slug is fixed here
// (text.Slug(title) + "-" + 6 random [a-z0-9]) and never changes afterwards.
func (s *Service) Create(ctx context.Context, partnerID, actor uuid.UUID, in CreateInput) (Listing, error) {
	c, err := in.normalize()
	if err != nil {
		return Listing{}, err
	}
	serviceType := strings.TrimSpace(in.ServiceType)
	now := s.clock.Now()
	var out store.Listing
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireActivePartner(ctx, tx, partnerID); err != nil {
			return err
		}
		if _, err := s.catalog.ServiceType(ctx, tx, serviceType); err != nil {
			return err
		}
		enabled, err := s.partners.IsServiceTypeEnabled(ctx, tx, partnerID, serviceType)
		if err != nil {
			return err
		}
		if !enabled {
			return ErrServiceTypeNotEnabled
		}
		if c.ProvinceCode != nil {
			if err := s.geo.ValidateLocation(ctx, tx, *c.ProvinceCode, *c.WardCode); err != nil {
				return err
			}
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("listing id: %w", err)
		}
		params := store.InsertListingParams{
			ID: id, PartnerID: partnerID, ServiceType: serviceType, Title: c.Title,
			Summary: c.Summary, Description: c.Description, Address: c.Address,
			ProvinceCode: c.ProvinceCode, WardCode: c.WardCode,
			Latitude: c.Latitude, Longitude: c.Longitude, Now: now,
		}
		q := store.New(tx)
		for try := 0; ; try++ {
			suffix, err := slugSuffix()
			if err != nil {
				return err
			}
			params.Slug = slugBase(c.Title) + "-" + suffix
			out, err = q.InsertListing(ctx, params)
			if err == nil {
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("insert listing: %w", err)
			}
			if try+1 == slugInsertTries {
				return fmt.Errorf("insert listing: no free slug after %d tries", slugInsertTries)
			}
		}
		changes := diff(content{}, c)
		changes["service_type"] = change{nil, serviceType}
		return s.logChange(ctx, tx, out.ID, actor, actionCreate, changes, now)
	})
	if err != nil {
		return Listing{}, err
	}
	return fromRow(out), nil
}

// Get returns one of the partner's listings; a listing of another partner is
// ErrListingNotFound, like a missing one.
func (s *Service) Get(ctx context.Context, partnerID, id uuid.UUID) (Listing, error) {
	r, err := store.New(s.pool).GetOwnedListing(ctx, store.GetOwnedListingParams{ID: id, PartnerID: partnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, ErrListingNotFound
	}
	if err != nil {
		return Listing{}, fmt.Errorf("get listing: %w", err)
	}
	return fromRow(r), nil
}

func validReviewStatus(s string) bool {
	switch s {
	case ReviewDraft, ReviewPendingReview, ReviewApproved, ReviewRejected:
		return true
	}
	return false
}

// ListOwn returns a page of the partner's listings, newest first (uuid v7 ids
// are time ordered), and the cursor of the next page (nil on the last page).
func (s *Service) ListOwn(ctx context.Context, partnerID uuid.UUID, f OwnFilter) ([]Listing, *uuid.UUID, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)
	if f.ReviewStatus != nil && !validReviewStatus(*f.ReviewStatus) {
		return nil, nil, httpx.ErrInvalidRequest
	}
	rows, err := store.New(s.pool).ListOwnListings(ctx, store.ListOwnListingsParams{
		PartnerID: partnerID, ReviewStatus: f.ReviewStatus, Cursor: f.Cursor, Lim: int32(limit + 1),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list own listings: %w", err)
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1].ID
		next = &last
	}
	out := make([]Listing, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, next, nil
}

// Update applies a partner's partial update to its listing and records the
// fields that really changed in listing_audit_log. An update that changes
// nothing writes nothing. Suspended partners get ErrPartnerSuspended.
func (s *Service) Update(ctx context.Context, partnerID, actor, id uuid.UUID, in UpdateInput) (Listing, error) {
	now := s.clock.Now()
	var out store.Listing
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireActivePartner(ctx, tx, partnerID); err != nil {
			return err
		}
		cur, err := s.lockOwned(ctx, tx, partnerID, id)
		if err != nil {
			return err
		}
		old := contentOf(cur)
		c, err := in.apply(old)
		if err != nil {
			return err
		}
		changes := diff(old, c)
		if len(changes) == 0 {
			out = cur
			return nil
		}
		_, provChanged := changes["province_code"]
		_, wardChanged := changes["ward_code"]
		if (provChanged || wardChanged) && c.ProvinceCode != nil {
			if err := s.geo.ValidateLocation(ctx, tx, *c.ProvinceCode, *c.WardCode); err != nil {
				return err
			}
		}
		out, err = store.New(tx).UpdateListingContent(ctx, store.UpdateListingContentParams{
			ID: id, Title: c.Title, Summary: c.Summary, Description: c.Description, Address: c.Address,
			ProvinceCode: c.ProvinceCode, WardCode: c.WardCode,
			Latitude: c.Latitude, Longitude: c.Longitude, Now: now,
		})
		if err != nil {
			return fmt.Errorf("update listing: %w", err)
		}
		return s.logChange(ctx, tx, id, actor, actionUpdate, changes, now)
	})
	if err != nil {
		return Listing{}, err
	}
	return fromRow(out), nil
}

// lockOwned locks the partner's listing FOR NO KEY UPDATE (DB §1.1; callers
// lock the partners row first). A listing of another partner is
// ErrListingNotFound and is not locked.
func (s *Service) lockOwned(ctx context.Context, tx pgx.Tx, partnerID, id uuid.UUID) (store.Listing, error) {
	r, err := store.New(tx).LockOwnedListing(ctx, store.LockOwnedListingParams{ID: id, PartnerID: partnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Listing{}, ErrListingNotFound
	}
	if err != nil {
		return store.Listing{}, fmt.Errorf("lock listing: %w", err)
	}
	return r, nil
}

func fromRow(r store.Listing) Listing {
	return Listing{
		ID: r.ID, PartnerID: r.PartnerID, ServiceType: r.ServiceType, Title: r.Title, Slug: r.Slug,
		Summary: r.Summary, Description: r.Description, Address: r.Address,
		ProvinceCode: r.ProvinceCode, WardCode: r.WardCode, Latitude: r.Latitude, Longitude: r.Longitude,
		CoverFileID: r.CoverFileID, ReviewStatus: r.ReviewStatus, Visibility: r.Visibility,
		RejectionReason: r.RejectionReason, HiddenReason: r.HiddenReason,
		SubmittedAt: r.SubmittedAt, ReviewedAt: r.ReviewedAt,
		RatingSum: int(r.RatingSum), RatingCount: int(r.RatingCount),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
