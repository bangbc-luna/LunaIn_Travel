# Plan 2 — Đối tác, loại dịch vụ, hoa hồng, địa giới, file, cấu hình — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Làm các bảng và module mà mọi plan sau đều dựa vào:
- cấu hình nền tảng, ngày lễ, nhật ký thao tác admin
- lưu file trên S3-compatible
- danh mục tỉnh/phường sau sáp nhập 2025
- loại dịch vụ và lịch sử % hoa hồng
- đối tác: tạo, bật loại dịch vụ, đình chỉ, hồ sơ, tài khoản ngân hàng

Kèm theo là API admin, API đối tác và API public tương ứng.

**Architecture:** Tiếp tục modular monolith của plan 1. Thêm các module mới:
- `internal/settings`
- `internal/files`
- `internal/geo`
- `internal/catalog` (plan 2 làm phần loại dịch vụ và hoa hồng, plan 3 thêm Listing)
- `internal/partner`

Thêm hai package nền tảng: `internal/platform/audit` và `internal/platform/storage`. Mỗi module có file `queries.sql` riêng và một mục riêng trong `sqlc.yaml`. Thao tác nào chạm nhiều module thì chạy trong **một** transaction, theo đúng thứ tự khóa của tài liệu DB §1.1.

**Tech Stack:** như plan 1, thêm `github.com/aws/aws-sdk-go-v2/service/s3` (S3-compatible, chạy được với MinIO hoặc Cloudflare R2), MinIO trong docker-compose, và `testcontainers-go/modules/minio` cho một test tích hợp.

**Spec:**
- [spec hệ thống](../specs/2026-11-04-lunain-travel-backend-design.md) §5.2, §5.3, §5.4 (phần địa giới), §14
- [thiết kế DB](../specs/2026-11-04-database-schema.md) §1, §4, §5.1, §5.2

## Global Constraints

- Mọi Global Constraints của plan 1 vẫn áp dụng: uuid v7, `created_at`/`updated_at`, time lấy từ clock được inject, tiền `bigint` VND, lỗi trả JSON `{"error":{"code","message"}}` với message tiếng Việt, test chạy Postgres thật, không mock DB, mỗi task kết thúc bằng `go vet ./... && go test -race ./...` xanh rồi commit.
- Thứ tự khóa (DB §1.1): `users` (`FOR NO KEY UPDATE`) → `partners` → ... → `partner_balances`. Thao tác nhiều module chạy chung một transaction. Hàm của module được gọi phải nhận `tx pgx.Tx` từ người gọi.
- Tỷ lệ hoa hồng lưu theo basis points `int`, giới hạn 0..10000. Không sửa dòng cũ của `commission_rates`, mỗi lần đổi thêm một dòng mới.
- Mọi thao tác ghi của Super Admin phải gọi `audit.Record` **trong cùng transaction**.
- API đối tác **không bao giờ** nhận `partner_id` từ client. `partner_id` luôn lấy từ phiên đăng nhập (spec §14).
- Migration mới bắt đầu từ `00005`, không sửa file `00001`–`00004`. Mỗi module mới có một mục riêng trong `sqlc.yaml` (schema là cả thư mục migration, queries là file của chính module đó), code sinh ra nằm ở `internal/<module>/store`.
- Enum dùng `text` + `CHECK`. Mọi trường `*_code` của địa giới có FK sang `provinces` hoặc `wards`.

## Review Focus

1. **Đối tác A sửa hoặc xem dữ liệu của đối tác B.** Mọi API `/v1/partner/*` chỉ chạm dữ liệu của đối tác gắn với phiên đăng nhập. Gửi kèm `partner_id` lạ trong body thì phải bị từ chối 400 (trường lạ), không được bỏ qua trong im lặng. Test ở Task 6.
2. **Gọi API admin bằng token đối tác hoặc token khách** thì trả 403 cho mọi route `/v1/admin/*`. Test ở Task 7, dạng bảng duyệt qua mọi route admin.
3. **Xem file private của người khác** (giấy phép kinh doanh của đối tác khác) phải trả 404, không phải 403, để không lộ việc file đó tồn tại. Test ở Task 2.
4. **Upload file giả loại** (file `.exe` đổi đuôi thành `.jpg`, hoặc file quá 10 MiB) phải trả 422 `invalid_file`, quyết định dựa trên nội dung file chứ không dựa vào header client gửi. Test ở Task 2.
5. **Tạo đối tác lỗi giữa chừng** (email chủ tài khoản đã tồn tại, mã loại dịch vụ sai) thì không được để lại dòng nào: không có user, partner hay audit mồ côi. Test ở Task 5.

---

## Cấu trúc file

```
internal/platform/audit/audit.go            # Record(ctx, tx, Entry)
internal/platform/storage/storage.go        # interface Storage + Memory (dùng cho test)
internal/platform/storage/s3.go             # S3 implementation
internal/settings/{queries.sql,service.go,http.go,store/}
internal/files/{queries.sql,service.go,http.go,store/}
internal/geo/{queries.sql,service.go,http.go,store/}
internal/geo/gen/main.go                    # sinh migration seed từ data/*.csv
internal/geo/data/{provinces.csv,wards.csv,README.md}
internal/catalog/{queries.sql,servicetype.go,commission.go,http.go,store/}
internal/partner/{queries.sql,service.go,member.go,profile.go,bank.go,http.go,errors.go,store/}
internal/platform/db/migrations/00005_platform.sql
internal/platform/db/migrations/00006_geo.sql
internal/platform/db/migrations/00007_geo_seed.sql   # sinh ra bởi geo/gen, commit
internal/platform/db/migrations/00008_catalog.sql
internal/platform/db/migrations/00009_partner.sql
```

---

### Task 1: Cấu hình nền tảng, ngày lễ, nhật ký admin

