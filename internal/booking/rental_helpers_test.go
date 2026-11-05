package booking_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

const rentalType = "self_drive_rental"

// rentalEnv is env plus a rental partner with the approved listing "Thuê xe
// Đà Lạt" in Lâm Đồng (68), a store open 06:00-22:00, commission 1000 bps on
// self_drive_rental, policy [{48,10000},{24,5000}], the models Vios
// (800.000/day, quantity 1) and Vision (150.000/day, quantity 2), and the
// pickup points P0 "Cửa hàng" (fee 0) and P1 "Sân bay Liên Khương" (fee
// 100.000).
type rentalEnv struct {
	*env
	RP     partner.Partner
	RL     listing.Listing
	Vios   listing.RentalModel
	Vision listing.RentalModel
	P0, P1 listing.PickupPoint
}

func newRentalEnv(t *testing.T) *rentalEnv {
	t.Helper()
	ctx := context.Background()
	e := newEnv(t)
	_, err := e.Catalog.CreateCommissionRate(ctx, e.Admin, rentalType, 1000, nil)
	require.NoError(t, err)
	r := &rentalEnv{env: e}
	r.RP, err = e.Partner.Create(ctx, e.Admin, partner.CreateInput{
		LegalName: "Công ty Thuê Xe Đà Lạt", DisplayName: "Thuê Xe Đà Lạt",
		OwnerEmail: "owner@thuexe.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{rentalType},
	})
	require.NoError(t, err)
	p := r.RP
	desc := "Xe tự lái"
	l, err := e.Listing.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: rentalType, Title: "Thuê xe Đà Lạt", Description: &desc})
	require.NoError(t, err)
	ward, province, addr := r.wardOf(t, "68"), "68", "1 Hoàng Văn Thụ"
	_, err = e.Listing.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{ProvinceCode: &province, WardCode: &ward, Address: &addr})
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	f, err := e.Files.Upload(ctx, auth.Principal{UserID: p.OwnerUserID, Role: auth.RolePartner}, "listing_image", &buf)
	require.NoError(t, err)
	_, _, err = e.Listing.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f.ID}, nil)
	require.NoError(t, err)
	_, err = e.Listing.SetRentalStore(ctx, p.ID, p.OwnerUserID, l.ID, listing.RentalStoreInput{HandoverOpen: "06:00", HandoverClose: "22:00"})
	require.NoError(t, err)
	r.Vios = r.rentalModel(t, l.ID, "car", "Toyota Vios", 5, 800_000, 1)
	r.Vision = r.rentalModel(t, l.ID, "motorbike", "Honda Vision", 2, 150_000, 2)
	r.P0 = r.point(t, l.ID, "Cửa hàng", "1 Hoàng Văn Thụ", 0)
	r.P1 = r.point(t, l.ID, "Sân bay Liên Khương", "Sân bay Liên Khương, Đức Trọng", 100_000)
	_, err = e.Listing.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, []listing.CancellationTier{
		{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000},
	})
	require.NoError(t, err)
	_, err = e.Listing.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	r.RL, err = e.Listing.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	return r
}

func (r *rentalEnv) wardOf(t *testing.T, province string) string {
	t.Helper()
	var w string
	require.NoError(t, r.Pool.QueryRow(context.Background(),
		`SELECT code FROM wards WHERE province_code = $1 ORDER BY code LIMIT 1`, province).Scan(&w))
	return w
}

func (r *rentalEnv) rentalModel(t *testing.T, listingID uuid.UUID, kind, name string, seats int, price int64, qty int) listing.RentalModel {
	t.Helper()
	m, err := r.Listing.CreateRentalModel(context.Background(), r.RP.ID, r.RP.OwnerUserID, listingID, listing.RentalModelInput{
		VehicleKind: kind, Name: name, ModelYear: 2023, Seats: seats, PricePerDay: price, Quantity: qty,
	})
	require.NoError(t, err)
	return m
}

func (r *rentalEnv) point(t *testing.T, listingID uuid.UUID, name, addr string, fee int64) listing.PickupPoint {
	t.Helper()
	pp, err := r.Listing.CreatePickupPoint(context.Background(), r.RP.ID, r.RP.OwnerUserID, listingID, listing.PickupPointInput{
		Name: name, Address: addr, ProvinceCode: "68", WardCode: r.wardOf(t, "68"), DeliveryFee: fee,
	})
	require.NoError(t, err)
	return pp
}

// rentalInput requests model at point over [pickup, ret), destination Lâm Đồng (68).
func (r *rentalEnv) rentalInput(model, point uuid.UUID, pickup, ret time.Time) booking.CreateRentalInput {
	return booking.CreateRentalInput{
		ListingID: r.RL.ID, VehicleModelID: model, PickupPointID: point,
		PickupAt: pickup, ReturnAt: ret, DestinationProvinceCode: "68",
		ContactName: "Lê Văn Cường", ContactPhone: "0901 234 567", ContactEmail: "Cuong@Gmail.com",
	}
}

func (r *rentalEnv) createRental(t *testing.T, customer uuid.UUID, key string, in booking.CreateRentalInput) booking.BookingView {
	t.Helper()
	v, err := r.Svc.CreateRental(context.Background(), customer, key, in)
	require.NoError(t, err)
	return v
}

// rentalHold reads holds_vehicle and hold_until of a rental booking.
func (r *rentalEnv) rentalHold(t *testing.T, bookingID uuid.UUID) (bool, *time.Time) {
	t.Helper()
	var (
		holds bool
		until *time.Time
	)
	require.NoError(t, r.Pool.QueryRow(context.Background(),
		`SELECT holds_vehicle, hold_until FROM booking_rental_details WHERE booking_id = $1`, bookingID).Scan(&holds, &until))
	return holds, until
}
