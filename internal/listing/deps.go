// Package listing owns listings (the common layer of every sellable service)
// and their per-service-type details. It reads other modules only through the
// narrow interfaces below.
package listing

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

// PartnerChecker reads a partner's status and enabled service types inside the
// caller's tx. Implemented by *partner.Service. Call Status(ctx, tx, id, true)
// (FOR SHARE) before IsServiceTypeEnabled in the same tx (DB §1.1).
type PartnerChecker interface {
	Status(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, lock bool) (string, error)
	IsServiceTypeEnabled(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, code string) (bool, error)
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

// FileChecker checks an uploaded file's owner and purpose, and resolves the
// fixed URL of a public file (listing images). Implemented by *files.Service.
type FileChecker interface {
	Owned(ctx context.Context, tx pgx.Tx, id, uploadedBy uuid.UUID, purpose string) error
	PublicURL(ctx context.Context, tx pgx.Tx, id uuid.UUID) (string, error)
}

// BookingGuard counts active (pending_payment or confirmed) bookings so the
// partner cannot move or close a schedule that customers already hold.
// Implemented by booking.Guard. Callers hold the listing FOR NO KEY UPDATE in
// tx before asking (see AddClosedDate). date and from are read as Vietnam
// calendar dates.
type BookingGuard interface {
	ActiveBookingsOnDate(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, date time.Time) (int, error)
	ActiveBookingsForSlot(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, from time.Time) (int, error)
}

// Deps are the collaborators of Service.
type Deps struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Partners PartnerChecker
	Catalog  ServiceTypeChecker
	Geo      LocationValidator
	Files    FileChecker
	// BookingGuard may be nil (tests without bookings): the checks are skipped.
	BookingGuard BookingGuard
}

// Service implements the listing use cases.
type Service struct {
	pool     *pgxpool.Pool
	clock    clock.Clock
	partners PartnerChecker
	catalog  ServiceTypeChecker
	geo      LocationValidator
	files    FileChecker
	bookings BookingGuard
}

// NewService builds a Service.
func NewService(d Deps) *Service {
	return &Service{pool: d.Pool, clock: d.Clock, partners: d.Partners, catalog: d.Catalog, geo: d.Geo, files: d.Files, bookings: d.BookingGuard}
}
