// Package audit records Super Admin write operations in admin_audit_log.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Entry describes one admin action. Before/After are marshaled to JSON; nil
// leaves the column NULL. An empty Reason is stored as NULL.
type Entry struct {
	ActorUserID uuid.UUID
	Action      string
	TargetKind  string
	TargetID    *uuid.UUID
	Before      any
	After       any
	Reason      string
}

// Record inserts e inside tx so the audit row commits or rolls back together
// with the change it describes.
func Record(ctx context.Context, tx pgx.Tx, now time.Time, e Entry) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("audit id: %w", err)
	}
	before, err := marshalNullable(e.Before)
	if err != nil {
		return fmt.Errorf("audit before: %w", err)
	}
	after, err := marshalNullable(e.After)
	if err != nil {
		return fmt.Errorf("audit after: %w", err)
	}
	var reason *string
	if e.Reason != "" {
		reason = &e.Reason
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO admin_audit_log
			(id, actor_user_id, action, target_kind, target_id, before, after, reason, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		id, e.ActorUserID, e.Action, e.TargetKind, e.TargetID, before, after, reason, now)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// marshalNullable returns nil (SQL NULL) for a nil value, else the JSON text.
func marshalNullable(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}
