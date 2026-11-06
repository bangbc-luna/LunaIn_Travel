package listing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	VisibilityPublished   = "published"
	VisibilityUnpublished = "unpublished_by_partner"
	VisibilityHidden      = "hidden_by_admin"

	maxReasonLen = 500

	actionSubmit    = "submit"
	actionPublish   = "publish"
	actionUnpublish = "unpublish"

	auditKindListing = "listing"
	recentAuditLimit = 20
)

// ReviewState is the review/visibility pair recorded as before/after of
// every admin audit entry.
type ReviewState struct {
	ReviewStatus string `json:"review_status"`
	Visibility   string `json:"visibility"`
}

func stateOf(r store.Listing) ReviewState {
	return ReviewState{ReviewStatus: r.ReviewStatus, Visibility: r.Visibility}
}

// transition is one state change applied to a locked listing row.
type transition struct {
	check func(cur store.Listing) bool
	// validate, if set, runs after a passing check (so 409 beats 422).
	validate func() error
	// verify, if set, runs after validate with the locked row (completeness).
	verify func(ctx context.Context, tx pgx.Tx, cur store.Listing) error
	apply  func(q *store.Queries, cur store.Listing, now time.Time) (store.Listing, error)
}

// partnerTransition runs t on the partner's own listing: active partner (403)
// -> lock owned listing (404) -> state check under the lock (409) -> extra
// (completeness) -> update -> listing_audit_log row.
func (s *Service) partnerTransition(ctx context.Context, partnerID, actor, id uuid.UUID, action string, t transition,
	extra func(ctx context.Context, tx pgx.Tx, cur store.Listing) error) (Listing, error) {
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
		if !t.check(cur) {
			return ErrInvalidListingState
		}
		if extra != nil {
			if err := extra(ctx, tx, cur); err != nil {
				return err
			}
		}
		if out, err = t.apply(store.New(tx), cur, now); err != nil {
			return fmt.Errorf("%s listing: %w", action, err)
		}
		return s.logChange(ctx, tx, id, actor, action, map[string]change{
			"review_status": {cur.ReviewStatus, out.ReviewStatus},
			"visibility":    {cur.Visibility, out.Visibility},
		}, now)
	})
	if err != nil {
		return Listing{}, err
	}
	return fromRow(out), nil
}

// Submit sends a draft or rejected listing to review once it is complete.
func (s *Service) Submit(ctx context.Context, partnerID, actor, id uuid.UUID) (Listing, error) {
	return s.partnerTransition(ctx, partnerID, actor, id, actionSubmit, transition{
		check: func(cur store.Listing) bool {
			return cur.ReviewStatus == ReviewDraft || cur.ReviewStatus == ReviewRejected
		},
		apply: func(q *store.Queries, cur store.Listing, now time.Time) (store.Listing, error) {
			return q.SubmitListing(ctx, store.SubmitListingParams{ID: cur.ID, Now: &now})
		},
	}, s.requireComplete)
}

// Unpublish takes an approved, published listing off the public site.
func (s *Service) Unpublish(ctx context.Context, partnerID, actor, id uuid.UUID) (Listing, error) {
	return s.setVisibility(ctx, partnerID, actor, id, actionUnpublish, VisibilityPublished, VisibilityUnpublished)
}

// Publish puts a partner-unpublished listing back. A listing hidden by an
// admin cannot be published by the partner.
func (s *Service) Publish(ctx context.Context, partnerID, actor, id uuid.UUID) (Listing, error) {
	return s.setVisibility(ctx, partnerID, actor, id, actionPublish, VisibilityUnpublished, VisibilityPublished)
}

func (s *Service) setVisibility(ctx context.Context, partnerID, actor, id uuid.UUID, action, from, to string) (Listing, error) {
	return s.partnerTransition(ctx, partnerID, actor, id, action, transition{
		check: func(cur store.Listing) bool { return cur.ReviewStatus == ReviewApproved && cur.Visibility == from },
		apply: func(q *store.Queries, cur store.Listing, now time.Time) (store.Listing, error) {
			return q.SetListingVisibility(ctx, store.SetListingVisibilityParams{ID: cur.ID, Visibility: to, Now: now})
		},
	}, nil)
}

