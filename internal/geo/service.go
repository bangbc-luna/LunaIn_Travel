// Package geo owns the catalog of Vietnamese administrative units after the
// 2025 merger (Quyết định 19/2025/QĐ-TTg): provinces and commune-level wards.
package geo

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/geo/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Business errors.
var (
	ErrProvinceNotFound = httpx.NewError(http.StatusNotFound, "province_not_found", "Không tìm thấy tỉnh/thành phố")
	ErrInvalidLocation  = httpx.NewError(http.StatusUnprocessableEntity, "invalid_location", "Phường/xã không thuộc tỉnh/thành phố đã chọn")
)

// Province is a province-level unit.
type Province struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Ward is a commune-level unit; Kind is xa, phuong or dac_khu.
type Ward struct {
	Code         string `json:"code"`
	ProvinceCode string `json:"province_code"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Slug         string `json:"slug"`
}

// Service reads the administrative units catalog.
type Service struct {
	pool *pgxpool.Pool
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Provinces lists all provinces ordered by sort_order.
func (s *Service) Provinces(ctx context.Context) ([]Province, error) {
	rows, err := store.New(s.pool).ListProvinces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list provinces: %w", err)
	}
	out := make([]Province, len(rows))
	for i, r := range rows {
		out[i] = Province{Code: r.Code, Name: r.Name, Slug: r.Slug}
	}
	return out, nil
}

// Wards lists the wards of provinceCode ordered by name. An unknown province
// is ErrProvinceNotFound.
func (s *Service) Wards(ctx context.Context, provinceCode string) ([]Ward, error) {
	q := store.New(s.pool)
	ok, err := q.ProvinceExists(ctx, provinceCode)
	if err != nil {
		return nil, fmt.Errorf("find province: %w", err)
	}
	if !ok {
		return nil, ErrProvinceNotFound
	}
	rows, err := q.ListWardsByProvince(ctx, provinceCode)
	if err != nil {
		return nil, fmt.Errorf("list wards: %w", err)
	}
	out := make([]Ward, len(rows))
	for i, r := range rows {
		out[i] = Ward{Code: r.Code, ProvinceCode: r.ProvinceCode, Name: r.Name, Kind: r.Kind, Slug: r.Slug}
	}
	return out, nil
}

// ValidateLocation returns ErrInvalidLocation unless wardCode is a ward of
// provinceCode. It runs inside the caller's transaction.
func (s *Service) ValidateLocation(ctx context.Context, tx pgx.Tx, provinceCode, wardCode string) error {
	ok, err := store.New(tx).WardInProvince(ctx, store.WardInProvinceParams{Code: wardCode, ProvinceCode: provinceCode})
	if err != nil {
		return fmt.Errorf("validate location: %w", err)
	}
	if !ok {
		return ErrInvalidLocation
	}
	return nil
}
