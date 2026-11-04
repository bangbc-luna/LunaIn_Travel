package files_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

const baseURL = "http://cdn.test/lunain"

type env struct {
	Svc  *files.Service
	Pool *pgxpool.Pool
	Mem  *storage.Memory
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.Postgres(t)
	mem := storage.NewMemory()
	clk := &clock.Fixed{T: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	return &env{Svc: files.NewService(pool, mem, clk, baseURL), Pool: pool, Mem: mem}
}

// user inserts a user with the given role and returns its principal.
func (e *env) user(t *testing.T, role auth.Role) auth.Principal {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	_, err := e.Pool.Exec(context.Background(),
		`INSERT INTO users (id, email, role) VALUES ($1, $2, $3)`, id, id.String()+"@x.vn", string(role))
	require.NoError(t, err)
	return auth.Principal{UserID: id, Role: role}
}

func (e *env) fileCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT count(*) FROM files`).Scan(&n))
	return n
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	return buf.Bytes()
}

func appCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae), "not an AppError: %v", err)
	return ae.Status, ae.Code
}

func TestUpload_ListingImageByPartnerIsPublic(t *testing.T) {
	e := newEnv(t)
	p := e.user(t, auth.RolePartner)

	f, err := e.Svc.Upload(context.Background(), p, "listing_image", bytes.NewReader(pngBytes(t)))
	require.NoError(t, err)
	require.Equal(t, "public", f.Visibility)
	require.Equal(t, "image/png", f.ContentType)
	require.Equal(t, p.UserID, f.UploadedBy)
	require.True(t, strings.HasPrefix(f.URL, baseURL+"/public/listing_image/2026/11/"), f.URL)
	require.True(t, strings.HasSuffix(f.URL, f.ID.String()+".png"), f.URL)
	key := strings.TrimPrefix(f.URL, baseURL+"/")
	require.True(t, e.Mem.Has(key), "object missing in storage")
	require.Equal(t, 1, e.fileCount(t))
}

func TestUpload_RejectsDisguisedFile(t *testing.T) {
	e := newEnv(t)
	p := e.user(t, auth.RolePartner)
	exe := append([]byte("MZ\x90\x00\x03\x00\x00\x00"), make([]byte, 200)...)

	_, err := e.Svc.Upload(context.Background(), p, "listing_image", bytes.NewReader(exe))
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_file", code)
	require.Zero(t, e.Mem.Len())
	require.Zero(t, e.fileCount(t))

	// a PDF is a real type but not allowed for listing images
	_, err = e.Svc.Upload(context.Background(), p, "listing_image", strings.NewReader("%PDF-1.4\n"+strings.Repeat("x", 100)))
	_, code = appCode(t, err)
	require.Equal(t, "invalid_file", code)
}

func TestUpload_RejectsOversize(t *testing.T) {
	e := newEnv(t)
	p := e.user(t, auth.RolePartner)
	head := pngBytes(t)
	big := append(append([]byte{}, head...), make([]byte, files.MaxSize+1-len(head))...)
	require.Len(t, big, files.MaxSize+1)

	_, err := e.Svc.Upload(context.Background(), p, "listing_image", bytes.NewReader(big))
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_file", code)
	require.Zero(t, e.Mem.Len())
	require.Zero(t, e.fileCount(t))

	// exactly at the limit is accepted
	ok := append(append([]byte{}, head...), make([]byte, files.MaxSize-len(head))...)
	_, err = e.Svc.Upload(context.Background(), p, "listing_image", bytes.NewReader(ok))
	require.NoError(t, err)
}

func TestUpload_RejectsWrongRoleAndPurpose(t *testing.T) {
	e := newEnv(t)
	c := e.user(t, auth.RoleCustomer)

	_, err := e.Svc.Upload(context.Background(), c, "business_license", bytes.NewReader(pngBytes(t)))
	status, code := appCode(t, err)
	require.Equal(t, http.StatusForbidden, status)
	require.Equal(t, "forbidden", code)

	_, err = e.Svc.Upload(context.Background(), c, "nonsense", bytes.NewReader(pngBytes(t)))
	status, code = appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_file", code)
	require.Zero(t, e.Mem.Len())
}

func TestURL_PrivateOnlyOwnerOrAdmin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.user(t, auth.RolePartner)
	b := e.user(t, auth.RolePartner)
	cust := e.user(t, auth.RoleCustomer)
	admin := e.user(t, auth.RoleSuperAdmin)

	f, err := e.Svc.Upload(ctx, a, "business_license", bytes.NewReader(pngBytes(t)))
	require.NoError(t, err)
	require.Equal(t, "private", f.Visibility)
	require.Empty(t, f.URL)

	for _, p := range []auth.Principal{a, admin} {
		u, err := e.Svc.URL(ctx, p, f.ID)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(u, "memory://private/business_license/"), u)
		require.Contains(t, u, "?ttl=5m0s")
	}
	for _, p := range []auth.Principal{b, cust} {
		_, err := e.Svc.URL(ctx, p, f.ID)
		status, code := appCode(t, err)
		require.Equal(t, http.StatusNotFound, status)
		require.Equal(t, "not_found", code)
	}
	_, err = e.Svc.URL(ctx, a, uuid.Must(uuid.NewV7()))
	_, code := appCode(t, err)
	require.Equal(t, "not_found", code)

	// public file: fixed URL for anyone
	pub, err := e.Svc.Upload(ctx, a, "listing_image", bytes.NewReader(pngBytes(t)))
	require.NoError(t, err)
	u, err := e.Svc.URL(ctx, cust, pub.ID)
	require.NoError(t, err)
	require.Equal(t, pub.URL, u)
}

func TestOwned_ChecksUploaderAndPurpose(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.user(t, auth.RolePartner)
	b := e.user(t, auth.RolePartner)
	f, err := e.Svc.Upload(ctx, a, "listing_image", bytes.NewReader(pngBytes(t)))
	require.NoError(t, err)

	check := func(id, by uuid.UUID, purpose string) error {
		return db.InTx(ctx, e.Pool, func(tx pgx.Tx) error { return e.Svc.Owned(ctx, tx, id, by, purpose) })
	}
	require.NoError(t, check(f.ID, a.UserID, "listing_image"))
	for name, err := range map[string]error{
		"other uploader": check(f.ID, b.UserID, "listing_image"),
		"other purpose":  check(f.ID, a.UserID, "business_license"),
		"missing file":   check(uuid.Must(uuid.NewV7()), a.UserID, "listing_image"),
	} {
		status, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status, name)
		require.Equal(t, "invalid_file", code, name)
	}
}

func TestPublicURL_PublicOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.user(t, auth.RolePartner)
	pub, err := e.Svc.Upload(ctx, a, "listing_image", bytes.NewReader(pngBytes(t)))
	require.NoError(t, err)
	priv, err := e.Svc.Upload(ctx, a, "business_license", bytes.NewReader(pngBytes(t)))
	require.NoError(t, err)

	publicURL := func(id uuid.UUID) (string, error) {
		var u string
		err := db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
			var err error
			u, err = e.Svc.PublicURL(ctx, tx, id)
			return err
		})
		return u, err
	}
	u, err := publicURL(pub.ID)
	require.NoError(t, err)
	require.Equal(t, pub.URL, u)
	require.True(t, strings.HasPrefix(u, baseURL+"/public/listing_image/"), u)

	// Private and missing files are a plain error, never a URL or an AppError.
	for _, id := range []uuid.UUID{priv.ID, uuid.Must(uuid.NewV7())} {
		_, err = publicURL(id)
		require.ErrorIs(t, err, files.ErrNotPublic)
		var ae *httpx.AppError
		require.False(t, errors.As(err, &ae))
	}
}
