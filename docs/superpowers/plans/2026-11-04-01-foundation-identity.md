# Plan 1 — Nền tảng + Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Dựng khung backend Go (cấu hình, DB, migration, HTTP, job nền, email) và module `identity` hoàn chỉnh: đăng ký bằng email + mật khẩu có mã xác minh, đăng nhập, Google, refresh token xoay vòng, quên mật khẩu, phân quyền theo vai trò, hồ sơ khách.

**Architecture:** Modular monolith, một binary Go. Code nền dùng chung nằm ở `internal/platform/*`, module nghiệp vụ ở `internal/<module>`. Truy cập DB bằng SQL viết tay sinh code bằng sqlc trên pgx v5. Job nền và outbox dùng River trên chính Postgres: job được insert **trong cùng transaction** với thay đổi nghiệp vụ, đó chính là outbox.

**Tech Stack:** Go ≥ 1.25, PostgreSQL 17, pgx/v5, sqlc (qua `go tool`), goose v3 (migration nhúng bằng `embed`), River (`riverqueue/river` + `riverpgxv5`), `net/http` ServeMux (Go 1.22+ route patterns), `log/slog`, `golang-jwt/jwt/v5`, `golang.org/x/crypto/argon2`, `golang.org/x/time/rate`, `coreos/go-oidc/v3`, `google/uuid` (v7), `caarlos0/env/v11`, `testcontainers-go` (module postgres), `stretchr/testify`.

**Spec:** [docs/superpowers/specs/2026-11-04-lunain-travel-backend-design.md](../specs/2026-11-04-lunain-travel-backend-design.md) (§3, §4, §5.1, §14), thuật ngữ trong [GLOSSARY.md](../../../GLOSSARY.md).

## Global Constraints

- Go module path: `github.com/bangbc-luna/lunain-travel`.
- Khóa chính `uuid` v7 sinh trong Go (`uuid.NewV7()`). Mọi bảng có `created_at timestamptz NOT NULL DEFAULT now()` và `updated_at timestamptz NOT NULL DEFAULT now()`.
- Thời gian lưu `timestamptz` (UTC). Thời gian lấy từ `clock.Clock` được inject, không gọi `time.Now()` trong code nghiệp vụ.
- Tiền là `bigint` VND (plan này chưa có tiền, nhưng helper nào chạm tới tiền phải theo quy ước này).
- Không xóa cứng dữ liệu nghiệp vụ.
- Email được chuẩn hóa: `strings.ToLower(strings.TrimSpace(email))` trước khi lưu và trước khi tra cứu. Unique trên cột đã chuẩn hóa.
- Mật khẩu hash bằng argon2id. Mã xác minh và refresh token chỉ lưu dạng hash.
- Vai trò: `customer`, `partner`, `super_admin`. Tài khoản `partner` và `super_admin` không đăng ký công khai.
- API dưới tiền tố `/v1`. Lỗi luôn trả JSON `{"error":{"code":"<snake_case>","message":"<tiếng Việt>"}}`.
- Test chạy với Postgres thật (testcontainers). Không mock DB.
- Mỗi task kết thúc bằng `go vet ./... && go test ./...` xanh rồi commit.

## Review Focus

1. **Email viết hoa hoặc có khoảng trắng**: đăng ký `"  An@Gmail.com "` rồi đăng nhập bằng `an@gmail.com` phải được, đăng ký lại `AN@gmail.com` phải báo `email_taken`. Test ở Task 6 và Task 7.
2. **Refresh token bị dùng lại** (bị đánh cắp, hoặc client gửi hai request song song): lần dùng thứ hai của cùng một token phải thất bại và thu hồi toàn bộ phiên cùng họ. Test ở Task 7.
3. **Dò mã xác minh**: sai quá 5 lần thì mã bị vô hiệu, kể cả lần sau nhập đúng. Test ở Task 6.
4. **Quên mật khẩu với email không tồn tại**: vẫn trả 202 giống email có thật, không lộ email nào có tài khoản. Test ở Task 8.
5. **Body JSON hỏng, quá lớn hoặc có trường lạ**: trả 400 `invalid_request`, không bao giờ 500. Test ở Task 3.

---

## Cấu trúc file

```
cmd/api/main.go                         # entrypoint: subcommand "serve" (mặc định) và "create-admin"
internal/platform/config/config.go      # đọc env
internal/platform/clock/clock.go        # Clock interface, Real, Fixed (test)
internal/platform/db/db.go              # Connect, InTx
internal/platform/db/migrate.go         # goose (embed) + river migrate
internal/platform/db/migrations/*.sql   # migration của mọi module
internal/platform/httpx/json.go         # DecodeJSON, WriteJSON
internal/platform/httpx/errors.go       # AppError, WriteError, mã lỗi chung
internal/platform/httpx/middleware.go   # RequestID, Logger, Recover, RateLimit
internal/platform/auth/principal.go     # Role, Principal, context helpers
internal/platform/auth/token.go         # JWT access token Issuer
internal/platform/auth/middleware.go    # Authenticate, RequireRole
internal/platform/jobs/jobs.go          # River client builder
internal/platform/mailer/mailer.go      # Mailer interface, LogMailer, SMTPMailer
internal/platform/mailer/job.go         # SendEmailArgs + worker
internal/platform/testutil/postgres.go  # container Postgres dùng chung cho test
internal/identity/queries.sql           # SQL cho sqlc
internal/identity/store/                # code sqlc sinh ra (không sửa tay)
internal/identity/errors.go             # lỗi nghiệp vụ
internal/identity/password.go           # argon2id + kiểm tra độ mạnh
internal/identity/codes.go              # sinh và hash mã 6 số
internal/identity/service.go            # Service, Tokens, Register/Verify/Login/...
internal/identity/google.go             # GoogleVerifier
internal/identity/profile.go            # hồ sơ khách
internal/identity/http.go               # handler + Routes
sqlc.yaml, docker-compose.yml, Makefile, .env.example, .gitignore
```

---

### Task 1: Khung project, cấu hình, health check

**Files:**
- Create: `go.mod`, `.gitignore`, `docker-compose.yml`, `Makefile`, `.env.example`
- Create: `internal/platform/config/config.go`, `internal/platform/clock/clock.go`, `cmd/api/main.go`
- Test: `internal/platform/config/config_test.go`