// requireComplete lists every missing item in one listing_incomplete error.
// A cancellation policy is not required: no policy means no refund.
func (s *Service) requireComplete(ctx context.Context, tx pgx.Tx, cur store.Listing) error {
	var missing []string
	if strings.TrimSpace(cur.Title) == "" {
		missing = append(missing, "tiêu đề")
	}
	if cur.Description == nil || strings.TrimSpace(*cur.Description) == "" {
		missing = append(missing, "mô tả")
	}
	// A ride's places are its route, not the listing's own location.
	if cur.ServiceType != rideServiceType {
		if cur.ProvinceCode == nil || cur.WardCode == nil {
			missing = append(missing, "tỉnh/thành và phường/xã")
		}
		if cur.Address == nil || strings.TrimSpace(*cur.Address) == "" {
			missing = append(missing, "địa chỉ")
		}
	}
	q := store.New(tx)
	n, err := q.CountListingImages(ctx, cur.ID)
	if err != nil {
		return fmt.Errorf("count listing images: %w", err)
	}
	if n == 0 {
		missing = append(missing, "ít nhất 1 ảnh")
	}
	switch cur.ServiceType {
	case attractionServiceType:
		tickets, err := q.CountActiveTicketTypes(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("count ticket types: %w", err)
		}
		if tickets == 0 {
			missing = append(missing, "ít nhất 1 loại vé đang bán")
		}
		slots, err := q.CountActiveTimeSlots(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("count time slots: %w", err)
		}
		if slots == 0 {
			missing = append(missing, "ít nhất 1 khung giờ đang hoạt động")
		}
	case rentalServiceType:
		// The location items above are the store address.
		if _, err := q.GetRentalStore(ctx, cur.ID); errors.Is(err, pgx.ErrNoRows) {
			missing = append(missing, "thông tin cửa hàng (giờ giao nhận)")
		} else if err != nil {
			return fmt.Errorf("get rental store: %w", err)
		}
		models, err := q.CountRentableRentalModels(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("count rental models: %w", err)
		}
		if models == 0 {
			missing = append(missing, "ít nhất 1 mẫu xe đang cho thuê")
		}
		points, err := q.CountActiveRentalPickupPoints(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("count rental pickup points: %w", err)
		}
		if points == 0 {
			missing = append(missing, "ít nhất 1 điểm nhận xe đang hoạt động")
		}
	case charterServiceType:
		// The location items above are the operator's address.
		areas, err := q.ListCharterServiceAreas(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("list charter areas: %w", err)
		}
		if len(areas) == 0 {
			missing = append(missing, "khu vực phục vụ (ít nhất 1 tỉnh)")
		}
		classes, err := q.CountActiveCharterClasses(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("count charter classes: %w", err)
		}
		if classes == 0 {
			missing = append(missing, "ít nhất 1 loại xe đang hoạt động")
		}
	case rideServiceType:
		if _, err := q.GetRideRoute(ctx, cur.ID); errors.Is(err, pgx.ErrNoRows) {
			missing = append(missing, "tuyến (điểm đi, điểm đến)")
		} else if err != nil {
			return fmt.Errorf("get ride route: %w", err)
		}
		n, err := q.CountActiveRideSchedules(ctx, store.CountActiveRideSchedulesParams{ListingID: cur.ID, Today: vnDate(s.clock.Now())})
		if err != nil {
			return fmt.Errorf("count ride schedules: %w", err)
		}
		if n == 0 {
			missing = append(missing, "ít nhất 1 lịch chạy đang hoạt động")
		}
	default:
		// No detail model: never "complete".
		return ErrServiceTypeNotSupported
	}
	if len(missing) > 0 {
		return incompleteListing("Listing chưa đủ thông tin để gửi duyệt, còn thiếu: " + strings.Join(missing, "; "))
	}
	return nil
}

// lockAny locks a listing by id only (admin side; callers lock no partner).
func (s *Service) lockAny(ctx context.Context, tx pgx.Tx, id uuid.UUID) (store.Listing, error) {
	r, err := store.New(tx).LockListing(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Listing{}, ErrListingNotFound
	}
	if err != nil {
		return store.Listing{}, fmt.Errorf("lock listing: %w", err)
	}
	return r, nil
}

// adminTransition locks the listing (404), checks the state under the lock
// (409), applies the change and records the admin audit entry in the same tx.
func (s *Service) adminTransition(ctx context.Context, actor, id uuid.UUID, action, reason string, t transition) (Listing, error) {
	now := s.clock.Now()
	var out store.Listing
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.lockAny(ctx, tx, id)
		if err != nil {
			return err
		}
		if !t.check(cur) {
			return ErrInvalidListingState
		}
		if t.validate != nil {
			if err := t.validate(); err != nil {
				return err
			}
		}
		if t.verify != nil {
			if err := t.verify(ctx, tx, cur); err != nil {
				return err
			}
		}
		if out, err = t.apply(store.New(tx), cur, now); err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: action, TargetKind: auditKindListing, TargetID: &id,
			Before: stateOf(cur), After: stateOf(out), Reason: reason,
		})
	})
	if err != nil {
		return Listing{}, err
	}
	return fromRow(out), nil
}