**Files:**
- Create: `internal/platform/db/migrations/00005_platform.sql`, `internal/platform/audit/audit.go`, `internal/settings/queries.sql`, `internal/settings/service.go`, `internal/settings/http.go`, `internal/settings/store/` (sinh ra)
- Modify: `sqlc.yaml`
- Test: `internal/platform/audit/audit_test.go`, `internal/settings/service_test.go`, `internal/settings/http_test.go`

**Schema** (theo DB §4):
- `admin_audit_log`: `id uuid PK`, `actor_user_id uuid NOT NULL REFERENCES users`, `action text NOT NULL`, `target_kind text NOT NULL`, `target_id uuid NULL`, `before jsonb NULL`, `after jsonb NULL`, `reason text NULL`, `created_at`, `updated_at`. Index `(target_kind, target_id, created_at DESC)`.
- `platform_settings`: `id smallint PK CHECK (id = 1)` + các cột giữ nguyên tên, kiểu, CHECK và mặc định như bảng ở DB §4. Thêm `updated_by uuid NULL REFERENCES users`. Seed đúng một dòng `id = 1` với các giá trị mặc định.
- `holidays`: `date date PRIMARY KEY`, `name text NOT NULL CHECK (btrim(name) <> '')`, `created_at`, `updated_at`.

**Interfaces:**
- Consumes: `db.InTx`, `clock.Clock`, `httpx.*`, `auth.Authenticate`, `auth.RequireRole`, `auth.PrincipalFrom`.
- Produces:
  - `audit.Entry{ActorUserID uuid.UUID; Action string; TargetKind string; TargetID *uuid.UUID; Before any; After any; Reason string}`
  - `func audit.Record(ctx context.Context, tx pgx.Tx, now time.Time, e Entry) error`: marshal `Before`/`After` thành JSON (nil thì để NULL).
  - `settings.Settings` có các trường Go tương ứng từng cột, JSON snake_case trùng tên cột: `HoldMinutes int`, `HoldGraceMinutes int`, `RequestResponseHours int`, `RequestPaymentHours int`, `CharterLateConfirmMinHours int`, `RideCheckinWindowMinutes int`, `RideNoShowAfterMinutes int`, `CharterAutoCompleteHours int`, `ReleaseDelayDays int`, `MinPayoutAmount int64`, `RescheduleWindowDays int`.
  - `settings.Patch`: cùng tập trường nhưng mỗi trường là con trỏ. Trường nil thì giữ nguyên.
  - `settings.NewService(pool *pgxpool.Pool, c clock.Clock) *Service`
  - `func (s *Service) Get(ctx context.Context) (Settings, error)`
  - `func (s *Service) GetTx(ctx context.Context, tx pgx.Tx) (Settings, error)`: plan 4 dùng khi tạo booking.
  - `func (s *Service) Update(ctx context.Context, actor uuid.UUID, p Patch) (Settings, error)`: validate theo đúng CHECK của DB, sai thì 422 `invalid_settings` với message nêu tên trường. Khóa dòng `FOR UPDATE`, cập nhật, gọi `audit.Record` với action `platform_settings.update`, `Before`/`After` là toàn bộ `Settings`.
  - `func (s *Service) ListHolidays(ctx context.Context, year int) ([]Holiday, error)`, `AddHoliday(ctx, actor, date time.Time, name string) error` (trùng ngày thì 409 `holiday_exists`), `RemoveHoliday(ctx, actor, date time.Time) error` (không có thì 404). Cả hai thao tác ghi đều gọi audit. **Ngoại lệ có chủ đích:** xóa ngày lễ là xóa cứng, vì đây là danh mục cấu hình, không phải dữ liệu nghiệp vụ.
  - `func (s *Service) IsHoliday(ctx context.Context, tx pgx.Tx, d time.Time) (bool, error)`: plan 8 dùng.
  - `func (s *Service) Routes(mux *http.ServeMux, iss *auth.Issuer)`:

    | Route | Quyền | Kết quả |
    |---|---|---|
    | `GET /v1/admin/settings` | `super_admin` | 200 `Settings` |
    | `PATCH /v1/admin/settings` | `super_admin` | 200 `Settings` |
    | `GET /v1/admin/holidays?year=2026` | `super_admin` | 200 `[{date,name}]` |
    | `POST /v1/admin/holidays` `{date:"YYYY-MM-DD",name}` | `super_admin` | 201 |
    | `DELETE /v1/admin/holidays/{date}` | `super_admin` | 204 |

- [ ] **Step 1: Viết migration** `00005_platform.sql` và `internal/settings/queries.sql`. Thêm mục `settings` vào `sqlc.yaml` (cùng overrides với mục identity). Chạy `make sqlc`. Kỳ vọng: sinh code không lỗi.
- [ ] **Step 2: Viết test fail.**
  - `audit_test.go` → `TestRecord_StoresJSONAndRollsBackWithTx`: Record trong `db.InTx` rồi trả lỗi, kỳ vọng 0 dòng. Record rồi commit, kỳ vọng 1 dòng, `before`/`after` là JSON đúng, `target_id` NULL khi không truyền.
  - `service_test.go`:
    - `TestGet_ReturnsSeededDefaults`: `HoldMinutes == 15`, `HoldGraceMinutes == 5`, `RequestResponseHours == 12`, `RequestPaymentHours == 2`, `ReleaseDelayDays == 1`, `MinPayoutAmount == 100000`, `RescheduleWindowDays == 14`.
    - `TestUpdate_PartialAndAudited`: chỉ đổi `HoldMinutes = 20`, các trường khác giữ nguyên, có đúng 1 dòng audit `platform_settings.update` với `before.hold_minutes = 15`, `after.hold_minutes = 20`.
    - `TestUpdate_RejectsInvalid`: `HoldMinutes = 0` cho 422 `invalid_settings`, message chứa `hold_minutes`, DB không đổi, không có audit.
    - `TestHolidays_AddListRemove`: thêm `2026-09-02`, thêm trùng thì 409, list năm 2026 có ngày đó, xóa rồi xóa lại thì 404.
  - `http_test.go` → `TestHTTP_SettingsRequireSuperAdmin`: token customer gọi `GET /v1/admin/settings` nhận 403, token super_admin nhận 200. Dùng `identity.CreateInternalUser` để tạo admin và `auth.Issuer` để cấp token.