**Interfaces:**
- Produces:
  - `config.Config{AppEnv string; HTTPAddr string; DatabaseURL string; JWTSecret []byte; CodePepper []byte; GoogleClientID string; MailFrom string; SMTPHost string; SMTPPort int; SMTPUser string; SMTPPassword string}`
  - `func config.Load() (config.Config, error)`: đọc env `APP_ENV` (mặc định `dev`, chỉ nhận `dev|test|prod`), `HTTP_ADDR` (mặc định `:8080`), `DATABASE_URL` (bắt buộc), `JWT_SECRET` (bắt buộc, ≥ 32 byte), `CODE_PEPPER` (bắt buộc, ≥ 32 byte), `GOOGLE_CLIENT_ID`, `MAIL_FROM` (mặc định `LunaIn Travel <no-reply@lunain.vn>`), `SMTP_*`. Khi `APP_ENV=prod` thì `SMTP_HOST` bắt buộc.
  - `clock.Clock interface{ Now() time.Time }`, `clock.Real{}`, `clock.Fixed{T time.Time}` (có method `Advance(d time.Duration)` trên con trỏ).

- [ ] **Step 1: Cài công cụ.** Cài Go ≥ 1.25 (tarball từ go.dev vào `/usr/local/go`, thêm vào `PATH`). Kiểm tra: `go version` in ra `go1.25` trở lên.
- [ ] **Step 2: Khởi tạo module.** `go mod init github.com/bangbc-luna/lunain-travel`. `.gitignore` gồm `.env`, `/bin/`, `coverage.out`. `docker-compose.yml` có service `postgres` image `postgres:17`, user/password/db `lunain`, cổng `5432:5432`, volume tên `pgdata`. `.env.example` liệt kê mọi biến ở trên với giá trị dev giả.
- [ ] **Step 3: Viết test fail** `config_test.go`:
  - `TestLoad_Defaults`: set chỉ `DATABASE_URL`, `JWT_SECRET` 32 byte, `CODE_PEPPER` 32 byte → `AppEnv=="dev"`, `HTTPAddr==":8080"`.
  - `TestLoad_RejectsShortSecret`: `JWT_SECRET="short"` → error chứa `JWT_SECRET`.
  - `TestLoad_ProdRequiresSMTP`: `APP_ENV=prod` không có `SMTP_HOST` → error chứa `SMTP_HOST`.
  - `TestLoad_RejectsUnknownEnv`: `APP_ENV=staging` → error.
  Dùng `t.Setenv`.
- [ ] **Step 4: Chạy** `go test ./internal/platform/config/...`. Kỳ vọng: FAIL (chưa có `Load`).
- [ ] **Step 5: Cài đặt** `config.Load` bằng `caarlos0/env/v11`, sau đó validate tay. Cài `clock`.
- [ ] **Step 6: Chạy lại test.** Kỳ vọng: PASS.
- [ ] **Step 7: `cmd/api/main.go`.** Khởi tạo `slog` JSON handler, `config.Load`, `http.Server` với `ReadHeaderTimeout: 5s`, `ReadTimeout: 15s`, `WriteTimeout: 30s`, `IdleTimeout: 60s`, route `GET /healthz` trả `200 {"status":"ok"}`, tắt êm khi nhận SIGINT/SIGTERM (`signal.NotifyContext`, `Shutdown` timeout 10s). `Makefile` có các target `up` (docker compose up -d), `run`, `test` (`go test ./...`), `lint` (`go vet ./...`), `sqlc` (`go tool sqlc generate`).
- [ ] **Step 8: Kiểm tra tay.** `make up && make run` rồi `curl -s localhost:8080/healthz`. Kỳ vọng: `{"status":"ok"}`.
- [ ] **Step 9: Commit** `feat: bootstrap go service with config and health check`.

---

### Task 2: Database, migration, transaction, Postgres cho test

**Files:**
- Create: `internal/platform/db/db.go`, `internal/platform/db/migrate.go`, `internal/platform/db/migrations/00001_init.sql`
- Create: `internal/platform/testutil/postgres.go`
- Test: `internal/platform/db/db_test.go`
- Modify: `cmd/api/main.go` (chạy migrate khi khởi động, thêm `GET /readyz` ping DB)

**Interfaces:**
- Produces:
  - `func db.Connect(ctx context.Context, url string) (*pgxpool.Pool, error)`: `MaxConns` 20, `MaxConnLifetime` 30 phút, ping trước khi trả về.
  - `func db.Migrate(ctx context.Context, pool *pgxpool.Pool) error`: chạy goose trên `embed.FS` `migrations/*.sql` rồi chạy `rivermigrate` lên bản mới nhất.
  - `func db.InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error`: commit nếu `fn` trả nil, rollback nếu lỗi hoặc panic (panic được ném lại sau khi rollback).
  - `func testutil.Postgres(t *testing.T) *pgxpool.Pool`: lần gọi đầu trong process thì khởi động một container `postgres:17` (dùng `sync.Once`) và chạy `db.Migrate`. Mỗi lần gọi `TRUNCATE ... RESTART IDENTITY CASCADE` mọi bảng trong schema `public` (kể cả `river_job`), **trừ** `goose_db_version` và `river_migration`. Các package test chạy song song trong cùng process không chia sẻ container. Test trong một package không gọi `t.Parallel()`. Trả về pool. Có `t.Cleanup` đóng pool. Tôn trọng `testing.Short()`: bỏ qua test (`t.Skip`).
- `00001_init.sql`: `CREATE EXTENSION IF NOT EXISTS btree_gist;` (plan 7 cần). Chỉ vậy.

- [ ] **Step 1: Viết test fail** `db_test.go`:
  - `TestMigrate_Idempotent`: gọi `db.Migrate` hai lần trên pool của `testutil.Postgres(t)` → không lỗi. Bảng `river_job` tồn tại.
  - `TestInTx_RollbackOnError`: tạo bảng tạm `t_probe(id int)` ngoài transaction. Trong `InTx` insert 1 dòng rồi trả `errors.New("boom")` → `InTx` trả lỗi bọc `boom`, bảng có 0 dòng.
  - `TestInTx_RollbackOnPanic`: `fn` insert rồi `panic("x")` → `require.PanicsWithValue(t, "x", ...)`, bảng có 0 dòng.
  - `TestInTx_Commit`: `fn` insert rồi trả nil → 1 dòng.