// reasonText trims reason and enforces 1..maxReasonLen runes.
func reasonText(reason string) (string, error) {
	return normalizeText(reason, "reason", true, maxReasonLen)
}

// Approve approves a listing in review; it keeps its visibility. The listing
// must still be complete: a partner may deactivate ticket types or slots, or
// change the gallery, while it waits for review (422 listing_incomplete).
func (s *Service) Approve(ctx context.Context, actor, id uuid.UUID) (Listing, error) {
	return s.adminTransition(ctx, actor, id, "listing.approve", "", transition{
		check:  func(cur store.Listing) bool { return cur.ReviewStatus == ReviewPendingReview },
		verify: s.requireComplete,
		apply: func(q *store.Queries, cur store.Listing, now time.Time) (store.Listing, error) {
			return q.ApproveListing(ctx, store.ApproveListingParams{ID: cur.ID, Now: &now, Actor: &actor})
		},
	})
}

// Reject sends a listing in review back to its partner with a reason.
func (s *Service) Reject(ctx context.Context, actor, id uuid.UUID, reason string) (Listing, error) {
	return s.adminReason(ctx, actor, id, "listing.reject", reason, ReviewPendingReview, "",
		func(q *store.Queries, cur store.Listing, now time.Time, r string) (store.Listing, error) {
			return q.RejectListing(ctx, store.RejectListingParams{ID: cur.ID, Now: &now, Actor: &actor, Reason: &r})
		})
}

// Hide hides an approved listing (published or unpublished by the partner)
// from the public site and remembers that visibility; only an admin can undo
// it.
func (s *Service) Hide(ctx context.Context, actor, id uuid.UUID, reason string) (Listing, error) {
	return s.adminReason(ctx, actor, id, "listing.hide", reason, ReviewApproved, VisibilityHidden,
		func(q *store.Queries, cur store.Listing, now time.Time, r string) (store.Listing, error) {
			return q.HideListing(ctx, store.HideListingParams{ID: cur.ID, Now: &now, Actor: &actor, Reason: &r})
		})
}

// Unhide lifts an admin hide and restores the visibility the listing had
// when it was hidden (a partner-unpublished listing stays unpublished).
func (s *Service) Unhide(ctx context.Context, actor, id uuid.UUID) (Listing, error) {
	return s.adminTransition(ctx, actor, id, "listing.unhide", "", transition{
		check: func(cur store.Listing) bool { return cur.Visibility == VisibilityHidden },
		apply: func(q *store.Queries, cur store.Listing, now time.Time) (store.Listing, error) {
			return q.UnhideListing(ctx, store.UnhideListingParams{ID: cur.ID, Now: now})
		},
	})
}

// adminReason is adminTransition for actions that carry a reason. Order:
// 404 (missing) -> 409 (state) -> 422 (reason). The state check requires
// needStatus and, when notVisibility is set, a different visibility.
func (s *Service) adminReason(ctx context.Context, actor, id uuid.UUID, action, reason, needStatus, notVisibility string,
	apply func(q *store.Queries, cur store.Listing, now time.Time, reason string) (store.Listing, error)) (Listing, error) {
	r, reasonErr := reasonText(reason)
	return s.adminTransition(ctx, actor, id, action, r, transition{
		check: func(cur store.Listing) bool {
			return cur.ReviewStatus == needStatus && (notVisibility == "" || cur.Visibility != notVisibility)
		},
		validate: func() error { return reasonErr },
		apply: func(q *store.Queries, cur store.Listing, now time.Time) (store.Listing, error) {
			return apply(q, cur, now, r)
		},
	})
}

// AdminFilter selects a page of listings for the Super Admin. With
// ReviewStatus pending_review the page is ordered by submitted_at ascending
// (oldest first); otherwise newest first.
type AdminFilter struct {
	ReviewStatus *string
	Visibility   *string
	ServiceType  *string
	PartnerID    *uuid.UUID
	Limit        int
	Cursor       *uuid.UUID
}