- [ ] **Step 3: Chạy** `go test ./internal/platform/audit/... ./internal/settings/...`. Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt** audit, settings service, http.
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(settings): platform settings, holidays and admin audit log`.

---

### Task 2: Lưu file (S3-compatible)

**Files:**
- Create: `internal/platform/storage/storage.go`, `internal/platform/storage/s3.go`, `internal/files/{queries.sql,service.go,http.go,store/}`
- Modify: `internal/platform/db/migrations/00005_platform.sql` (thêm bảng `files`, vì task 1 cùng branch chưa merge nên được sửa file này), `internal/platform/config/config.go`, `.env.example`, `docker-compose.yml`, `sqlc.yaml`, `cmd/api/main.go`
- Test: `internal/platform/storage/s3_test.go`, `internal/files/service_test.go`, `internal/files/http_test.go`

**Schema** `files`: `id uuid PK`, `storage_key text UNIQUE NOT NULL`, `purpose text NOT NULL CHECK (purpose IN ('listing_image','review_image','business_license','payout_proof','refund_proof'))`, `visibility text NOT NULL CHECK (visibility IN ('public','private'))`, `content_type text NOT NULL`, `size_bytes bigint NOT NULL CHECK (size_bytes > 0)`, `uploaded_by uuid NOT NULL REFERENCES users`, `created_at`, `updated_at`.

**Quy tắc theo `purpose`** **[Chốt trong plan]**:

| purpose | visibility | Role được upload | Loại file nhận |
|---|---|---|---|
| `listing_image` | public | partner | image/jpeg, image/png, image/webp |
| `review_image` | public | customer | image/jpeg, image/png, image/webp |
| `business_license` | private | partner | image/jpeg, image/png, application/pdf |
| `payout_proof` | private | super_admin | image/jpeg, image/png, application/pdf |
| `refund_proof` | private | super_admin | image/jpeg, image/png, application/pdf |

- Kích thước tối đa **10 MiB**. Loại file xác định bằng `http.DetectContentType` trên 512 byte đầu, bỏ qua `Content-Type` client gửi.
- `storage_key` có dạng `{visibility}/{purpose}/{yyyy}/{mm}/{file_id}{ext}`, trong đó `ext` suy ra từ loại file đã phát hiện.
- File public có URL cố định `PUBLIC_FILE_BASE_URL + "/" + storage_key`. File private chỉ trả URL ký sẵn (presigned GET) hạn **5 phút**.

**Config mới** (`config.Config`): `S3Endpoint`, `S3Region` (mặc định `us-east-1`), `S3Bucket`, `S3AccessKey`, `S3SecretKey`, `S3UsePathStyle bool` (mặc định true), `PublicFileBaseURL`.
- `APP_ENV=prod` thì bắt buộc có cả bốn biến endpoint, bucket, key, secret.
- Ở dev, `docker-compose.yml` thêm service `minio` (`minio/minio`, cổng `127.0.0.1:9000` và `9001`) và một service một lần chạy `mc` để tạo bucket `lunain` với policy public-read cho prefix `public/`. `.env.example` thêm các biến S3 trỏ tới MinIO.

**Interfaces:**
- Produces:
  - `storage.Storage interface{ Put(ctx context.Context, key, contentType string, body io.Reader, size int64) error; PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) }`
  - `storage.NewS3(ctx context.Context, cfg S3Config) (*S3, error)`
  - `storage.Memory` (map có mutex, dùng cho test). `PresignGet` trả `"memory://" + key + "?ttl=" + ttl`.
  - `files.NewService(pool *pgxpool.Pool, st storage.Storage, c clock.Clock, publicBaseURL string) *Service`
  - `files.File{ID uuid.UUID; Purpose string; Visibility string; ContentType string; SizeBytes int64; UploadedBy uuid.UUID; URL string}` (`URL` chỉ có với file public)
  - `func (s *Service) Upload(ctx context.Context, p auth.Principal, purpose string, r io.Reader) (File, error)`: đọc tối đa 10 MiB + 1 byte để phát hiện quá cỡ. Sai purpose, sai role, sai loại hoặc quá cỡ thì 422 `invalid_file` (sai role thì 403 `forbidden`). Put lên storage **trước**, insert dòng DB **sau**. Insert lỗi thì object bị bỏ lại, chấp nhận được vì không có dòng DB nào trỏ tới.
  - `func (s *Service) URL(ctx context.Context, p auth.Principal, id uuid.UUID) (string, error)`: public thì trả URL cố định. Private thì chỉ người upload hoặc `super_admin` nhận được presigned URL, người khác nhận 404 `not_found`.
  - `func (s *Service) Owned(ctx context.Context, tx pgx.Tx, id uuid.UUID, uploadedBy uuid.UUID, purpose string) error`: kiểm tra file tồn tại, đúng người upload và đúng purpose, sai thì 422 `invalid_file`. Task 6 và các plan sau dùng hàm này trước khi gắn `file_id` vào bản ghi.
  - `func (s *Service) Routes(mux *http.ServeMux, iss *auth.Issuer, clientIP func(*http.Request) string)`:
    - `POST /v1/files`: multipart, trường `purpose` và `file`, cần đăng nhập, rate limit 30/phút theo IP. Trả 201 `File`.
    - `GET /v1/files/{id}/url`: cần đăng nhập. Trả 200 `{"url": "..."}`.

- [ ] **Step 1: Viết test fail.**
  - `files/service_test.go`, dùng `storage.Memory`:
    - `TestUpload_ListingImageByPartnerIsPublic`: nhận PNG thật (dựng bằng `image/png` encode 1×1), kỳ vọng `Visibility == "public"`, `URL` bắt đầu bằng base URL, object tồn tại trong Memory.
    - `TestUpload_RejectsDisguisedFile`: nội dung `MZ\x90\x00...` với purpose `listing_image` cho 422 `invalid_file`, không có object, không có dòng DB.
    - `TestUpload_RejectsOversize`: 10 MiB + 1 byte PNG header cho 422 `invalid_file`.
    - `TestUpload_RejectsWrongRole`: customer upload `business_license` cho 403.
    - `TestURL_PrivateOnlyOwnerOrAdmin`: partner A upload `business_license`. A và super_admin nhận URL bắt đầu bằng `memory://`. Partner B và customer nhận 404 `not_found`.
    - `TestOwned_ChecksUploaderAndPurpose`.
  - `files/http_test.go` → `TestHTTP_UploadMultipart`: multipart thật qua `httptest`, kỳ vọng 201 và có `id`. Không gửi token thì 401.
  - `storage/s3_test.go` → `TestS3_PutAndPresign`: dùng testcontainers module minio (bỏ qua khi `-short`), tạo bucket, Put rồi tải lại bằng presigned URL qua `http.Get`, nội dung phải khớp.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt** storage (Memory và S3), files service, http, config, phần compose cho MinIO, nối dây trong `main.go`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Kiểm tra tay.** `docker compose up -d minio minio-init`, chạy server, đăng nhập admin (tài khoản `admin@lunain.vn` trên DB dev), upload một PNG `payout_proof`, gọi `GET /v1/files/{id}/url`, rồi `curl` URL đó. Kỳ vọng nhận lại đúng file.
