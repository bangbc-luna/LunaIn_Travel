package listing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

var testNow = time.Date(2026, 11, 4, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

const attraction = "attraction_ticket"

// env wires real identity, catalog, geo, files, partner and listing services
// on a fresh Postgres database with a fixed clock.
type env struct {
	Svc      *listing.Service
	Partner  *partner.Service
	Identity *identity.Service
	Issuer   *auth.Issuer
	Pool     *pgxpool.Pool
	Files    *files.Service
	Clock    *clock.Fixed
	Admin    uuid.UUID
	Handler  http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: testNow}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	idSvc := identity.NewService(identity.Deps{
		Pool: pool, Issuer: iss, Clock: clk,
		CodePepper: []byte("test-pepper"), RefreshTTL: 30 * 24 * time.Hour,
	})
	cat := catalog.NewService(pool, clk)
	geoSvc := geo.NewService(pool)
	fileSvc := files.NewService(pool, storage.NewMemory(), clk, "http://files.test")
	pSvc := partner.NewService(partner.Deps{
		Pool: pool, Clock: clk, Users: idSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc,
	})
	svc := listing.NewService(listing.Deps{
		Pool: pool, Clock: clk, Partners: pSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc,
	})
	mux := http.NewServeMux()
	svc.PartnerRoutes(mux, iss, pSvc.RequireMember())
	svc.AdminRoutes(mux, iss)
	svc.PublicRoutes(mux)
	mux.HandleFunc("/", httpx.NotFound)
	e := &env{
		Svc: svc, Files: fileSvc, Partner: pSvc, Identity: idSvc, Issuer: iss, Pool: pool, Clock: clk,
		Handler: httpx.Chain(mux, httpx.RequestID, httpx.Recover),
	}
	require.NoError(t, db.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		e.Admin, err = idSvc.CreateInternalUser(context.Background(), tx, "admin@lunain.vn", "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	return e
}

// partner creates a partner named n with the given enabled service types.
func (e *env) partner(t *testing.T, n string, serviceTypes ...string) partner.Partner {
	t.Helper()
	key := strings.ToLower(n)
	p, err := e.Partner.Create(context.Background(), e.Admin, partner.CreateInput{
		LegalName: "Công ty " + n, DisplayName: n,
		OwnerEmail: "owner@" + key + ".vn", OwnerPassword: "matkhau123",
		ServiceTypes: serviceTypes,
	})
	require.NoError(t, err)
	return p
}

func (e *env) create(t *testing.T, p partner.Partner, title string) listing.Listing {
	t.Helper()
	l, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: attraction, Title: title})
	require.NoError(t, err)
	return l
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

type auditRow struct {
	Action  string
	Actor   uuid.UUID
	Changes map[string]struct {
		Old any `json:"old"`
		New any `json:"new"`
	}
}

// audits returns the listing_audit_log rows of a listing, oldest first.
func (e *env) audits(t *testing.T, listingID uuid.UUID) []auditRow {
	t.Helper()
	rows, err := e.Pool.Query(context.Background(),
		`SELECT action, actor_user_id, changes FROM listing_audit_log WHERE listing_id = $1 ORDER BY created_at, id`, listingID)
	require.NoError(t, err)
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var raw []byte
		require.NoError(t, rows.Scan(&r.Action, &r.Actor, &raw))
		require.NoError(t, json.Unmarshal(raw, &r.Changes))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// location returns a ward of some province and a province it does not belong to.
func (e *env) location(t *testing.T) (province, ward, otherProvince string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT province_code, code FROM wards ORDER BY code LIMIT 1`).Scan(&province, &ward))
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT code FROM provinces WHERE code <> $1 ORDER BY code LIMIT 1`, province).Scan(&otherProvince))
	return province, ward, otherProvince
}

func (e *env) token(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	s, _, err := e.Issuer.Issue(auth.Principal{UserID: userID, Role: auth.RolePartner})
	require.NoError(t, err)
	return s
}

// do sends a JSON request through the real mux.
func (e *env) do(t *testing.T, method, path, tok string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		require.NoError(t, json.NewEncoder(&buf).Encode(b))
	}
	req := httptest.NewRequest(method, path, &buf)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return rec
}

func requireCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	var eb struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &eb), rec.Body.String())
	require.Equal(t, code, eb.Error.Code)
}

func appCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae), "not an AppError: %v", err)
	return ae.Status, ae.Code
}

func ptr[T any](v T) *T { return &v }

// upload stores a 1x1 png uploaded by userID with the given purpose.
func (e *env) upload(t *testing.T, userID uuid.UUID, purpose string) uuid.UUID {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	f, err := e.Files.Upload(context.Background(), auth.Principal{UserID: userID, Role: auth.RolePartner}, purpose, &buf)
	require.NoError(t, err)
	return f.ID
}

// rawListing inserts a draft listing of any service type straight into the
// table (Create refuses unsupported types) with every common field set.
func (e *env) rawListing(t *testing.T, p partner.Partner, serviceType, title string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	province, ward, _ := e.location(t)
	id := uuid.Must(uuid.NewV7())
	_, err := e.Pool.Exec(ctx, `INSERT INTO listings (id, partner_id, service_type, title, slug, description,
		province_code, ward_code, address) VALUES ($1, $2, $3, $4, $5, 'Mô tả', $6, $7, '1 Đường A')`,
		id, p.ID, serviceType, title, "raw-"+id.String(), province, ward)
	require.NoError(t, err)
	return id
}
