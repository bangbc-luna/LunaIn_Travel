// Package files owns uploaded files (images, licenses, payment proofs): type
// detection, purpose-based access rules and the files table. Bytes live in
// object storage; the DB row is the source of truth for who may see a file.
package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/files/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
)

const (
	// MaxSize is the largest accepted file, in bytes (10 MiB).
	MaxSize = 10 << 20
	// PrivateURLTTL is the lifetime of a presigned download URL.
	PrivateURLTTL = 5 * time.Minute

	visibilityPublic  = "public"
	visibilityPrivate = "private"
)

// ErrInvalidFile covers every rejected upload and every file reference that
// does not match its uploader or purpose.
var ErrInvalidFile = httpx.NewError(http.StatusUnprocessableEntity, "invalid_file", "Tệp không hợp lệ")

func invalidFile(msg string) error {
	return httpx.NewError(http.StatusUnprocessableEntity, "invalid_file", msg)
}

type rule struct {
	visibility string
	role       auth.Role
	types      []string
}

var (
	imageTypes    = []string{"image/jpeg", "image/png", "image/webp"}
	imagePDFTypes = []string{"image/jpeg", "image/png", "application/pdf"}

	rules = map[string]rule{
		"listing_image":    {visibilityPublic, auth.RolePartner, imageTypes},
		"review_image":     {visibilityPublic, auth.RoleCustomer, imageTypes},
		"business_license": {visibilityPrivate, auth.RolePartner, imagePDFTypes},
		"payout_proof":     {visibilityPrivate, auth.RoleSuperAdmin, imagePDFTypes},
		"refund_proof":     {visibilityPrivate, auth.RoleSuperAdmin, imagePDFTypes},
	}

	extByType = map[string]string{
		"image/jpeg":      ".jpg",
		"image/png":       ".png",
		"image/webp":      ".webp",
		"application/pdf": ".pdf",
	}
)

// File is an uploaded file. URL is set only for public files.
type File struct {
	ID          uuid.UUID `json:"id"`
	Purpose     string    `json:"purpose"`
	Visibility  string    `json:"visibility"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	UploadedBy  uuid.UUID `json:"uploaded_by"`
	URL         string    `json:"url,omitempty"`
}

// Service stores and serves files.
type Service struct {
	pool          *pgxpool.Pool
	storage       storage.Storage
	clock         clock.Clock
	publicBaseURL string
}

// NewService builds a Service. publicBaseURL has no trailing slash needed.
func NewService(pool *pgxpool.Pool, st storage.Storage, c clock.Clock, publicBaseURL string) *Service {
	return &Service{pool: pool, storage: st, clock: c, publicBaseURL: strings.TrimRight(publicBaseURL, "/")}
}

func (s *Service) publicURL(key string) string { return s.publicBaseURL + "/" + key }

// Upload validates r against the rules of purpose and stores it. The object is
// put on storage before the row is inserted: if the insert fails the object is
// orphaned, which is harmless because no row points to it.
func (s *Service) Upload(ctx context.Context, p auth.Principal, purpose string, r io.Reader) (File, error) {
	rl, ok := rules[purpose]
	if !ok {
		return File{}, invalidFile("Mục đích tải tệp không hợp lệ")
	}
	if p.Role != rl.role {
		return File{}, httpx.ErrForbidden
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return File{}, fmt.Errorf("read upload: %w", err)
	}
	if len(data) == 0 {
		return File{}, invalidFile("Tệp trống")
	}
	if len(data) > MaxSize {
		return File{}, invalidFile("Tệp vượt quá 10 MiB")
	}
	// Type comes from the content, never from what the client claims.
	ct := http.DetectContentType(data[:min(len(data), 512)])
	if !contains(rl.types, ct) {
		return File{}, invalidFile("Loại tệp không được hỗ trợ")
	}

	now := s.clock.Now()
	id, err := uuid.NewV7()
	if err != nil {
		return File{}, fmt.Errorf("new file id: %w", err)
	}
	key := fmt.Sprintf("%s/%s/%04d/%02d/%s%s", rl.visibility, purpose, now.Year(), int(now.Month()), id, extByType[ct])

	if err := s.storage.Put(ctx, key, ct, bytes.NewReader(data), int64(len(data))); err != nil {
		return File{}, fmt.Errorf("store file: %w", err)
	}
	if err := store.New(s.pool).InsertFile(ctx, store.InsertFileParams{
		ID: id, StorageKey: key, Purpose: purpose, Visibility: rl.visibility,
		ContentType: ct, SizeBytes: int64(len(data)), UploadedBy: p.UserID, Now: now,
	}); err != nil {
		return File{}, fmt.Errorf("insert file: %w", err)
	}
	f := File{ID: id, Purpose: purpose, Visibility: rl.visibility, ContentType: ct, SizeBytes: int64(len(data)), UploadedBy: p.UserID}
	if rl.visibility == visibilityPublic {
		f.URL = s.publicURL(key)
	}
	return f, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// URL returns a download URL: the fixed URL for public files, a 5-minute
// presigned URL for private ones. A private file is visible only to its
// uploader and super_admin; everyone else gets 404 so existence is not leaked.
func (s *Service) URL(ctx context.Context, p auth.Principal, id uuid.UUID) (string, error) {
	row, err := store.New(s.pool).GetFile(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", httpx.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get file: %w", err)
	}
	if row.Visibility == visibilityPublic {
		return s.publicURL(row.StorageKey), nil
	}
	if p.Role != auth.RoleSuperAdmin && p.UserID != row.UploadedBy {
		return "", httpx.ErrNotFound
	}
	u, err := s.storage.PresignGet(ctx, row.StorageKey, PrivateURLTTL)
	if err != nil {
		return "", fmt.Errorf("presign file: %w", err)
	}
	return u, nil
}

// Owned checks, inside tx, that file id exists, was uploaded by uploadedBy and
// has the given purpose. Callers use it before attaching a file_id to a record.
func (s *Service) Owned(ctx context.Context, tx pgx.Tx, id, uploadedBy uuid.UUID, purpose string) error {
	row, err := store.New(tx).GetFile(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidFile
	}
	if err != nil {
		return fmt.Errorf("get file: %w", err)
	}
	if row.UploadedBy != uploadedBy || row.Purpose != purpose {
		return ErrInvalidFile
	}
	return nil
}
