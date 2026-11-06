// Package listing owns listings (the common layer of every sellable service)
// and their per-service-type details. It reads other modules only through the
// narrow interfaces below.
package listing

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
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
// partner cannot move or close a schedule that customers already hold, nor
// lower a rental quantity below the vehicles held.
// Implemented by booking.Guard. Callers hold the listing FOR NO KEY UPDATE in
// tx before asking (see AddClosedDate). date and from are read as Vietnam
// calendar dates.
type BookingGuard interface {
	ActiveBookingsOnDate(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, date time.Time) (int, error)
	// weekdays is a mask (Monday = bit 0 ... Sunday = bit 6): only visit
	// dates on those weekdays count.
	ActiveBookingsForSlot(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, from time.Time, weekdays int) (int, error)
	// RentalPeakHeld returns, per vehicle model, the peak number of bookings
	// holding a vehicle at the same instant inside [from, to) (to nil:
	// open-ended), as seen at now; a model without holds is absent. The
	// caller holds the models FOR UPDATE in tx.
	RentalPeakHeld(ctx context.Context, tx pgx.Tx, modelIDs []uuid.UUID, from time.Time, to *time.Time, now time.Time) (map[uuid.UUID]int, error)
}

// HolidayChecker reports, inside the caller's tx, whether the Vietnam calendar
// date of d is a holiday. Implemented by *settings.Service.
type HolidayChecker interface {
	IsHoliday(ctx context.Context, tx pgx.Tx, d time.Time) (bool, error)
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
	// Maps prices private charters (road distance). Never called inside a
	// transaction. May be nil when charters are not quoted: quotes then fail
	// with ErrMapUnavailable.
	Maps maps.Provider
	// Holidays may be nil when charters are not quoted.
	Holidays HolidayChecker
	// Log may be nil: slog.Default() is used.
	Log *slog.Logger
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
	maps     maps.Provider
	holidays HolidayChecker
	log      *slog.Logger
}

// NewService builds a Service.
func NewService(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: d.Pool, clock: d.Clock, partners: d.Partners, catalog: d.Catalog, geo: d.Geo, files: d.Files, bookings: d.BookingGuard,
		maps: d.Maps, holidays: d.Holidays, log: log}
}