- [ ] **Step 2: Chạy** `go test ./internal/platform/db/...`. Kỳ vọng: FAIL (chưa có package).
- [ ] **Step 3: Cài đặt** `db.go`, `migrate.go` (goose: `goose.SetBaseFS`, dialect `postgres`, mở `*sql.DB` bằng `stdlib.OpenDBFromPool`), `testutil/postgres.go`.
- [ ] **Step 4: Chạy lại test.** Kỳ vọng: PASS.
- [ ] **Step 5: Nối vào `main.go`.** `Connect` → `Migrate` → `GET /readyz` (ping DB với timeout 2s, lỗi trả 503).
- [ ] **Step 6: Commit** `feat: add postgres pool, embedded migrations and tx helper`.

---

### Task 3: Nền HTTP (JSON, lỗi, middleware, rate limit)

**Files:**
- Create: `internal/platform/httpx/json.go`, `errors.go`, `middleware.go`
- Test: `internal/platform/httpx/httpx_test.go`

**Interfaces:**
- Produces:
  - `type httpx.AppError struct{ Status int; Code string; Message string }` có method `Error()`. Constructor `httpx.NewError(status int, code, msg string) *AppError`.
  - Lỗi chung: `httpx.ErrInvalidRequest` (400 `invalid_request` "Dữ liệu gửi lên không hợp lệ"), `httpx.ErrUnauthorized` (401 `unauthorized` "Bạn cần đăng nhập"), `httpx.ErrForbidden` (403 `forbidden` "Bạn không có quyền thực hiện thao tác này"), `httpx.ErrNotFound` (404 `not_found` "Không tìm thấy"), `httpx.ErrTooManyRequests` (429 `too_many_requests` "Bạn thao tác quá nhanh, vui lòng thử lại sau"), `httpx.ErrInternal` (500 `internal_error` "Có lỗi xảy ra, vui lòng thử lại").
  - `func httpx.WriteError(w http.ResponseWriter, r *http.Request, err error)`: dùng `errors.As` lấy `*AppError`. Nếu không lấy được thì log lỗi gốc kèm request id rồi trả `ErrInternal`.
  - `func httpx.WriteJSON(w http.ResponseWriter, status int, v any)`.
  - `func httpx.DecodeJSON(r *http.Request, dst any) error`: giới hạn body 1 MiB (`http.MaxBytesReader`), `DisallowUnknownFields`, chỉ một giá trị JSON. Mọi lỗi decode trả `ErrInvalidRequest`.
  - `func httpx.Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler`
  - `httpx.RequestID` (đọc header `X-Request-ID` hoặc sinh uuid v7, ghi lại vào response và context; `httpx.RequestIDFrom(ctx) string`), `httpx.Logger(*slog.Logger)` (method, path, status, duration, request id), `httpx.Recover` (panic → log stack + trả `ErrInternal`).
  - `func httpx.RateLimit(perMinute int, burst int, key func(*http.Request) string) func(http.Handler) http.Handler`: token bucket `x/time/rate` trong bộ nhớ theo key, xóa key không dùng sau 10 phút. Quá hạn mức → `ErrTooManyRequests` kèm header `Retry-After`. `httpx.ClientIP(r)` lấy `X-Forwarded-For` đầu tiên, không có thì `RemoteAddr`. **Ghi chú:** chỉ đúng khi chạy một instance, đủ cho giai đoạn 1 (spec §4).

- [ ] **Step 1: Viết test fail** `httpx_test.go` (dùng `httptest`):
  - `TestDecodeJSON_UnknownField`: body `{"a":1,"zzz":2}` vào struct có `A int` → `errors.Is(err, httpx.ErrInvalidRequest)`.
  - `TestDecodeJSON_TooLarge`: body 2 MiB → `ErrInvalidRequest`.
  - `TestDecodeJSON_Malformed`: body `{"a":` → `ErrInvalidRequest`.
  - `TestWriteError_AppError`: `WriteError` với `ErrForbidden` → status 403, body `{"error":{"code":"forbidden","message":"Bạn không có quyền thực hiện thao tác này"}}`.
  - `TestWriteError_UnknownErrorIs500`: lỗi thường → 500, code `internal_error`, body không chứa nội dung lỗi gốc.
  - `TestRecover`: handler panic → 500 `internal_error`.
  - `TestRateLimit`: `RateLimit(2, 2, func(*http.Request) string { return "k" })`, gọi 3 lần → 200, 200, 429 và có header `Retry-After`.
- [ ] **Step 2: Chạy** `go test ./internal/platform/httpx/...`. Kỳ vọng: FAIL.
- [ ] **Step 3: Cài đặt** ba file.
- [ ] **Step 4: Chạy lại test.** Kỳ vọng: PASS.
- [ ] **Step 5: Nối vào `main.go`.** Thứ tự middleware toàn cục: `RequestID → Logger → Recover`.
- [ ] **Step 6: Commit** `feat: add http json/error helpers, middleware and rate limiter`.

---

### Task 4: Access token, Principal, phân quyền

**Files:**
- Create: `internal/platform/auth/principal.go`, `token.go`, `middleware.go`
- Test: `internal/platform/auth/auth_test.go`

**Interfaces:**
- Consumes: `clock.Clock`, `httpx.WriteError`, `httpx.ErrUnauthorized`, `httpx.ErrForbidden`.
- Produces:
  - `type auth.Role string` với hằng `auth.RoleCustomer = "customer"`, `auth.RolePartner = "partner"`, `auth.RoleSuperAdmin = "super_admin"`. `func (r Role) Valid() bool`.
  - `type auth.Principal struct{ UserID uuid.UUID; Role Role }`. `func auth.WithPrincipal(ctx, Principal) context.Context`, `func auth.PrincipalFrom(ctx) (Principal, bool)`.
  - `type auth.Issuer struct` tạo bằng `auth.NewIssuer(secret []byte, ttl time.Duration, c clock.Clock) *Issuer`. Method `Issue(p Principal) (token string, expiresAt time.Time, err error)` và `Verify(token string) (Principal, error)`. JWT HS256, claim `sub` = user id, `role`, `iat`, `exp`, `iss`="lunain". TTL access token: **15 phút** (truyền từ `main`). `Verify` từ chối thuật toán khác HS256, token hết hạn (theo `clock`), `iss` sai, role không hợp lệ, và trả `auth.ErrInvalidToken`.
  - `func auth.Authenticate(iss *Issuer) func(http.Handler) http.Handler`: đọc `Authorization: Bearer <token>`. Không có header thì cho qua mà không gắn Principal. Header sai hoặc token không hợp lệ → 401.
  - `func auth.RequireRole(roles ...Role) func(http.Handler) http.Handler`: không có Principal → 401, sai role → 403.