- [ ] **Step 6: Commit** `feat(files): s3-compatible file storage with purpose-based access`.

---

### Task 3: Danh mục địa giới (34 tỉnh/thành, phường/xã sau sáp nhập 2025)

**Files:**
- Create: `internal/geo/data/provinces.csv`, `internal/geo/data/wards.csv`, `internal/geo/data/README.md`, `internal/geo/gen/main.go`, `internal/platform/db/migrations/00006_geo.sql`, `internal/platform/db/migrations/00007_geo_seed.sql` (sinh ra), `internal/geo/{queries.sql,service.go,http.go,store/}`
- Modify: `sqlc.yaml`, `cmd/api/main.go`
- Test: `internal/geo/service_test.go`

**Schema** `00006_geo.sql`:
- `provinces`: `code text PK`, `name text NOT NULL`, `slug text NOT NULL UNIQUE`, `sort_order int NOT NULL`, `created_at`, `updated_at`.
- `wards`: `code text PK`, `province_code text NOT NULL REFERENCES provinces`, `name text NOT NULL`, `kind text NOT NULL CHECK (kind IN ('xa','phuong','dac_khu'))`, `slug text NOT NULL`, `UNIQUE (province_code, slug)`, `created_at`, `updated_at`. Index `(province_code)`.
- `slug` là tên bỏ dấu, viết thường, nối bằng `-` (ví dụ `Thành phố Hồ Chí Minh` thành `thanh-pho-ho-chi-minh`). `đ` đổi thành `d`.

**Nguồn dữ liệu:**
- Dữ liệu là danh mục và mã đơn vị hành chính theo Quyết định 19/2025/QĐ-TTg, hiệu lực từ 01/07/2025. Có **34** tỉnh/thành và khoảng **3.321** đơn vị cấp xã (xã, phường, đặc khu).
- Implementer lấy từ nguồn chính thức hoặc nguồn công khai đáng tin, chuyển thành CSV (`code,name,sort_order` và `code,province_code,name,kind`), rồi ghi **URL nguồn và ngày lấy** vào `data/README.md`.
- Không tự gõ tay dữ liệu. Không lấy được nguồn đáng tin thì trả trạng thái `NEEDS_CONTEXT`.

**Interfaces:**
- Produces:
  - `go run ./internal/geo/gen` đọc 2 file CSV và ghi `00007_geo_seed.sql` (`-- +goose Up` với `INSERT ... VALUES` theo lô 500 dòng, `-- +goose Down` xóa hết). Chạy lại phải cho ra file giống hệt (deterministic, sắp theo `code`).
  - `geo.NewService(pool *pgxpool.Pool) *Service`
  - `func (s *Service) Provinces(ctx context.Context) ([]Province, error)`: sắp theo `sort_order`. `Province{Code, Name, Slug string}`.
  - `func (s *Service) Wards(ctx context.Context, provinceCode string) ([]Ward, error)`: sắp theo `name`. Mã tỉnh không tồn tại thì 404. `Ward{Code, ProvinceCode, Name, Kind, Slug string}`.
  - `func (s *Service) ValidateLocation(ctx context.Context, tx pgx.Tx, provinceCode, wardCode string) error`: phường phải thuộc đúng tỉnh, sai thì 422 `invalid_location`. Task 6 và plan 3 dùng hàm này.
  - Routes public (không cần đăng nhập, có header `Cache-Control: public, max-age=86400`): `GET /v1/geo/provinces`, `GET /v1/geo/provinces/{code}/wards`.

