package partner

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/partner/store"
)

// OwnerEmail returns, inside tx, the email of the partner's owner account
// (the earliest owner member), where partner emails such as payout notices
// are sent. A partner without an owner is ErrPartnerNotFound.
func (s *Service) OwnerEmail(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID) (string, error) {
	email, err := store.New(tx).OwnerEmail(ctx, partnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPartnerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("partner owner email: %w", err)
	}
	return email, nil
}