- [ ] **Step 1: Viết test fail** `auth_test.go` với `clock.Fixed`:
  - `TestIssuer_RoundTrip`: `Issue` rồi `Verify` → Principal giống hệt.
  - `TestIssuer_Expired`: issue, `Advance(15*time.Minute + time.Second)` → `ErrInvalidToken`.
  - `TestIssuer_WrongSecret`: verify bằng issuer khác secret → `ErrInvalidToken`.
  - `TestIssuer_RejectsNoneAlg`: token tự ký `alg: none` → `ErrInvalidToken`.
  - `TestRequireRole`: chain `Authenticate` + `RequireRole(RoleSuperAdmin)`. Không token → 401. Token customer → 403. Token super_admin → 200.
  - `TestAuthenticate_MalformedHeader`: `Authorization: Token abc` → 401.
- [ ] **Step 2: Chạy** `go test ./internal/platform/auth/...`. Kỳ vọng: FAIL.
- [ ] **Step 3: Cài đặt** ba file.
- [ ] **Step 4: Chạy lại test.** Kỳ vọng: PASS.
- [ ] **Step 5: Commit** `feat: add jwt access tokens and role-based middleware`.

---

### Task 5: Job nền (River) và gửi email

**Files:**
- Create: `internal/platform/jobs/jobs.go`, `internal/platform/mailer/mailer.go`, `internal/platform/mailer/job.go`
- Test: `internal/platform/mailer/mailer_test.go`
- Modify: `cmd/api/main.go` (khởi động và dừng River client cùng server)

**Interfaces:**
- Consumes: `db.InTx`, `testutil.Postgres`, `config.Config`.
- Produces:
  - `type mailer.Message struct{ To string; Subject string; Text string }`
  - `type mailer.Mailer interface{ Send(ctx context.Context, m Message) error }`. `mailer.LogMailer{Logger *slog.Logger}` chỉ log (dùng cho `dev`/`test`). `mailer.SMTPMailer` dùng `net/smtp` với STARTTLS, `From` = `config.MailFrom`. `mailer.Recorder` (dùng cho test, lưu `[]Message` có mutex) có method `Messages() []Message`.
  - `type mailer.SendEmailArgs struct{ Message }` với `Kind() string { return "send_email" }` và `InsertOpts()` trả `MaxAttempts: 8`.
  - `type mailer.SendEmailWorker struct{ river.WorkerDefaults[SendEmailArgs]; Mailer Mailer }`.
  - `func jobs.NewClient(pool *pgxpool.Pool, workers *river.Workers, logger *slog.Logger) (*river.Client[pgx.Tx], error)`: queue `default` với `MaxWorkers: 10`.
  - `func mailer.EnqueueTx(ctx context.Context, tx pgx.Tx, client *river.Client[pgx.Tx], m Message) error`: gọi `client.InsertTx`. **Mọi email nghiệp vụ phải được enqueue qua hàm này, trong transaction nghiệp vụ** (spec §4, §12).

- [ ] **Step 1: Viết test fail** `mailer_test.go`:
  - `TestEnqueueTx_RolledBackTxSendsNothing`: trong `db.InTx` gọi `EnqueueTx` rồi trả lỗi → bảng `river_job` có 0 job kind `send_email`.
  - `TestEnqueueTx_CommittedJobIsSent`: tạo client với `SendEmailWorker{Mailer: rec}` (`rec` là `*mailer.Recorder`), start client, enqueue trong tx thành công, chờ tối đa 5s (subscribe `river.EventKindJobCompleted`) → `rec.Messages()` có đúng 1 message với `To`/`Subject` khớp.
- [ ] **Step 2: Chạy** `go test ./internal/platform/mailer/...`. Kỳ vọng: FAIL.
- [ ] **Step 3: Cài đặt** ba file.
- [ ] **Step 4: Chạy lại test.** Kỳ vọng: PASS.
- [ ] **Step 5: Nối vào `main.go`.** `APP_ENV=prod` thì dùng `SMTPMailer`, còn lại dùng `LogMailer`. `client.Start` trước khi mở cổng HTTP, `client.Stop` sau khi tắt server.
- [ ] **Step 6: Commit** `feat: add river job client and transactional email outbox`.

---

### Task 6: Schema identity, đăng ký và xác minh email

**Files:**
- Create: `internal/platform/db/migrations/00002_identity.sql`, `sqlc.yaml`, `internal/identity/queries.sql`, `internal/identity/store/` (sinh ra)
- Create: `internal/identity/errors.go`, `password.go`, `codes.go`, `service.go`
- Test: `internal/identity/helpers_test.go`, `internal/identity/password_test.go`, `internal/identity/register_test.go`

**Schema** (`00002_identity.sql`):
- `users(id uuid PK, email text NOT NULL UNIQUE, password_hash text NULL, email_verified_at timestamptz NULL, role text NOT NULL CHECK (role IN ('customer','partner','super_admin')), status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','locked')), created_at, updated_at)`. Thêm `CHECK (email = lower(btrim(email)))`.
- `user_identities(id uuid PK, user_id uuid NOT NULL REFERENCES users, provider text NOT NULL CHECK (provider IN ('google')), provider_subject text NOT NULL, created_at, updated_at, UNIQUE(provider, provider_subject))`.
- `email_verifications(id uuid PK, user_id uuid NOT NULL REFERENCES users, purpose text NOT NULL CHECK (purpose IN ('signup','password_reset')), code_hash bytea NOT NULL, expires_at timestamptz NOT NULL, attempts int NOT NULL DEFAULT 0, consumed_at timestamptz NULL, created_at, updated_at)` + index `(user_id, purpose, created_at DESC)`.
- `refresh_tokens(id uuid PK, user_id uuid NOT NULL REFERENCES users, family_id uuid NOT NULL, token_hash bytea NOT NULL UNIQUE, expires_at timestamptz NOT NULL, used_at timestamptz NULL, revoked_at timestamptz NULL, created_at, updated_at)` + index `(family_id)`.
- `customer_profiles(user_id uuid PK REFERENCES users, full_name text NULL, phone text NULL, date_of_birth date NULL, created_at, updated_at)`.