- [ ] **Step 1: Thu thập dữ liệu** vào CSV và viết README nguồn.
- [ ] **Step 2: Viết test fail** `service_test.go`:
  - `TestSeed_Has34Provinces`: đếm được đúng 34.
  - `TestSeed_WardCountPlausible`: tổng số phường/xã nằm trong khoảng 3.200 đến 3.400. Mỗi tỉnh có ít nhất 1 phường/xã. Không có slug rỗng.
  - `TestSeed_KnownEntries`: có tỉnh tên `Thành phố Hồ Chí Minh` và `Thành phố Hà Nội`. Không còn tỉnh tên `Bà Rịa - Vũng Tàu` vì đã sáp nhập.
  - `TestWards_UnknownProvince404`.
  - `TestValidateLocation_WardMustBelongToProvince`.
  - `TestGen_Deterministic`: chạy hàm sinh 2 lần vào bộ nhớ, kết quả giống nhau byte-by-byte. Tách logic sinh thành hàm thuần `Render(provinces, wards) []byte` để test được.
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt** generator, chạy `go run ./internal/geo/gen`, rồi cài service, http, mục sqlc.
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(geo): post-2025 administrative units catalog`.

---

### Task 4: Loại dịch vụ và hoa hồng

**Files:**
- Create: `internal/platform/db/migrations/00008_catalog.sql`, `internal/catalog/{queries.sql,servicetype.go,commission.go,http.go,errors.go,store/}`
- Modify: `sqlc.yaml`, `cmd/api/main.go`
- Test: `internal/catalog/commission_test.go`, `internal/catalog/http_test.go`

**Schema** (DB §5.2):
- `service_types`: `code text PK`, `name text NOT NULL`, `booking_mode text NOT NULL CHECK (booking_mode IN ('instant','request'))`, `sort_order int NOT NULL`, `created_at`, `updated_at`. Seed 4 dòng:

  | code | name | booking_mode | sort_order |
  |---|---|---|---|
  | `attraction_ticket` | Vé tham quan | instant | 1 |
  | `scheduled_ride` | Xe tuyến cố định | instant | 2 |
  | `private_charter` | Xe bao chuyến | request | 3 |
  | `self_drive_rental` | Thuê xe tự lái | request | 4 |

- `commission_rates`: `id uuid PK`, `service_type text NOT NULL REFERENCES service_types`, `rate_bps int NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000)`, `effective_from timestamptz NOT NULL`, `created_by uuid NOT NULL REFERENCES users`, `created_at`, `updated_at`, `UNIQUE (service_type, effective_from)`.
- **Không seed `commission_rates`.** Loại dịch vụ chưa có % nào thì chưa bán được (plan 4 sẽ chặn), để tránh vô tình bán với hoa hồng 0%.

**Interfaces:**
- Produces:
  - `catalog.ServiceType{Code, Name, BookingMode string; SortOrder int}`
  - `catalog.NewService(pool *pgxpool.Pool, c clock.Clock) *Service`
  - `func (s *Service) ServiceTypes(ctx context.Context) ([]ServiceType, error)`
  - `func (s *Service) ServiceType(ctx context.Context, tx pgx.Tx, code string) (ServiceType, error)`: không có thì 422 `invalid_service_type`.
  - `func (s *Service) CreateCommissionRate(ctx context.Context, actor uuid.UUID, serviceType string, rateBps int, effectiveFrom *time.Time) (CommissionRate, error)`:
    - `effectiveFrom` nil thì lấy `clock.Now()`. Nếu có thì phải ≥ `clock.Now()` trừ 1 phút, không cho đặt lùi ngày, sai thì 422 `invalid_commission_rate`.
    - `rateBps` phải trong khoảng 0..10000, sai cũng trả 422.
    - Trùng `(service_type, effective_from)` thì 409 `commission_rate_exists`.
    - Ghi audit `commission_rate.create`.
  - `func (s *Service) CurrentCommissionRate(ctx context.Context, tx pgx.Tx, serviceType string, at time.Time) (int, error)`: lấy dòng có `effective_from` lớn nhất mà ≤ `at`. Không có thì `ErrNoCommissionRate` (422 `no_commission_rate`, "Loại dịch vụ này chưa được cấu hình hoa hồng"). Plan 4 dùng khi tạo booking.
  - `func (s *Service) CommissionHistory(ctx context.Context, serviceType string) ([]CommissionRate, error)`: mới nhất trước. `CommissionRate{ID; ServiceType; RateBps int; EffectiveFrom time.Time; CreatedBy uuid.UUID}`.
  - Routes:
    - `GET /v1/service-types`: public, trả danh sách loại dịch vụ.
    - `GET /v1/admin/commission-rates`: super_admin. Trả cho mỗi loại dịch vụ `{service_type, current_rate_bps|null, scheduled:[...rate có effective_from > now]}`.
    - `GET /v1/admin/commission-rates/{service_type}/history`: super_admin.
    - `POST /v1/admin/commission-rates` `{service_type, rate_bps, effective_from?}`: super_admin, trả 201.

- [ ] **Step 1: Viết migration và queries, thêm mục sqlc, chạy `make sqlc`.**
- [ ] **Step 2: Viết test fail** `commission_test.go`:
  - `TestCurrentRate_NoneConfigured`: trả `ErrNoCommissionRate`.
  - `TestCurrentRate_PicksLatestEffective`: tạo 1000 bps có hiệu lực ngay, rồi 1200 bps có hiệu lực từ now + 1 ngày. `at = now` cho 1000, `at = now + 1 ngày` cho 1200. Đây là kiểm chứng cho quy tắc đổi % không ảnh hưởng đơn cũ.
  - `TestCreateRate_RejectsBackdated`: `effective_from = now - 2 phút` cho 422.
  - `TestCreateRate_RejectsOutOfRange`: 10001 cho 422, -1 cho 422.
  - `TestCreateRate_Audited`: có 1 dòng audit `commission_rate.create`, `after.rate_bps == 1000`.
  - `TestCreateRate_UnknownServiceType`: 422 `invalid_service_type`.
  - `http_test.go` → `TestHTTP_ServiceTypesPublic` (200, đủ 4 loại, đúng thứ tự) và `TestHTTP_CommissionAdminOnly` (token partner nhận 403).
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt.**
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(catalog): service types and commission rate history`.

