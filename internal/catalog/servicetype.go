// Package catalog owns service types and the commission rate history.
package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/catalog/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

// ServiceType is one sellable kind of service.
type ServiceType struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	BookingMode string `json:"booking_mode"`
	SortOrder   int    `json:"sort_order"`
}

// Service exposes service types and commission rates.
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool, c clock.Clock) *Service {
	return &Service{pool: pool, clock: c}
}

// ServiceTypes lists all service types in display order.
func (s *Service) ServiceTypes(ctx context.Context) ([]ServiceType, error) {
	rows, err := store.New(s.pool).ListServiceTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list service types: %w", err)
	}
	out := make([]ServiceType, 0, len(rows))
	for _, r := range rows {
		out = append(out, ServiceType{Code: r.Code, Name: r.Name, BookingMode: r.BookingMode, SortOrder: int(r.SortOrder)})
	}
	return out, nil
}

// ServiceType returns one service type inside tx; an unknown code is
// ErrInvalidServiceType.
func (s *Service) ServiceType(ctx context.Context, tx pgx.Tx, code string) (ServiceType, error) {
	r, err := store.New(tx).GetServiceType(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceType{}, ErrInvalidServiceType
	}
	if err != nil {
		return ServiceType{}, fmt.Errorf("get service type: %w", err)
	}
	return ServiceType{Code: r.Code, Name: r.Name, BookingMode: r.BookingMode, SortOrder: int(r.SortOrder)}, nil
}