**Interfaces:**
- Consumes: `db.InTx`, `mailer.EnqueueTx`, `auth.Issuer`, `auth.Role`, `clock.Clock`, `httpx.NewError`.
- Produces:
  - Lỗi nghiệp vụ trong `errors.go`, đều là `*httpx.AppError`:
    | Biến | Status | Code |
    |---|---|---|
    | `ErrEmailTaken` | 409 | `email_taken` |
    | `ErrWeakPassword` | 422 | `weak_password` |
    | `ErrInvalidEmail` | 422 | `invalid_email` |
    | `ErrInvalidCode` | 400 | `invalid_code` (sai, hết hạn, đã dùng hoặc quá số lần thử đều dùng mã này, không phân biệt) |
    | `ErrResendTooSoon` | 429 | `resend_too_soon` |
    | `ErrInvalidCredentials` | 401 | `invalid_credentials` |
    | `ErrEmailNotVerified` | 403 | `email_not_verified` |
    | `ErrAccountLocked` | 403 | `account_locked` |
    | `ErrInvalidRefreshToken` | 401 | `invalid_refresh_token` |
    | `ErrGoogleTokenInvalid` | 401 | `google_token_invalid` |
  - `func HashPassword(pw string) (string, error)` / `func VerifyPassword(encoded, pw string) (bool, error)`: argon2id `m=64MiB, t=3, p=2, saltLen=16, keyLen=32`, định dạng PHC `$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`, so sánh constant-time.
  - `func ValidatePassword(pw string) error`: 8–128 ký tự, có ít nhất một chữ cái và một chữ số, nếu không thì `ErrWeakPassword`.
  - `func NormalizeEmail(s string) (string, error)`: trim + lower + `net/mail.ParseAddress`, địa chỉ phải trùng nguyên văn chuỗi đã chuẩn hóa. Sai → `ErrInvalidEmail`.
  - `codes.go`: `func newCode() (string, error)` sinh 6 chữ số bằng `crypto/rand`. `func hashCode(pepper []byte, purpose, userID, code string) []byte` = HMAC-SHA256.
  - Hằng: `codeTTL = 10 * time.Minute`, `maxCodeAttempts = 5`, `resendCooldown = 60 * time.Second`.
  - `type Service struct` tạo bằng `identity.NewService(deps Deps) *Service`, trong đó `Deps{Pool *pgxpool.Pool; Jobs *river.Client[pgx.Tx]; Issuer *auth.Issuer; Clock clock.Clock; CodePepper []byte; Google GoogleVerifier; RefreshTTL time.Duration}`. `RefreshTTL` = **30 ngày**.
  - `type Tokens struct{ AccessToken string; AccessExpiresAt time.Time; RefreshToken string; RefreshExpiresAt time.Time; Role auth.Role }` (JSON snake_case).
  - `func (s *Service) Register(ctx, email, password string) error`
  - `func (s *Service) ResendVerification(ctx, email string) error`
  - `func (s *Service) VerifyEmail(ctx, email, code, password string) (Tokens, error)` (Ruling 7: mật khẩu đăng ký gắn với mã `signup` qua `email_verifications.password_hash` và phải nhập lại khi xác minh, `users.password_hash` chỉ được đặt khi xác minh thành công. Thứ tự khóa luôn là user → mã.)
  - `func (s *Service) issueSession(ctx, tx pgx.Tx, userID uuid.UUID, role auth.Role, familyID uuid.UUID) (Tokens, error)` (nội bộ, Task 7 dùng lại).

Quy tắc hành vi:
- `Register`: chuẩn hóa email, validate mật khẩu. Email chưa có → tạo user `customer` chưa xác minh + `customer_profiles` rỗng + mã `signup` + enqueue email, tất cả trong **một** transaction. Email đã có và **đã xác minh** → `ErrEmailTaken`. Email đã có nhưng **chưa xác minh** → cập nhật `password_hash` mới, áp `resendCooldown` (`ErrResendTooSoon`), tạo mã mới, gửi lại (người đăng ký trước có thể là người gõ nhầm email).
- Email mã xác minh: subject `"Mã xác minh LunaIn Travel"`, nội dung chứa mã 6 số và câu `"Mã có hiệu lực trong 10 phút."`.
- Mã mới làm mọi mã cũ cùng `purpose` của user đó mất hiệu lực (đặt `consumed_at`).
- `VerifyEmail`: lấy mã mới nhất chưa dùng của user cho `signup` với `FOR UPDATE`. Hết hạn, quá số lần thử hoặc không có → `ErrInvalidCode`. Sai → tăng `attempts` (commit dù trả lỗi) rồi `ErrInvalidCode`. Đúng → `consumed_at`, `email_verified_at`, cấp phiên mới (đăng nhập luôn). Email không tồn tại → `ErrInvalidCode`.

- [ ] **Step 1: Thêm sqlc làm tool.** `go get -tool github.com/sqlc-dev/sqlc/cmd/sqlc`. `sqlc.yaml`: engine `postgresql`, schema `internal/platform/db/migrations`, queries `internal/identity/queries.sql`, out `internal/identity/store`, `sql_package: pgx/v5`, `emit_json_tags: false`, override `uuid` → `github.com/google/uuid.UUID`, `timestamptz` → `time.Time`.
- [ ] **Step 2: Viết migration và queries** cho Task 6 (insert user/profile/verification, get user by email, get latest active code `FOR UPDATE`, increment attempts, consume code, invalidate codes, mark verified). Chạy `make sqlc`. Kỳ vọng: sinh code không lỗi.
- [ ] **Step 3: Viết test fail** `password_test.go`:
  - `TestHashVerify_RoundTrip`, `TestVerify_WrongPassword` (false), `TestHash_DifferentSalts` (hai lần hash khác nhau).
  - `TestValidatePassword`: `"abc12345"` ok. `"abcdefgh"` → `ErrWeakPassword`. `"1234567"` → `ErrWeakPassword`. Chuỗi 129 ký tự → `ErrWeakPassword`.
  - `TestNormalizeEmail`: `"  An@Gmail.COM "` → `"an@gmail.com"`. `"not-an-email"` → `ErrInvalidEmail`. `"A <a@b.com>"` → `ErrInvalidEmail`.