---

### Task 5: Đối tác — tạo, bật loại dịch vụ, đình chỉ (phía Super Admin)

**Files:**
- Create: `internal/platform/db/migrations/00009_partner.sql`, `internal/partner/{queries.sql,service.go,errors.go,http.go,store/}`
- Modify: `sqlc.yaml`, `cmd/api/main.go`
- Test: `internal/partner/service_test.go`, `internal/partner/helpers_test.go`

**Schema** (DB §5.1):
- `partners`: các cột đúng như DB §5.1.
  - `status CHECK (status IN ('active','suspended'))`, mặc định `active`.
  - `tax_code text NULL UNIQUE`.
  - `business_license_file_id uuid NULL REFERENCES files`.
  - `province_code` và `ward_code` NULL, có FK.
  - `rating_sum int NOT NULL DEFAULT 0 CHECK (>= 0)`, `rating_count` tương tự.
  - `created_by uuid NOT NULL REFERENCES users`.
  - `CHECK (status <> 'suspended' OR suspended_at IS NOT NULL)`.
- `partner_members`: `partner_id uuid REFERENCES partners`, `user_id uuid NOT NULL UNIQUE REFERENCES users`, `role text NOT NULL CHECK (role IN ('owner'))`, PK `(partner_id, user_id)`, `created_at`, `updated_at`.
- `partner_service_types`: `partner_id`, `service_type REFERENCES service_types`, `enabled_by uuid REFERENCES users`, `enabled_at timestamptz NOT NULL`, `disabled_at timestamptz NULL`, `disabled_by uuid NULL`, PK `(partner_id, service_type)`, `created_at`, `updated_at`.
- `partner_bank_accounts`: các cột đúng như DB §5.1, cộng unique index `(partner_id) WHERE is_current`. Task 6 dùng.

**Interfaces:**
- Consumes:
  - identity: `(*identity.Service).CreateInternalUser(ctx, tx, email, password, auth.RolePartner) (uuid.UUID, error)`, gọi qua interface hẹp `partner.UserCreator`.
  - `(*catalog.Service).ServiceType(ctx, tx, code)`.
  - `audit.Record`.
- Produces:
  - `partner.Deps{Pool; Clock; Users UserCreator; Catalog ServiceTypeChecker; Geo LocationValidator; Files FileChecker}`. Các interface hẹp được khai báo trong package `partner`. Task 6 dùng `Geo` và `Files`.
  - `partner.NewService(d Deps) *Service`
  - `partner.CreateInput{LegalName, DisplayName, ContactName, ContactPhone, ContactEmail, OwnerEmail, OwnerPassword string; ServiceTypes []string}`
  - `func (s *Service) Create(ctx context.Context, actor uuid.UUID, in CreateInput) (Partner, error)`:
    - Thực hiện trong **một** transaction, theo thứ tự: tạo user (role partner, đã xác minh), tạo `partners`, tạo `partner_members` với role owner, bật từng loại dịch vụ, ghi audit `partner.create` (không lưu mật khẩu vào audit).
    - Tên rỗng hoặc dài quá 200 ký tự thì 422 `invalid_partner`. `ServiceTypes` có thể rỗng. Mã loại dịch vụ trùng nhau trong danh sách thì gộp lại.
  - `partner.Partner{ID; LegalName; DisplayName; TaxCode *string; BusinessLicenseFileID *uuid.UUID; Address *string; ProvinceCode *string; WardCode *string; ContactName; ContactPhone; ContactEmail string; Status string; SuspendedAt *time.Time; SuspendedReason *string; ServiceTypes []string (chỉ các loại đang bật); OwnerUserID uuid.UUID; CreatedAt time.Time}`
  - `func (s *Service) Get(ctx context.Context, id uuid.UUID) (Partner, error)`: không có thì 404.
  - `func (s *Service) List(ctx context.Context, f ListFilter) ([]Partner, *uuid.UUID, error)`: phân trang theo con trỏ `id` (uuid v7 tăng theo thời gian, sắp mới nhất trước). `ListFilter{Status *string; Query string (tìm trong legal_name, display_name không phân biệt hoa thường); Limit int (mặc định 20, tối đa 100); Cursor *uuid.UUID}`. Trả về con trỏ trang sau.
  - `func (s *Service) EnableServiceType(ctx, actor, partnerID uuid.UUID, code string) error`: idempotent. Đang tắt thì bật lại (đặt `disabled_at = NULL`, cập nhật `enabled_at` và `enabled_by`). Ghi audit.
  - `func (s *Service) DisableServiceType(ctx, actor, partnerID uuid.UUID, code string) error`: đặt `disabled_at`. Chưa từng bật thì 404. Ghi audit. Plan 3 sẽ dùng trạng thái này để ẩn Listing (DB §6).
  - `func (s *Service) Suspend(ctx, actor, partnerID uuid.UUID, reason string) error`: `reason` bắt buộc, sai thì 422. Đang suspended thì 409 `partner_already_suspended`. Khóa dòng `partners FOR UPDATE`. Ghi audit `partner.suspend`.
  - `func (s *Service) Unsuspend(ctx, actor, partnerID uuid.UUID) error`: đối xứng với `Suspend`, đang active thì 409.
  - `func (s *Service) IsServiceTypeEnabled(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, code string) (bool, error)` và `func (s *Service) Status(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, lock bool) (string, error)`. Khi `lock == true` thì đọc với `FOR SHARE` (DB §1.1). Plan 3 và 4 dùng.

