package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/identity/store"
)

// SuperAdminEmails returns, inside tx, the email of every active
// super_admin, sorted. It takes no lock (alerts only read them).
func (s *Service) SuperAdminEmails(ctx context.Context, tx pgx.Tx) ([]string, error) {
	out, err := store.New(tx).ListActiveSuperAdminEmails(ctx)
	if err != nil {
		return nil, fmt.Errorf("list super admin emails: %w", err)
	}
	return out, nil
}

// EmailOf returns the email of a user, inside tx. An unknown user is an
// error (callers only pass ids taken from foreign keys).
func (s *Service) EmailOf(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (string, error) {
	email, err := store.New(tx).GetUserEmail(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("identity: user %s not found", userID)
	}
	if err != nil {
		return "", fmt.Errorf("get user email: %w", err)
	}
	return email, nil
}