- [ ] **Step 4: Viết helper test dùng chung** `internal/identity/helpers_test.go`: `newTestEnv(t) *testEnv` với `testEnv{Svc *Service; Pool *pgxpool.Pool; Clock *clock.Fixed; Mail *mailer.Recorder; Google *fakeGoogle}`. Helper này dựng `testutil.Postgres`, River client đã start với `SendEmailWorker{Mailer: Mail}` (dừng trong `t.Cleanup`), `clock.Fixed` bắt đầu ở `2026-11-04T09:00:00+07:00`, issuer TTL 15 phút, `RefreshTTL` 30 ngày. Thêm `lastCode(t, env) string`: chờ tối đa 5s tới khi có email, rồi dùng regex `\b\d{6}\b` lấy mã từ email gần nhất. `fakeGoogle` (map token → `GoogleIdentity`) đặt ở đây để Task 9 dùng. Các test Task 7–11 đều dùng helper này.
- [ ] **Step 5: Viết test fail** `register_test.go`:
  - `TestRegister_SendsCodeAndCreatesUnverifiedCustomer`: user có `role=customer`, `email_verified_at IS NULL`, có `customer_profiles`. Đúng 1 email tới `an@gmail.com`, subject `"Mã xác minh LunaIn Travel"`.
  - `TestRegister_NormalizesEmail`: đăng ký `"  An@Gmail.com "` → lưu `an@gmail.com`.
  - `TestRegister_VerifiedEmailTaken`: đăng ký, xác minh, đăng ký lại `AN@gmail.com` → `ErrEmailTaken`.
  - `TestRegister_UnverifiedReRegisterRespectsCooldown`: đăng ký lại ngay → `ErrResendTooSoon`. `Advance(61s)` rồi đăng ký lại → ok, mã cũ không còn dùng được, mã mới dùng được.
  - `TestVerifyEmail_Success`: trả `Tokens` có `AccessToken`, `RefreshToken` khác rỗng, `Role == customer`. User có `email_verified_at` = thời gian của `clock`.
  - `TestVerifyEmail_Expired`: `Advance(10*time.Minute + time.Second)` → `ErrInvalidCode`.
  - `TestVerifyEmail_LockedAfterFiveWrong`: 5 lần mã sai đều `ErrInvalidCode`, lần thứ 6 dùng mã **đúng** → vẫn `ErrInvalidCode`.
  - `TestVerifyEmail_CodeSingleUse`: xác minh thành công, dùng lại mã đó → `ErrInvalidCode`.
  - `TestVerifyEmail_UnknownEmail`: → `ErrInvalidCode`.
- [ ] **Step 6: Chạy** `go test ./internal/identity/...`. Kỳ vọng: FAIL.
- [ ] **Step 7: Cài đặt** `errors.go`, `password.go`, `codes.go`, `service.go` (`Register`, `ResendVerification`, `VerifyEmail`, `issueSession`; `issueSession` sinh refresh token 32 byte ngẫu nhiên, mã hóa base64url, lưu SHA-256 hash).
- [ ] **Step 8: Chạy lại test.** Kỳ vọng: PASS.
- [ ] **Step 9: Commit** `feat(identity): signup with email verification code`.

---

### Task 7: Đăng nhập, refresh xoay vòng, đăng xuất, tạo tài khoản nội bộ

**Files:**
- Modify: `internal/identity/queries.sql`, `internal/identity/service.go`
- Test: `internal/identity/session_test.go`

**Interfaces:**
- Consumes: Task 6.
- Produces:
  - `func (s *Service) Login(ctx, email, password string) (Tokens, error)`
  - `func (s *Service) Refresh(ctx, refreshToken string) (Tokens, error)`
  - `func (s *Service) Logout(ctx, refreshToken string) error`
  - `func (s *Service) RevokeAllSessions(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error` (Task 8 dùng; plan 9 dùng khi khóa tài khoản)
  - `func (s *Service) CreateInternalUser(ctx context.Context, tx pgx.Tx, email, password string, role auth.Role) (uuid.UUID, error)`: tạo user **đã xác minh** với role `partner` hoặc `super_admin` (role `customer` → lỗi). Plan 2 dùng để tạo tài khoản đối tác. Email trùng → `ErrEmailTaken`.

Quy tắc hành vi:
- `Login`: email không tồn tại hoặc sai mật khẩu → `ErrInvalidCredentials`, và **vẫn chạy một lần verify argon2 với hash giả** để thời gian phản hồi không lộ email có tồn tại hay không. User đã xác minh nhưng không có `password_hash` (chỉ dùng Google) → `ErrInvalidCredentials`. Chưa xác minh (theo Ruling 7, lúc này `users.password_hash` là NULL và mật khẩu chờ kích hoạt nằm trên mã `signup` mới nhất) → so mật khẩu với `password_hash` của mã `signup` mới nhất của user (đã dùng hay chưa đều được). Khớp → `ErrEmailNotVerified`, không khớp hoặc không có mã → `ErrInvalidCredentials`. `status=locked` → `ErrAccountLocked`. Thành công → phiên mới, `family_id` mới.
- `Refresh`: tìm theo hash `FOR UPDATE`. Không thấy, hết hạn hoặc `revoked_at` khác null → `ErrInvalidRefreshToken`. **Đã có `used_at`** (bị dùng lại) → thu hồi mọi token cùng `family_id` (commit việc thu hồi) rồi `ErrInvalidRefreshToken`. User bị khóa → thu hồi family, `ErrAccountLocked`. Hợp lệ → đặt `used_at`, phát token mới **cùng `family_id`**, access token mang role hiện tại của user.
- `Logout`: thu hồi cả family của token. Token không hợp lệ cũng trả nil (idempotent).

- [ ] **Step 1: Viết test fail** `session_test.go`:
  - `TestLogin_Success`, `TestLogin_EmailCaseInsensitive` (đăng ký `An@Gmail.com`, đăng nhập `an@gmail.com`).
  - `TestLogin_WrongPassword` và `TestLogin_UnknownEmail` → cả hai `ErrInvalidCredentials`.
  - `TestLogin_Unverified` → `ErrEmailNotVerified`. `TestLogin_Locked` (update `status='locked'` bằng SQL) → `ErrAccountLocked`.
  - `TestRefresh_Rotates`: refresh trả token mới khác token cũ. Token mới refresh được tiếp.
  - `TestRefresh_ReuseRevokesFamily`: R1 → R2. Dùng lại R1 → `ErrInvalidRefreshToken`. Sau đó R2 cũng → `ErrInvalidRefreshToken`.
  - `TestRefresh_ConcurrentReuse`: hai goroutine cùng refresh R1 → đúng một thành công. Sau khi cả hai xong, token vừa cấp cũng không dùng được (family bị thu hồi).
  - `TestRefresh_Expired`: `Advance(30*24*time.Hour + time.Second)` → `ErrInvalidRefreshToken`.
  - `TestLogout_RevokesFamily`: logout R2 → refresh R2 lỗi. Logout token rác → nil.
  - `TestCreateInternalUser`: tạo `partner` → đăng nhập được ngay, `Tokens.Role == partner`. Role `customer` → lỗi.
