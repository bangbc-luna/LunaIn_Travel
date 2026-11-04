package partner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/partner/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/validate"
)

const (
	purposeBusinessLicense = "business_license"
	taxCodeConstraint      = "partners_tax_code_key"
	pgUniqueViolation      = "23505"
)

// ProfileUpdate is a partner's partial self-service update: nil keeps a
// field, "" clears an optional one. legal_name is admin-only and absent on
// purpose. The JSON tags are the PATCH /v1/partner/profile body; unknown
// fields such as partner_id are rejected by httpx.DecodeJSON.
type ProfileUpdate struct {
	DisplayName           *string    `json:"display_name"`
	TaxCode               *string    `json:"tax_code"`
	Address               *string    `json:"address"`
	ProvinceCode          *string    `json:"province_code"`
	WardCode              *string    `json:"ward_code"`
	ContactName           *string    `json:"contact_name"`
	ContactPhone          *string    `json:"contact_phone"`
	ContactEmail          *string    `json:"contact_email"`
	BusinessLicenseFileID *uuid.UUID `json:"business_license_file_id"`
}

// normalizeContactPhone applies identity's phone rule; "" stays empty.
func normalizeContactPhone(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	v, ok := validate.NormalizePhone(s)
	if !ok {
		return "", invalidPartner("contact_phone phải gồm 10 chữ số và bắt đầu bằng 0 (hoặc +84)")
	}
	return v, nil
}

// normalizeContactEmail applies identity.NormalizeEmail; "" stays empty.
func normalizeContactEmail(s string) (string, error) {
	s, err := normalizeText(s, "contact_email", false, maxTextLen)
	if err != nil || s == "" {
		return s, err
	}
	if s, err = identity.NormalizeEmail(s); err != nil {
		return "", invalidPartner("contact_email không hợp lệ")
	}
	return s, nil
}

// optionalText normalizes an optional nullable column: "" becomes NULL.
func optionalText(s, field string) (*string, error) {
	v, err := normalizeText(s, field, false, maxTextLen)
	if err != nil || v == "" {
		return nil, err
	}
	return &v, nil
}

// applyProfile validates u and merges it into the locked row cur.
func applyProfile(cur store.Partner, u ProfileUpdate) (store.UpdateProfileParams, error) {
	p := store.UpdateProfileParams{
		ID: cur.ID, DisplayName: cur.DisplayName, TaxCode: cur.TaxCode,
		BusinessLicenseFileID: cur.BusinessLicenseFileID, Address: cur.Address,
		ProvinceCode: cur.ProvinceCode, WardCode: cur.WardCode,
		ContactName: cur.ContactName, ContactPhone: cur.ContactPhone, ContactEmail: cur.ContactEmail,
	}
	var err error
	if u.DisplayName != nil {
		if p.DisplayName, err = normalizeText(*u.DisplayName, "display_name", true, maxTextLen); err != nil {
			return p, err
		}
	}
	if u.TaxCode != nil {
		if p.TaxCode, err = optionalText(*u.TaxCode, "tax_code"); err != nil {
			return p, err
		}
	}
	if u.Address != nil {
		if p.Address, err = optionalText(*u.Address, "address"); err != nil {
			return p, err
		}
	}
	if u.ContactName != nil {
		if p.ContactName, err = normalizeText(*u.ContactName, "contact_name", false, maxTextLen); err != nil {
			return p, err
		}
	}
	if u.ContactPhone != nil {
		if p.ContactPhone, err = normalizeContactPhone(*u.ContactPhone); err != nil {
			return p, err
		}
	}
	if u.ContactEmail != nil {
		if p.ContactEmail, err = normalizeContactEmail(*u.ContactEmail); err != nil {
			return p, err
		}
	}
	// Province and ward travel together so the stored pair stays consistent.
	if (u.ProvinceCode == nil) != (u.WardCode == nil) {
		return p, geo.ErrInvalidLocation
	}
	if u.ProvinceCode != nil {
		prov, ward := strings.TrimSpace(*u.ProvinceCode), strings.TrimSpace(*u.WardCode)
		if (prov == "") != (ward == "") {
			return p, geo.ErrInvalidLocation
		}
		p.ProvinceCode, p.WardCode = nil, nil
		if prov != "" {
			p.ProvinceCode, p.WardCode = &prov, &ward
		}
	}
	if u.BusinessLicenseFileID != nil {
		p.BusinessLicenseFileID = pgtype.UUID{Bytes: *u.BusinessLicenseFileID, Valid: true}
	}
	return p, nil
}

// UpdateProfile applies a partner's self-service profile update. Suspended
// partners may still edit their profile. A ward must belong to its province,
// the license file must be a business_license uploaded by actorUserID, and a
// tax_code used by another partner is ErrTaxCodeTaken.
func (s *Service) UpdateProfile(ctx context.Context, partnerID, actorUserID uuid.UUID, u ProfileUpdate) (Partner, error) {
	var out Partner
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetPartnerForUpdate(ctx, partnerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPartnerNotFound
		}
		if err != nil {
			return fmt.Errorf("lock partner: %w", err)
		}
		params, err := applyProfile(cur, u)
		if err != nil {
			return err
		}
		if u.ProvinceCode != nil && params.ProvinceCode != nil {
			if err := s.geo.ValidateLocation(ctx, tx, *params.ProvinceCode, *params.WardCode); err != nil {
				return err
			}
		}
		if u.BusinessLicenseFileID != nil {
			if err := s.files.Owned(ctx, tx, *u.BusinessLicenseFileID, actorUserID, purposeBusinessLicense); err != nil {
				return err
			}
		}
		params.Now = s.clock.Now()
		if err := q.UpdateProfile(ctx, params); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == taxCodeConstraint {
				return ErrTaxCodeTaken
			}
			return fmt.Errorf("update partner profile: %w", err)
		}
		out, err = get(ctx, q, partnerID)
		return err
	})
	if err != nil {
		return Partner{}, err
	}
	return out, nil
}