- [ ] **Step 1: Viết migration và queries, thêm mục sqlc, chạy `make sqlc`.**
- [ ] **Step 2: Viết helper test** `helpers_test.go`, dựng service thật của identity, catalog, geo, files (dùng `storage.Memory`) và partner trên `testutil.Postgres`, cùng clock cố định `2026-11-04T09:00:00+07:00`. Kèm hàm tạo super_admin.
- [ ] **Step 3: Viết test fail** `service_test.go`:
  - `TestCreate_CreatesUserPartnerMemberAndServiceTypes`: owner đăng nhập được với role `partner`. `Partner.ServiceTypes` là `["attraction_ticket","private_charter"]` theo thứ tự `sort_order`. Có 1 dòng audit `partner.create` mà `after` không chứa mật khẩu (assert chuỗi JSON không chứa mật khẩu).
  - `TestCreate_DuplicateOwnerEmailLeavesNothing`: email đã tồn tại thì `identity.ErrEmailTaken`, số dòng `partners`, `partner_members` và `admin_audit_log` giữ nguyên như trước (Review Focus 5).
  - `TestCreate_UnknownServiceTypeLeavesNothing`: `["nope"]` thì 422 `invalid_service_type`, không có user mới.
  - `TestEnableDisableServiceType`: tắt rồi `IsServiceTypeEnabled` trả false. Bật lại thì true. Bật hai lần không lỗi. Tắt loại chưa từng bật thì 404.
  - `TestSuspendUnsuspend`: đình chỉ thì `Status == "suspended"` và `SuspendedAt` bằng clock. Đình chỉ lần nữa thì 409. Bỏ đình chỉ thì active. Có đủ 2 dòng audit. User của đối tác **vẫn đăng nhập được** (spec §5.2).
  - `TestList_PaginatesAndFilters`: tạo 3 đối tác, `Limit = 2` thì trang 1 có 2, trang 2 có 1. Lọc `Status = "suspended"`. Tìm `Query` không phân biệt hoa thường.
- [ ] **Step 4: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 5: Cài đặt** service (chưa cần HTTP admin ở task này).
- [ ] **Step 6: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 7: Commit** `feat(partner): create partners, toggle service types, suspend`.

---

### Task 6: Đối tác tự phục vụ — xác định đối tác từ phiên, hồ sơ, tài khoản ngân hàng

**Files:**
- Create: `internal/partner/member.go`, `internal/partner/profile.go`, `internal/partner/bank.go`
- Modify: `internal/partner/queries.sql`, `internal/partner/http.go`
- Test: `internal/partner/member_test.go`, `internal/partner/profile_test.go`, `internal/partner/bank_test.go`

**Interfaces:**
- Produces:
  - `func (s *Service) RequireMember() func(http.Handler) http.Handler`: đặt sau `auth.Authenticate` và `auth.RequireRole(auth.RolePartner)`. Tra `partner_members` theo `UserID` của Principal. Không có thì 403 `forbidden`. Có thì gắn vào context.
  - `func partner.IDFrom(ctx context.Context) (uuid.UUID, bool)`: **cách duy nhất** để handler phía đối tác lấy `partner_id`. Plan 3+ dùng.
  - `partner.ProfileUpdate{DisplayName, TaxCode, Address, ProvinceCode, WardCode, ContactName, ContactPhone, ContactEmail *string; BusinessLicenseFileID *uuid.UUID}`: trường nil thì giữ nguyên. `legal_name` **không** cho đối tác tự sửa, chỉ admin sửa được (plan 9).
  - `func (s *Service) UpdateProfile(ctx context.Context, partnerID, actorUserID uuid.UUID, u ProfileUpdate) (Partner, error)`:
    - Validate độ dài (tối đa 200), số điện thoại theo đúng quy tắc của identity (dùng chung một hàm, không chép lại), email theo `identity.NormalizeEmail`.
    - Tỉnh và phường đi qua `Geo.ValidateLocation`. Hai trường này phải cùng có hoặc cùng không, sai thì 422 `invalid_location`.
    - File giấy phép đi qua `Files.Owned(..., actorUserID, "business_license")`.
    - `tax_code` trùng với đối tác khác thì 409 `tax_code_taken`.
    - Đối tác đang bị đình chỉ **vẫn sửa được hồ sơ**.
  - `partner.BankAccount{ID; BankBIN, BankName, AccountNumber, AccountHolder string; IsCurrent bool; CreatedAt time.Time}`
  - `func (s *Service) SetBankAccount(ctx context.Context, partnerID, actorUserID uuid.UUID, in BankAccountInput) (BankAccount, error)`:
    - `bank_bin` gồm 6 chữ số. `account_number` gồm 6–19 chữ số. `account_holder` không rỗng, chuyển sang chữ IN HOA không dấu (giống tên trên thẻ ngân hàng).
    - Trong cùng transaction: khóa dòng `partners`, tắt `is_current` của tài khoản cũ, thêm tài khoản mới với `is_current = true`.
  - `func (s *Service) BankAccounts(ctx context.Context, partnerID uuid.UUID) ([]BankAccount, error)` (mới nhất trước) và `func (s *Service) CurrentBankAccount(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID) (*BankAccount, error)` (plan 5 dùng khi chụp lại tài khoản vào payout).
  - Routes đối tác, đều đi qua `RequireRole(partner)` và `RequireMember`:

    | Route | Kết quả |
    |---|---|
    | `GET /v1/partner/profile` | 200 `Partner` |
    | `PATCH /v1/partner/profile` | 200 `Partner` |
    | `GET /v1/partner/bank-accounts` | 200 `[]BankAccount` |
    | `PUT /v1/partner/bank-account` | 200 `BankAccount` |