- [ ] **Step 2: Chạy** `go test ./internal/identity/ -run 'Login|Refresh|Logout|CreateInternal'`. Kỳ vọng: FAIL.
- [ ] **Step 3: Cài đặt** queries + method.
- [ ] **Step 4: Chạy lại test** (thêm `-race`). Kỳ vọng: PASS.
- [ ] **Step 5: Commit** `feat(identity): login, rotating refresh tokens with reuse detection, logout`.

---

### Task 8: Quên mật khẩu

**Files:**
- Modify: `internal/identity/queries.sql`, `internal/identity/service.go`
- Test: `internal/identity/password_reset_test.go`

**Interfaces:**
- Produces:
  - `func (s *Service) RequestPasswordReset(ctx, email string) error`: email không tồn tại, không hợp lệ, hoặc tài khoản bị khóa → trả **nil** và không gửi gì. Có tài khoản → mã `password_reset` (cùng TTL, cooldown, số lần thử như Task 6). Trong cooldown → vẫn trả nil, không gửi thêm. Email subject `"Đặt lại mật khẩu LunaIn Travel"`. Áp dụng cả cho user chưa xác minh và user chỉ dùng Google.
  - `func (s *Service) ResetPassword(ctx, email, code, newPassword string) error`: validate mật khẩu mới, kiểm tra mã như `VerifyEmail` (khóa user trước, rồi tới mã) → đặt `password_hash`, đặt `email_verified_at` nếu đang null (vì đã chứng minh sở hữu email) và khi đó vô hiệu mọi mã `signup` đang chờ, gọi `RevokeAllSessions`. Mọi thứ trong một transaction.

- [ ] **Step 1: Viết test fail**:
  - `TestRequestPasswordReset_UnknownEmailSilent`: trả nil, `rec.Messages()` rỗng.
  - `TestResetPassword_Success`: login mật khẩu cũ → `ErrInvalidCredentials`, mật khẩu mới → ok. Refresh token cấp trước khi reset → `ErrInvalidRefreshToken`.
  - `TestResetPassword_WrongCodeFiveTimes`: giống Task 6 (lần 6 dùng mã đúng vẫn `ErrInvalidCode`).
  - `TestResetPassword_GoogleOnlyUserCanSetPassword`: user tạo từ Google (insert trực tiếp bằng SQL, `password_hash` null) → reset → login bằng mật khẩu được.
  - `TestResetPassword_SignupCodeNotAccepted`: mã `signup` không dùng được cho reset → `ErrInvalidCode`.
- [ ] **Step 2: Chạy.** Kỳ vọng: FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy.** Kỳ vọng: PASS.
- [ ] **Step 5: Commit** `feat(identity): password reset by email code`.

---

### Task 9: Đăng nhập Google

**Files:**
- Create: `internal/identity/google.go`
- Modify: `internal/identity/service.go`, `internal/identity/queries.sql`
- Test: `internal/identity/google_test.go`

**Interfaces:**
- Produces:
  - `type GoogleIdentity struct{ Subject string; Email string; EmailVerified bool }`
  - `type GoogleVerifier interface{ Verify(ctx context.Context, idToken string) (GoogleIdentity, error) }`
  - `func NewGoogleVerifier(ctx context.Context, clientID string) (GoogleVerifier, error)`: dùng `oidc.NewProvider(ctx, "https://accounts.google.com")` và `provider.Verifier(&oidc.Config{ClientID: clientID})`, đọc claim `email`, `email_verified`.
  - `func (s *Service) LoginWithGoogle(ctx, idToken string) (Tokens, error)`

Luồng: frontend lấy Google ID token (Google Identity Services) rồi gửi lên backend. Backend xác minh token.

Quy tắc hành vi:
- Verify lỗi → `ErrGoogleTokenInvalid`. `EmailVerified == false` → `ErrGoogleTokenInvalid`.
- Đã có `user_identities(google, sub)` → đăng nhập user đó.
- Chưa có, nhưng có user cùng email (đã chuẩn hóa) → tạo liên kết. Nếu user đó chưa xác minh thì đặt `email_verified_at` và vô hiệu mọi mã `signup` đang chờ. `password_hash` giữ NULL, mật khẩu chờ kích hoạt bị bỏ (Ruling 7). Người dùng muốn có mật khẩu thì dùng quên mật khẩu. Nếu user có role `partner` hoặc `super_admin` → **không** liên kết, trả `ErrGoogleTokenInvalid` (tài khoản nội bộ chỉ đăng nhập bằng mật khẩu).
- Chưa có gì → tạo user `customer` đã xác minh, `password_hash` null, `customer_profiles` rỗng, và liên kết.
- `status=locked` → `ErrAccountLocked`.

- [ ] **Step 1: Viết test fail** với `fakeGoogle` (map token → `GoogleIdentity`):
  - `TestGoogle_NewUser`: tạo user customer đã xác minh, có identity, có profile.
  - `TestGoogle_ReturningUser`: lần hai cùng `sub` → cùng user id, không tạo user mới.
  - `TestGoogle_LinksExistingUnverifiedUser`: đăng ký email (chưa xác minh) rồi Google cùng email → cùng user id, `email_verified_at` khác null, `password_hash` vẫn NULL, mã `signup` cũ không dùng được nữa.
  - `TestGoogle_LinksExistingVerifiedPasswordUser`: đăng ký + xác minh rồi Google cùng email → cùng user id, vẫn đăng nhập bằng mật khẩu được.
  - `TestGoogle_UnverifiedGoogleEmailRejected` → `ErrGoogleTokenInvalid`.
  - `TestGoogle_DoesNotLinkPartner`: `CreateInternalUser` partner rồi Google cùng email → `ErrGoogleTokenInvalid`.
- [ ] **Step 2: Chạy.** Kỳ vọng: FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy.** Kỳ vọng: PASS.
- [ ] **Step 5: Commit** `feat(identity): google sign-in with account linking`.

