package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

// Request Booking lifecycle shared by every service type sold on request
// (spec §6.2): the booking is created requested, the partner accepts or
// rejects it before response_due_at, the customer pays before
// payment_due_at. What is specific to a service type (its capacity) lives in
// capacity.go.

// Request Booking statuses and mode.
const (
	StatusRequested       = "requested"
	StatusAwaitingPayment = "awaiting_payment"
	StatusInService       = "in_service"
	StatusRejected        = "rejected"

	bookingModeRequest = "request"
)

// PartnerContact finds where partner emails are sent (*partner.Service).
type PartnerContact interface {
	OwnerEmail(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID) (string, error)
}

// requestWithdrawable reports whether the customer may still withdraw a
// Request Booking for free: it is requested or accepted but not yet paid.
func requestWithdrawable(b store.Booking) bool {
	return b.BookingMode == bookingModeRequest && (b.Status == StatusRequested || b.Status == StatusAwaitingPayment)
}

// requestResponseDue is the deadline for the partner to answer a request
// made at now for a service starting at serviceStart: request_response_hours
// after now, but no later than request_payment_hours before the start, so an
// accepted request always leaves the customer the full time to pay.
func requestResponseDue(cfg settings.Settings, now, serviceStart time.Time) time.Time {
	due := now.Add(time.Duration(cfg.RequestResponseHours) * time.Hour)
	if latest := serviceStart.Add(-time.Duration(cfg.RequestPaymentHours) * time.Hour); latest.Before(due) {
		return latest
	}
	return due
}

// insertRequestedBooking inserts a requested Request Booking (no hold
// columns; p.ResponseDueAt set) with a fresh id and LT code (p.ID and p.Code
// are set here) and returns both, retrying on a code collision (ON CONFLICT
// DO NOTHING keeps the tx usable).
func insertRequestedBooking(ctx context.Context, q *store.Queries, p store.InsertRequestedBookingParams) (uuid.UUID, string, error) {
	if p.ResponseDueAt == nil {
		return uuid.Nil, "", errors.New("booking: a requested booking needs response_due_at")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, "", err
	}
	p.ID = id
	for range codeInsertTries {
		if p.Code, err = randomCode("LT"); err != nil {
			return uuid.Nil, "", err
		}
		n, err := q.InsertRequestedBooking(ctx, p)
		if err != nil {
			return uuid.Nil, "", fmt.Errorf("insert requested booking: %w", err)
		}
		if n == 1 {
			return id, p.Code, nil
		}
	}
	return uuid.Nil, "", errors.New("booking code: too many collisions")
}

// emailPartner queues an email to the partner's owner in tx (outbox). Without
// a PartnerContact (tests) it is only logged.
func (s *Service) emailPartner(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, subject, text string) error {
	if s.partners == nil {
		s.log.Info("booking: partner email not queued (no partner contact)", "partner_id", partnerID, "subject", subject)
		return nil
	}
	to, err := s.partners.OwnerEmail(ctx, tx, partnerID)
	if err != nil {
		return err
	}
	return s.enqueueEmail(ctx, tx, mailer.Message{To: to, Subject: subject, Text: text})
}