func validVisibility(v string) bool {
	return v == VisibilityPublished || v == VisibilityUnpublished || v == VisibilityHidden
}

// AdminList returns a page of listings and the cursor of the next page.
func (s *Service) AdminList(ctx context.Context, f AdminFilter) ([]Listing, *uuid.UUID, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)
	if f.ReviewStatus != nil && !validReviewStatus(*f.ReviewStatus) {
		return nil, nil, httpx.ErrInvalidRequest
	}
	if f.Visibility != nil && !validVisibility(*f.Visibility) {
		return nil, nil, httpx.ErrInvalidRequest
	}
	q := store.New(s.pool)
	var rows []store.Listing
	var err error
	if f.ReviewStatus != nil && *f.ReviewStatus == ReviewPendingReview {
		rows, err = q.ListAdminPendingListings(ctx, store.ListAdminPendingListingsParams{
			Visibility: f.Visibility, ServiceType: f.ServiceType, PartnerID: f.PartnerID,
			Cursor: f.Cursor, Lim: int32(limit + 1),
		})
	} else {
		rows, err = q.ListAdminListings(ctx, store.ListAdminListingsParams{
			ReviewStatus: f.ReviewStatus, Visibility: f.Visibility, ServiceType: f.ServiceType,
			PartnerID: f.PartnerID, Cursor: f.Cursor, Lim: int32(limit + 1),
		})
	}
	if err != nil {
		return nil, nil, fmt.Errorf("list admin listings: %w", err)
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

// AuditEntry is one listing_audit_log row.
type AuditEntry struct {
	ID          uuid.UUID       `json:"id"`
	ActorUserID uuid.UUID       `json:"actor_user_id"`
	Action      string          `json:"action"`
	Changes     json.RawMessage `json:"changes"`
	CreatedAt   time.Time       `json:"created_at"`
}

// AdminListingDetail is everything the Super Admin sees when reviewing.
type AdminListingDetail struct {
	Listing            Listing            `json:"listing"`
	Images             []Image            `json:"images"`
	CancellationPolicy []CancellationTier `json:"cancellation_policy"`
	Attraction         *AttractionDetail  `json:"attraction"`
	Ride               *RideDetail        `json:"ride"`
	Rental             *RentalDetail      `json:"rental"`
	Charter            *CharterDetail     `json:"charter"`
	AuditLog           []AuditEntry       `json:"audit_log"`
}

// AdminGet returns a listing with its images, cancellation policy, the
// per-service-type detail and the latest 20 listing_audit_log rows (newest
// first). Inactive ticket types, slots and ride schedules are included.
func (s *Service) AdminGet(ctx context.Context, id uuid.UUID) (AdminListingDetail, error) {
	q := store.New(s.pool)
	r, err := q.GetListing(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminListingDetail{}, ErrListingNotFound
	}
	if err != nil {
		return AdminListingDetail{}, fmt.Errorf("get listing: %w", err)
	}
	out := AdminListingDetail{Listing: fromRow(r)}
	if out.Images, err = listImages(ctx, s.pool, id); err != nil {
		return out, err
	}
	if out.CancellationPolicy, err = listTiers(ctx, s.pool, id); err != nil {
		return out, err
	}
	switch r.ServiceType {
	case attractionServiceType:
		d, err := s.attractionDetail(ctx, s.pool, id, true)
		if err != nil {
			return out, err
		}
		out.Attraction = &d
	case rideServiceType:
		d, err := s.rideDetail(ctx, s.pool, id, true)
		if err != nil {
			return out, err
		}
		out.Ride = &d
	case rentalServiceType:
		d, err := s.rentalDetail(ctx, s.pool, id, true)
		if err != nil {
			return out, err
		}
		out.Rental = &d
	case charterServiceType:
		d, err := s.charterDetail(ctx, s.pool, id, true)
		if err != nil {
			return out, err
		}
		out.Charter = &d
	default:
		return AdminListingDetail{}, ErrServiceTypeNotSupported
	}
	logs, err := q.ListRecentAuditLog(ctx, id)
	if err != nil {
		return out, fmt.Errorf("list listing audit log: %w", err)
	}
	out.AuditLog = make([]AuditEntry, 0, min(len(logs), recentAuditLimit))
	for _, l := range logs {
		out.AuditLog = append(out.AuditLog, AuditEntry{
			ID: l.ID, ActorUserID: l.ActorUserID, Action: l.Action, Changes: l.Changes, CreatedAt: l.CreatedAt,
		})
	}
	return out, nil
}
