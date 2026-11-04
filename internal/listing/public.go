package listing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 50
	maxSearchOffset    = 1000
	maxSearchQuery     = 100
)

// ErrListingNotBookable means the listing is missing or not publicly visible
// (not approved, unpublished, hidden, partner suspended or service type
// disabled), so no booking may be created for it.
var ErrListingNotBookable = httpx.NewError(http.StatusUnprocessableEntity, "listing_not_bookable", "Dịch vụ này hiện không nhận đặt chỗ")

// likeEscaper makes LIKE wildcards in user input literal (ESCAPE '\'). The
// replacer works in one pass, so the backslash is escaped before % and _.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// SearchFilter selects a page of publicly visible listings. Nil or blank
// fields are no filter. Limit defaults to 20 and is capped at 50; Offset is at
// most 1000; Q is trimmed and at most 100 runes.
type SearchFilter struct {
	ServiceType  *string
	ProvinceCode *string
	Q            *string
	Limit        int
	Offset       int
}

// PublicCard is a listing as shown in public search results.
type PublicCard struct {
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	ServiceType  string   `json:"service_type"`
	Summary      *string  `json:"summary"`
	ProvinceCode *string  `json:"province_code"`
	CoverURL     *string  `json:"cover_url"`
	RatingAvg    *float64 `json:"rating_avg"`
	RatingCount  int      `json:"rating_count"`
	PriceFrom    *int64   `json:"price_from"`
}

// PublicDetail is the public listing page. Attraction holds active ticket
// types and slots only.
type PublicDetail struct {
	PublicCard
	Description        *string            `json:"description"`
	Address            *string            `json:"address"`
	WardCode           *string            `json:"ward_code"`
	Latitude           *float64           `json:"latitude"`
	Longitude          *float64           `json:"longitude"`
	ImageURLs          []string           `json:"image_urls"`
	CancellationPolicy []CancellationTier `json:"cancellation_policy"`
	Attraction         *AttractionDetail  `json:"attraction"`
}