---

### Task 10: Hồ sơ khách

**Files:**
- Create: `internal/identity/profile.go`
- Modify: `internal/identity/queries.sql`
- Test: `internal/identity/profile_test.go`

**Interfaces:**
- Produces:
  - `type Profile struct{ UserID uuid.UUID; Email string; FullName *string; Phone *string; DateOfBirth *time.Time }` (JSON snake_case, `date_of_birth` dạng `YYYY-MM-DD`)
  - `type ProfileUpdate struct{ FullName *string; Phone *string; DateOfBirth *string }`: trường nil thì giữ nguyên, chuỗi rỗng thì xóa.
  - `func (s *Service) GetProfile(ctx, userID uuid.UUID) (Profile, error)`
  - `func (s *Service) UpdateProfile(ctx, userID uuid.UUID, u ProfileUpdate) (Profile, error)`: `full_name` tối đa 100 ký tự sau trim. `phone` chuẩn hóa bỏ khoảng trắng, dấu chấm, gạch ngang, đổi `+84` thành `0`, sau đó phải khớp `^0\d{9}$`. `date_of_birth` phải là ngày hợp lệ trong quá khứ. Sai → 422 `invalid_profile` kèm message chỉ ra trường lỗi.
  - `func (s *Service) FillProfileIfEmpty(ctx context.Context, tx pgx.Tx, userID uuid.UUID, fullName, phone string) error`: chỉ điền những trường đang null (spec §5.1, plan 4 gọi khi tạo Booking đầu tiên). Dữ liệu sai thì bỏ qua trường đó, không trả lỗi.

- [ ] **Step 1: Viết test fail**:
  - `TestUpdateProfile_PartialUpdate`: cập nhật `full_name` → `phone` giữ nguyên.
  - `TestUpdateProfile_PhoneNormalization`: `"+84 912.345.678"` → `"0912345678"`. `"12345"` → `invalid_profile`.
  - `TestUpdateProfile_ClearField`: `FullName=""` → null.
  - `TestUpdateProfile_FutureDOB` → `invalid_profile`.
  - `TestFillProfileIfEmpty_DoesNotOverwrite`: đã có tên A, gọi với tên B → vẫn là A, phone (đang null) được điền.
- [ ] **Step 2: Chạy.** Kỳ vọng: FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy.** Kỳ vọng: PASS.
- [ ] **Step 5: Commit** `feat(identity): customer profile`.

---

### Task 11: HTTP API identity, nối dây, CLI tạo admin

**Files:**
- Create: `internal/identity/http.go`
- Modify: `cmd/api/main.go`
- Test: `internal/identity/http_test.go`

**Interfaces:**
- Consumes: mọi thứ ở trên.
- Produces: `func (s *Service) Routes(mux *http.ServeMux, iss *auth.Issuer)` đăng ký:

| Method + path | Auth | Body vào | Thành công | Rate limit (theo IP, mỗi phút) |
|---|---|---|---|---|
| `POST /v1/auth/register` | — | `{email,password}` | 202 `{}` | 10 |
| `POST /v1/auth/resend-verification` | — | `{email}` | 202 `{}` (email lạ cũng 202) | 5 |
| `POST /v1/auth/verify-email` | — | `{email,code,password}` | 200 `Tokens` | 20 |
| `POST /v1/auth/login` | — | `{email,password}` | 200 `Tokens` | 20 |
| `POST /v1/auth/google` | — | `{id_token}` | 200 `Tokens` | 20 |
| `POST /v1/auth/refresh` | — | `{refresh_token}` | 200 `Tokens` | 60 |
| `POST /v1/auth/logout` | — | `{refresh_token}` | 204 | 60 |
| `POST /v1/auth/password/forgot` | — | `{email}` | 202 `{}` | 5 |
| `POST /v1/auth/password/reset` | — | `{email,code,new_password}` | 204 | 10 |
| `GET /v1/me` | mọi role | — | 200 `{id,email,role}` | — |
| `GET /v1/customer/profile` | `customer` | — | 200 `Profile` | — |
| `PATCH /v1/customer/profile` | `customer` | `ProfileUpdate` | 200 `Profile` | — |

  `ResendVerification` với email lạ hoặc đã xác minh trả nil ở tầng HTTP (không lộ email). Riêng `ErrResendTooSoon` vẫn trả 429.
- `cmd/api/main.go`: subcommand `create-admin --email <e> --password <p>` gọi `CreateInternalUser(..., auth.RoleSuperAdmin)` rồi thoát. Không có subcommand thì chạy `serve`. Nếu `GOOGLE_CLIENT_ID` rỗng thì dùng verifier luôn trả `ErrGoogleTokenInvalid` và log cảnh báo khi khởi động.

- [ ] **Step 1: Viết test fail** `http_test.go`, dựng toàn bộ mux qua `httptest.NewServer`:
  - `TestHTTP_SignupFlow`: register → 202. Lấy mã từ `Recorder` → verify-email 200 có `access_token`. `GET /v1/me` với bearer → `role == "customer"`.
  - `TestHTTP_ProfileRequiresCustomer`: token partner gọi `GET /v1/customer/profile` → 403 `forbidden`. Không token → 401.
  - `TestHTTP_UnknownFieldRejected`: `POST /v1/auth/login` với `{"email":"a@b.com","password":"x","admin":true}` → 400 `invalid_request`.
  - `TestHTTP_ForgotUnknownEmail202`: → 202.
  - `TestHTTP_LoginRateLimited`: 21 lần login sai từ cùng IP → lần 21 nhận 429.
- [ ] **Step 2: Chạy.** Kỳ vọng: FAIL.
- [ ] **Step 3: Cài đặt** `http.go` và nối dây trong `main.go`.
- [ ] **Step 4: Chạy toàn bộ** `go vet ./... && go test -race ./...`. Kỳ vọng: PASS.
- [ ] **Step 5: Kiểm tra tay.** `make up`, `go run ./cmd/api create-admin --email admin@lunain.vn --password Admin12345`, `make run`. Đăng nhập qua `curl -s -XPOST localhost:8080/v1/auth/login -d '{"email":"admin@lunain.vn","password":"Admin12345"}'`. Kỳ vọng: JSON có `"role":"super_admin"`.
- [ ] **Step 6: Commit** `feat(identity): http api, wiring and create-admin command`.