- [ ] **Step 1: Viết test fail.**
  - `member_test.go`:
    - `TestRequireMember_SetsPartnerID`: handler đọc `IDFrom` và nhận đúng id của đối tác A.
    - `TestRequireMember_PartnerRoleWithoutMembership403`: tạo user role partner bằng `CreateInternalUser` nhưng không gắn đối tác nào, kỳ vọng 403.
  - `profile_test.go`:
    - `TestUpdateProfile_Partial`.
    - `TestUpdateProfile_WardMustMatchProvince`: 422 `invalid_location`.
    - `TestUpdateProfile_LicenseFileMustBeOwnBusinessLicense`: file của đối tác B, hoặc file có purpose `listing_image`, đều cho 422 `invalid_file`.
    - `TestUpdateProfile_TaxCodeTaken409`.
    - `TestUpdateProfile_SuspendedStillAllowed`.
    - `TestHTTP_PartnerCannotTargetAnotherPartner`: token đối tác A `PATCH /v1/partner/profile` với body `{"display_name":"X","partner_id":"<id của B>"}` nhận 400 `invalid_request`. Gửi `{"display_name":"X"}` thì chỉ đối tác A đổi, B giữ nguyên (Review Focus 1).
  - `bank_test.go`:
    - `TestSetBankAccount_RotatesCurrent`: đặt hai lần thì có 2 dòng, đúng 1 dòng `is_current`, dòng mới nhất là current.
    - `TestSetBankAccount_Validation`: BIN 5 số hoặc số tài khoản có chữ đều cho 422.
    - `TestSetBankAccount_HolderUppercasedNoDiacritics`: `"Nguyễn Văn An"` thành `"NGUYEN VAN AN"`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Nếu hàm chuẩn hóa số điện thoại của identity đang private thì đưa ra package dùng chung `internal/platform/validate`, cả identity và partner cùng import. Chỉnh identity tối thiểu, các test cũ phải vẫn xanh.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(partner): partner self-service profile and bank accounts`.

---

### Task 7: API admin đối tác, nối dây, kiểm tra phân quyền toàn bộ

**Files:**
- Modify: `internal/partner/http.go`, `cmd/api/main.go`
- Test: `internal/partner/http_admin_test.go`, `cmd/api/routes_test.go`

**Interfaces:**
- Produces: các route admin của đối tác (super_admin):

  | Route | Body | Kết quả |
  |---|---|---|
  | `POST /v1/admin/partners` | `CreateInput` (snake_case) | 201 `Partner` |
  | `GET /v1/admin/partners?status=&q=&limit=&cursor=` | | 200 `{items:[Partner], next_cursor}` |
  | `GET /v1/admin/partners/{id}` | | 200 `Partner` |
  | `PUT /v1/admin/partners/{id}/service-types/{code}` | | 204 |
  | `DELETE /v1/admin/partners/{id}/service-types/{code}` | | 204 |
  | `POST /v1/admin/partners/{id}/suspend` | `{reason}` | 204 |
  | `POST /v1/admin/partners/{id}/unsuspend` | | 204 |

- `cmd/api/main.go` gom việc đăng ký route vào hàm `buildMux(deps) *http.ServeMux` để test được. Hàm này nối settings, files, geo, catalog, partner và giữ nguyên identity cùng route `/` trả 404 JSON.

- [ ] **Step 1: Viết test fail.**
  - `http_admin_test.go` → `TestHTTP_AdminCreatePartnerFlow`: admin tạo đối tác. Owner đăng nhập được. `GET /v1/partner/profile` bằng token owner nhận đúng tên đối tác.
  - `routes_test.go` → `TestAllAdminRoutesRejectNonAdmins`:
    - Dựng mux thật bằng `buildMux` trên `testutil.Postgres` và `storage.Memory`.
    - Duyệt qua **danh sách mọi route `/v1/admin/*`** (khai báo danh sách này trong test và assert nó khớp với số route admin đã đăng ký, để route thêm sau không lọt khỏi test).
    - Mỗi route: không có token nhận 401, token customer nhận 403, token partner nhận 403 (Review Focus 2).
  - `routes_test.go` → `TestPublicRoutes`: `GET /v1/geo/provinces` và `GET /v1/service-types` trả 200 khi không có token.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt** route admin của đối tác và `buildMux`. Mọi route admin đều qua `auth.RequireRole(auth.RoleSuperAdmin)`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Kiểm tra tay** trên DB dev (Postgres 5434, MinIO):
  - Đăng nhập `admin@lunain.vn`.
  - Tạo commission 1000 bps cho `attraction_ticket`.
  - Tạo một đối tác có loại `attraction_ticket`.
  - Đăng nhập bằng owner của đối tác đó, upload ảnh `business_license`, rồi `PATCH /v1/partner/profile` gắn file vừa upload.

  Kỳ vọng mọi bước thành công và `GET /v1/admin/partners/{id}` thấy `business_license_file_id`.
- [ ] **Step 6: Commit** `feat(partner): admin partner api and route wiring`.

---

## Không làm trong plan này

- Listing, ẩn Listing khi đối tác bị đình chỉ hoặc loại dịch vụ bị tắt (plan 3).
- `partner_balances` và sổ cái (plan 4). Dòng số dư sẽ được tạo lười khi phát sinh bút toán đầu tiên.
- Admin sửa `legal_name` hoặc liên hệ của đối tác, xem nhật ký audit qua API (plan 9).
- Các lỗi Minor còn hoãn từ plan 1 (IPv6 /64, giới hạn đăng nhập theo tài khoản, bắt buộc `APP_ENV`, đăng nhập Google lười) được ghi vào lộ trình và làm ở plan 9.
