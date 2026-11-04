package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bangbc-luna/lunain-travel/internal/identity/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/validate"
)

const (
	maxFullNameRunes = 100
	dateLayout       = "2006-01-02"
)

// Profile is a customer's profile. DateOfBirth marshals as YYYY-MM-DD.
type Profile struct {
	UserID      uuid.UUID  `json:"user_id"`
	Email       string     `json:"email"`
	FullName    *string    `json:"full_name"`
	Phone       *string    `json:"phone"`
	DateOfBirth *time.Time `json:"-"`
}

// ProfileUpdate is a partial update: nil keeps the field, "" clears it.
type ProfileUpdate struct {
	FullName    *string `json:"full_name"`
	Phone       *string `json:"phone"`
	DateOfBirth *string `json:"date_of_birth"`
}

// invalidProfileError is a per-call 422 with a field-specific message that
// still satisfies errors.Is(err, ErrInvalidProfile).
type invalidProfileError struct{ app *httpx.AppError }

func (e *invalidProfileError) Error() string { return e.app.Error() }

// Unwrap exposes the specific *AppError so httpx.WriteError (errors.As) writes
// the field-specific message.
func (e *invalidProfileError) Unwrap() error { return e.app }

func (e *invalidProfileError) Is(target error) bool { return target == ErrInvalidProfile }

func invalidProfile(msg string) error {
	return &invalidProfileError{app: httpx.NewError(http.StatusUnprocessableEntity, ErrInvalidProfile.Code, msg)}
}

func normalizeFullName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxFullNameRunes {
		return "", invalidProfile("full_name tối đa 100 ký tự")
	}
	return s, nil
}

func normalizePhone(s string) (string, error) {
	v, ok := validate.NormalizePhone(s)
	if !ok {
		return "", invalidProfile("phone phải gồm 10 chữ số và bắt đầu bằng 0 (hoặc +84)")
	}
	return v, nil
}

func (s *Service) parseDOB(v string) (time.Time, error) {
	d, err := time.Parse(dateLayout, strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, invalidProfile("date_of_birth phải là ngày hợp lệ theo định dạng YYYY-MM-DD")
	}
	n := s.clock.Now().In(clock.VN)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	if !d.Before(today) {
		return time.Time{}, invalidProfile("date_of_birth phải là ngày hợp lệ trong quá khứ")
	}
	return d, nil
}

func profileFromRow(r store.GetCustomerProfileRow) Profile {
	p := Profile{UserID: r.UserID, Email: r.Email, FullName: r.FullName, Phone: r.Phone}
	if r.DateOfBirth.Valid {
		t := r.DateOfBirth.Time
		p.DateOfBirth = &t
	}
	return p
}

// GetProfile returns the customer's profile; internal users have none (404).
func (s *Service) GetProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	row, err := store.New(s.pool).GetCustomerProfile(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, httpx.ErrNotFound
	}
	if err != nil {
		return Profile{}, err
	}
	return profileFromRow(row), nil
}

// UpdateProfile applies a partial update after validating every given field.
func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, u ProfileUpdate) (Profile, error) {
	var fullName, phone *string
	var dob pgtype.Date
	var setName, setPhone, setDOB bool

	if u.FullName != nil {
		setName = true
		v, err := normalizeFullName(*u.FullName)
		if err != nil {
			return Profile{}, err
		}
		if v != "" {
			fullName = &v
		}
	}
	if u.Phone != nil {
		setPhone = true
		if strings.TrimSpace(*u.Phone) != "" {
			v, err := normalizePhone(*u.Phone)
			if err != nil {
				return Profile{}, err
			}
			phone = &v
		}
	}
	if u.DateOfBirth != nil {
		setDOB = true
		if strings.TrimSpace(*u.DateOfBirth) != "" {
			d, err := s.parseDOB(*u.DateOfBirth)
			if err != nil {
				return Profile{}, err
			}
			dob = pgtype.Date{Time: d, Valid: true}
		}
	}

	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetCustomerProfileForUpdate(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		params := store.UpdateCustomerProfileParams{
			FullName: cur.FullName, Phone: cur.Phone, DateOfBirth: cur.DateOfBirth,
			Now: s.clock.Now(), UserID: userID,
		}
		if setName {
			params.FullName = fullName
		}
		if setPhone {
			params.Phone = phone
		}
		if setDOB {
			params.DateOfBirth = dob
		}
		return q.UpdateCustomerProfile(ctx, params)
	})
	if err != nil {
		return Profile{}, err
	}
	return s.GetProfile(ctx, userID)
}

// FillProfileIfEmpty fills only the NULL name/phone fields of the profile
// inside tx. Invalid or empty values are skipped; a missing profile is a no-op.
func (s *Service) FillProfileIfEmpty(ctx context.Context, tx pgx.Tx, userID uuid.UUID, fullName, phone string) error {
	var name, ph *string
	if v, err := normalizeFullName(fullName); err == nil && v != "" {
		name = &v
	}
	if strings.TrimSpace(phone) != "" {
		if v, err := normalizePhone(phone); err == nil {
			ph = &v
		}
	}
	if name == nil && ph == nil {
		return nil
	}
	return store.New(tx).FillCustomerProfileIfEmpty(ctx, store.FillCustomerProfileIfEmptyParams{
		FullName: name, Phone: ph, Now: s.clock.Now(), UserID: userID,
	})
}

// MarshalJSON renders date_of_birth as YYYY-MM-DD.
func (p Profile) MarshalJSON() ([]byte, error) {
	var dob *string
	if p.DateOfBirth != nil {
		s := p.DateOfBirth.Format(dateLayout)
		dob = &s
	}
	return json.Marshal(struct {
		UserID      uuid.UUID `json:"user_id"`
		Email       string    `json:"email"`
		FullName    *string   `json:"full_name"`
		Phone       *string   `json:"phone"`
		DateOfBirth *string   `json:"date_of_birth"`
	}{p.UserID, p.Email, p.FullName, p.Phone, dob})
}