func blankToNil(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

// card builds a PublicCard; priceFrom 0 means no active price.
func (s *Service) card(ctx context.Context, tx pgx.Tx, r store.Listing, priceFrom int64) (PublicCard, error) {
	c := PublicCard{
		Slug: r.Slug, Title: r.Title, ServiceType: r.ServiceType, Summary: r.Summary,
		ProvinceCode: r.ProvinceCode, RatingCount: int(r.RatingCount),
	}
	if r.RatingCount > 0 {
		avg := float64(r.RatingSum) / float64(r.RatingCount)
		c.RatingAvg = &avg
	}
	if priceFrom > 0 {
		c.PriceFrom = &priceFrom
	}
	if r.CoverFileID != nil {
		u, err := s.files.PublicURL(ctx, tx, *r.CoverFileID)
		if err != nil {
			return c, fmt.Errorf("cover url of listing %s: %w", r.ID, err)
		}
		c.CoverURL = &u
	}
	return c, nil
}

// Search returns a page of publicly visible listings (spec §5.4). With Q,
// titles match unaccented and case-insensitively (substring, with %, _ and \
// literal, or trigram similarity >= 0.3), best match first; otherwise newest
// first. A bad Limit, Offset or Q is httpx.ErrInvalidRequest.
func (s *Service) Search(ctx context.Context, f SearchFilter) ([]PublicCard, error) {
	if f.Limit < 0 || f.Offset < 0 || f.Offset > maxSearchOffset {
		return nil, httpx.ErrInvalidRequest
	}
	limit := f.Limit
	if limit == 0 {
		limit = defaultSearchLimit
	}
	limit = min(limit, maxSearchLimit)
	params := store.SearchPublicListingsParams{
		ServiceType: blankToNil(f.ServiceType), ProvinceCode: blankToNil(f.ProvinceCode),
		Q: blankToNil(f.Q), Lim: int32(limit), Off: int32(f.Offset),
	}
	if params.Q != nil {
		if utf8.RuneCountInString(*params.Q) > maxSearchQuery {
			return nil, httpx.ErrInvalidRequest
		}
		pattern := likeEscaper.Replace(*params.Q)
		params.Pattern = &pattern
	}
	out := []PublicCard{}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := store.New(tx).SearchPublicListings(ctx, params)
		if err != nil {
			return fmt.Errorf("search listings: %w", err)
		}
		for _, r := range rows {
			c, err := s.card(ctx, tx, r.Listing, r.PriceFrom)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PublicDetail returns the public page of a visible listing by slug. A
// missing or not visible listing is ErrListingNotFound.
func (s *Service) PublicDetail(ctx context.Context, slug string) (PublicDetail, error) {
	var out PublicDetail
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := store.New(tx).GetPublicListingBySlug(ctx, slug)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrListingNotFound
		}
		if err != nil {
			return fmt.Errorf("get public listing: %w", err)
		}
		r := row.Listing
		if out.PublicCard, err = s.card(ctx, tx, r, row.PriceFrom); err != nil {
			return err
		}
		out.Description, out.Address, out.WardCode = r.Description, r.Address, r.WardCode
		out.Latitude, out.Longitude = r.Latitude, r.Longitude
		images, err := listImages(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		out.ImageURLs = make([]string, 0, len(images))
		for _, im := range images {
			u, err := s.files.PublicURL(ctx, tx, im.FileID)
			if err != nil {
				return fmt.Errorf("image url of listing %s: %w", r.ID, err)
			}
			out.ImageURLs = append(out.ImageURLs, u)
		}
		if out.CancellationPolicy, err = listTiers(ctx, tx, r.ID); err != nil {
			return err
		}
		if r.ServiceType == attractionServiceType {
			d, err := s.attractionDetail(ctx, tx, r.ID, false)
			if err != nil {
				return err
			}
			out.Attraction = &d
		}
		return nil
	})
	if err != nil {
		return PublicDetail{}, err
	}
	return out, nil
}

// BookableListing checks, inside the caller's tx, that a listing can be
// booked now, and returns it. Plan 4 MUST call it inside the booking-creation
// tx, then TicketTypesForBooking / SlotsForDate in the same tx, and reject a
// booking with no ticket lines.
//
// Lock order (DB §1.1: partners -> listings): it reads partner_id unlocked
// (safe: a listing's partner_id never changes),
// takes the partners row FOR SHARE via Partners.Status (a concurrent Suspend
// or DisableServiceType waits for the booking tx), then the listings row FOR
// SHARE (a concurrent partner edit or admin hide waits too), and only then
// checks the public visibility rule and IsServiceTypeEnabled. A missing or
// not visible listing is ErrListingNotBookable.
func (s *Service) BookableListing(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) (Listing, error) {
	q := store.New(tx)
	partnerID, err := q.ListingPartnerID(ctx, listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, ErrListingNotBookable
	}
	if err != nil {
		return Listing{}, fmt.Errorf("listing partner: %w", err)
	}
	st, err := s.partners.Status(ctx, tx, partnerID, true)
	if err != nil {
		return Listing{}, err
	}
	r, err := q.ShareListing(ctx, listingID)
	if err != nil {
		// The row exists (partner_id was read) and listings are never deleted.
		return Listing{}, fmt.Errorf("share listing: %w", err)
	}
	if st != partner.StatusActive || r.ReviewStatus != ReviewApproved || r.Visibility != VisibilityPublished {
		return Listing{}, ErrListingNotBookable
	}
	enabled, err := s.partners.IsServiceTypeEnabled(ctx, tx, partnerID, r.ServiceType)
	if err != nil {
		return Listing{}, err
	}
	if !enabled {
		return Listing{}, ErrListingNotBookable
	}
	return fromRow(r), nil
}
