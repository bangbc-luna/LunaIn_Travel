# Plan 3 — Listing lớp chung + Vé tham quan — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Đối tác tạo được Listing và gửi duyệt, Super Admin duyệt, từ chối hoặc ẩn, khách tìm và xem được Listing đang hiển thị. Plan này làm đầy đủ phần Attraction: loại vé, khung giờ, ngày nghỉ. Kèm theo là các hàm mà plan 4 (booking) sẽ gọi: kiểm tra Listing có đặt được không, khung giờ của một ngày, tra loại vé, tính % hoàn theo chính sách hủy.

**Architecture:** Thêm module mới `internal/listing`. Không đặt trong `internal/catalog`, vì `partner` đã import `catalog`, nếu `catalog` cũng cần `partner.IDFrom` thì sẽ import vòng. Spec gọi khối này là "catalog". Ở mức code, `catalog` giữ loại dịch vụ và hoa hồng, còn `listing` giữ Listing và chi tiết theo từng loại dịch vụ. `listing` chỉ gọi module khác qua interface hẹp (partner, catalog, files, geo).

**Tech Stack:** như plan 1–2. Thêm extension Postgres `pg_trgm` và `unaccent` để tìm kiếm không dấu.

**Spec:**
- [spec hệ thống](../specs/2026-11-04-lunain-travel-backend-design.md) §5.4, §5.5
- [thiết kế DB](../specs/2026-11-04-database-schema.md) §1 (§1.1 thứ tự khóa, §1.2 khóa ngoại kép), §5.3, §5.4

## Global Constraints

- Mọi Global Constraints của plan 1–2 vẫn áp dụng:
  - uuid v7, `created_at`/`updated_at`, thời gian lấy từ clock được inject, tiền `bigint` VND
  - lỗi trả JSON tiếng Việt, test chạy Postgres thật
  - mỗi task kết thúc bằng `go vet ./... && go test -race ./...` xanh rồi commit
  - mỗi module có mục riêng trong `sqlc.yaml`
  - migration mới từ `00010`, không sửa `00001`–`00009`
- Thứ tự khóa (DB §1.1): `users` → `partners` → `listings` → ...
  - Dòng `listings` khóa bằng `FOR NO KEY UPDATE` (giống `partners`), vì bookings sau này có FK trỏ về `listings`.
  - Khi cần chặn đối tác bị đình chỉ hoặc loại dịch vụ bị tắt cùng lúc, gọi `partner.Status(ctx, tx, partnerID, true)` (`FOR SHARE`) **trước** `IsServiceTypeEnabled`, trong cùng transaction.
- Đối tác chỉ chạm được Listing của mình. `partner_id` luôn lấy từ `partner.IDFrom`. Listing của đối tác khác, hoặc id sai định dạng, đều trả **404** `not_found`, không phải 403.
- Đối tác đang bị **đình chỉ** thì không tạo hoặc sửa được Listing (spec §5.2), trả 403 `partner_suspended`. Họ vẫn đọc được Listing của mình. Loại dịch vụ chưa bật hoặc đã tắt cho đối tác thì không tạo được Listing loại đó, trả 422 `service_type_not_enabled`.
- Mọi thao tác ghi của Super Admin đi kèm `audit.Record` trong cùng transaction. Mọi lần đối tác sửa Listing (kể cả ảnh, chính sách hủy, loại vé, khung giờ, ngày nghỉ) ghi một dòng `listing_audit_log` trong cùng transaction, với `changes` dạng `{field: {old, new}}`.
- **[Chốt]** Chỉ duyệt lần đầu. Đã `approved` thì đối tác sửa trực tiếp, Listing vẫn hiển thị.
- Không xóa cứng loại vé hoặc khung giờ, vì booking sau này tham chiếu tới. Muốn bỏ thì đặt `active = false`. Ngày nghỉ là cấu hình nên được xóa cứng.
- Khách chỉ thấy Listing có `review_status = 'approved'`, `visibility = 'published'`, đối tác đang `active`, và loại dịch vụ của Listing đang được bật cho đối tác.
- Mỗi route mới dưới `/v1/admin/*` hoặc `/v1/partner/*` phải được thêm vào `adminRoutes` hoặc `partnerRoutes` trong `cmd/api/routes_test.go`. Test kiểm tra danh sách khớp tuyệt đối sẽ fail nếu thiếu.
- **Quy ước HTTP:** id hoặc mã trên path không trỏ tới tài nguyên nào thì trả 404. Lỗi ở body thì trả 422. Body có trường lạ thì trả 400.

## Review Focus

1. **Đối tác A xem hoặc sửa Listing của B qua id** phải nhận 404, và dữ liệu của B không đổi. Áp dụng cho mọi route `/v1/partner/listings/{id}/*`. Test ở Task 1 (lõi), Task 2–3 (các route con).
2. **Đối tác bị đình chỉ** tạo hoặc sửa Listing thì nhận 403 `partner_suspended`. Listing đã duyệt của họ biến mất khỏi kết quả tìm kiếm public, và `BookableListing` trả `ErrListingNotBookable`. Test ở Task 1 và Task 5.
3. **Chính sách hủy sai** (mốc trùng, % tăng khi càng sát giờ, quá 10 mốc, `refund_bps` ngoài 0..10000) thì nhận 422. `RefundBps` đúng tại biên: hủy trước đúng 48 giờ được 100%, trước 47 giờ 59 phút được 50%. Test ở Task 2.
4. **Tìm không dấu:** `q=vung tau` tìm ra "Vũng Tàu". `q` chứa `%`, `_` hoặc `\` được hiểu là ký tự thường. Test ở Task 5.
5. **Sửa Listing đã duyệt:** Listing vẫn hiển thị, `listing_audit_log` có đủ giá trị cũ và mới. Listing bị từ chối thì sửa xong gửi duyệt lại được. Listing `hidden_by_admin` thì đối tác không tự publish lại được. Test ở Task 4.

---

## Cấu trúc file

```
internal/platform/text/slug.go            # Slug dùng chung (chuyển từ geo)
internal/listing/queries.sql
internal/listing/store/                   # sqlc sinh ra
internal/listing/deps.go                  # interface hẹp: PartnerChecker, FileChecker, LocationValidator, ServiceTypeChecker
internal/listing/errors.go
internal/listing/listing.go               # tạo, đọc, sửa, danh sách của đối tác, ghi listing_audit_log
internal/listing/images.go
internal/listing/cancellation.go          # chính sách hủy + RefundBps
internal/listing/attraction.go            # loại vé, khung giờ, ngày nghỉ, SlotsForDate
internal/listing/review.go                # gửi duyệt, duyệt, từ chối, ẩn, bỏ ẩn, publish, unpublish
internal/listing/public.go                # tìm kiếm, chi tiết, BookableListing
internal/listing/http_partner.go, http_admin.go, http_public.go
internal/listing/helpers_test.go
internal/platform/db/migrations/00010_listings.sql
internal/platform/db/migrations/00011_attractions.sql
```

---

### Task 1: Lõi Listing — schema, tạo, đọc, sửa, nhật ký sửa

**Files:**
- Create:
  - `internal/platform/text/slug.go` và test
  - `internal/platform/db/migrations/00010_listings.sql`
  - `internal/listing/{queries.sql,deps.go,errors.go,listing.go,http_partner.go,helpers_test.go,listing_test.go,http_partner_test.go}`
- Modify:
  - `internal/geo/seed.go` (dùng `text.Slug`, kết quả phải giống hệt như cũ)
  - `sqlc.yaml`
  - `cmd/api/routes.go`, `cmd/api/routes_test.go`

**Schema** `00010_listings.sql`:
- `CREATE EXTENSION IF NOT EXISTS pg_trgm; CREATE EXTENSION IF NOT EXISTS unaccent;`
- Hàm wrapper `CREATE FUNCTION f_unaccent(text) RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT AS $$ SELECT public.unaccent('public.unaccent', $1) $$;`
- `listings`: đúng các cột ở DB §5.3, cùng các ràng buộc sau:
  - `UNIQUE (id, partner_id)`
  - FK `partner_id → partners`, FK `service_type → service_types`, FK `cover_file_id → files`
  - FK `province_code → provinces`, FK kép `(ward_code, province_code) → wards(code, province_code)`, `CHECK ((province_code IS NULL) = (ward_code IS NULL))`
  - `review_status CHECK`, mặc định `draft`. `visibility CHECK`, mặc định `published`.
  - `CHECK (review_status <> 'rejected' OR rejection_reason IS NOT NULL)`, `CHECK (visibility <> 'hidden_by_admin' OR hidden_reason IS NOT NULL)`
  - `latitude numeric(9,6) CHECK (latitude BETWEEN -90 AND 90)`, `longitude` tương tự với khoảng −180..180, cả hai cùng có hoặc cùng không
  - `rating_sum`, `rating_count int NOT NULL DEFAULT 0 CHECK (>= 0)`
- Index:
  - `(partner_id, created_at DESC)`
  - `(service_type, province_code) WHERE review_status = 'approved' AND visibility = 'published'`
  - GIN `(f_unaccent(lower(title)) gin_trgm_ops)`
- `listing_audit_log`: `id`, `listing_id` FK, `actor_user_id` FK users, `action text NOT NULL`, `changes jsonb NOT NULL`, `created_at`, `updated_at`. Index `(listing_id, created_at DESC)`.

**Slug** **[Builder]**: lúc tạo Listing, `slug = text.Slug(title) + "-" + 6 ký tự ngẫu nhiên [a-z0-9]`. Slug **không đổi** khi sửa tên, để URL ổn định. Nếu `text.Slug(title)` rỗng thì dùng chữ `listing`.

**Interfaces:**
- Consumes:
  - `partner.IDFrom`, `(*partner.Service).RequireMember()`
  - `partner.Status(ctx, tx, id, lock bool)`, `partner.IsServiceTypeEnabled(ctx, tx, id, code)`
  - `catalog.ServiceType(ctx, tx, code)`, `geo.ValidateLocation(ctx, tx, p, w)`
  - `audit.Record`, `db.InTx`
- Produces:
  - `text.Slug(s string) string`
  - `listing.Deps{Pool; Clock; Partners PartnerChecker; Catalog ServiceTypeChecker; Geo LocationValidator; Files FileChecker}`. Các interface hẹp nằm trong `deps.go`. `PartnerChecker` gồm `Status` và `IsServiceTypeEnabled`.
  - `listing.NewService(d Deps) *Service`
  - `listing.Listing{ID, PartnerID uuid.UUID; ServiceType, Title, Slug string; Summary, Description, Address, ProvinceCode, WardCode *string; Latitude, Longitude *float64; CoverFileID *uuid.UUID; ReviewStatus, Visibility string; RejectionReason, HiddenReason *string; SubmittedAt, ReviewedAt *time.Time; RatingSum, RatingCount int; CreatedAt, UpdatedAt time.Time}`
  - `listing.CreateInput{ServiceType, Title string; Summary, Description, Address, ProvinceCode, WardCode *string; Latitude, Longitude *float64}` và `listing.UpdateInput`: cùng các trường trừ `ServiceType`, tất cả đều là con trỏ. Trường nil thì giữ nguyên, chuỗi rỗng thì xóa giá trị (trừ `Title`, vốn bắt buộc).
  - Ràng buộc độ dài: `Title` 1..200, `Summary` ≤ 500, `Description` ≤ 20000, `Address` ≤ 300, đếm theo rune sau khi trim. Sai thì 422 `invalid_listing` với message nêu tên trường.
  - `func (s *Service) Create(ctx context.Context, partnerID, actor uuid.UUID, in CreateInput) (Listing, error)`
    - Thứ tự trong transaction: `Status(lock=true)` (đang suspended thì `ErrPartnerSuspended` 403 `partner_suspended`) → `catalog.ServiceType` → `IsServiceTypeEnabled` (tắt thì 422 `service_type_not_enabled`) → kiểm tra địa điểm → insert → `listing_audit_log` (`action = "create"`).
  - `func (s *Service) Get(ctx context.Context, partnerID, id uuid.UUID) (Listing, error)`: không có, hoặc thuộc đối tác khác, thì 404 `ErrListingNotFound`.
  - `func (s *Service) ListOwn(ctx context.Context, partnerID uuid.UUID, f OwnFilter) ([]Listing, *uuid.UUID, error)`: `OwnFilter{ReviewStatus *string; Limit int (mặc định 20, tối đa 100); Cursor *uuid.UUID}`, sắp theo id giảm dần.
  - `func (s *Service) Update(ctx context.Context, partnerID, actor, id uuid.UUID, in UpdateInput) (Listing, error)`: `Status(lock=true)` (suspended thì 403) → khóa listing bằng `FOR NO KEY UPDATE` và kiểm tra chủ sở hữu (sai thì 404) → kiểm tra → update → ghi `listing_audit_log` (`action = "update"`) chỉ với những trường thật sự thay đổi. Không có gì đổi thì không ghi.
  - Hàm nội bộ `(s *Service) lockOwned(ctx, tx, partnerID, id) (store.Listing, error)` để Task 2–4 dùng lại.
  - Routes đối tác (`Authenticate → RequireRole(partner) → RequireMember`): `POST /v1/partner/listings` (201), `GET /v1/partner/listings`, `GET /v1/partner/listings/{id}`, `PATCH /v1/partner/listings/{id}`.

- [ ] **Step 1: Chuyển `Slug` sang `internal/platform/text`.** `geo` gọi lại hàm mới. Test của geo, gồm `TestGen_Deterministic`, phải vẫn xanh mà không đổi seed.
- [ ] **Step 2: Viết migration, queries, deps, thêm mục sqlc, chạy `make sqlc`.**
- [ ] **Step 3: Viết test fail.**
  - `listing_test.go`:
    - `TestCreate_DraftWithSlug`: trạng thái là `draft`/`published`, slug khớp regex `^[a-z0-9-]+-[a-z0-9]{6}$`, có 1 dòng audit `create`.
    - `TestCreate_RejectsSuspendedPartner`: 403 `partner_suspended`, không có dòng nào được tạo.
    - `TestCreate_RejectsDisabledServiceType`: 422 `service_type_not_enabled`.
    - `TestCreate_RejectsUnknownServiceType`: 422 `invalid_service_type`.
    - `TestCreate_LocationMustMatch`: 422 `invalid_location`.
    - `TestCreate_Validation`: tên rỗng hoặc 201 rune cho 422 `invalid_listing`.
    - `TestUpdate_LogsOnlyChangedFields`: đổi tên, gửi lại cùng `summary` cũ, kỳ vọng audit `changes` chỉ có `title` với old/new đúng. Update không có gì đổi thì không thêm dòng audit.
    - `TestUpdate_SlugStable`.
    - `TestUpdate_OtherPartner404`.
    - `TestUpdate_SuspendedPartner403`.
    - `TestListOwn_OnlyOwnAndPaginates`.
  - `http_partner_test.go`:
    - `TestHTTP_PartnerListingCRUD`: đi qua mux thật.
    - `TestHTTP_CannotReadOtherPartnersListing404`.
    - `TestHTTP_MalformedID404`.
    - `TestHTTP_UnknownFieldRejected`: body có `partner_id` cho 400.
- [ ] **Step 4: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 5: Cài đặt.** Nối dây trong `buildMux` và thêm 4 route vào `partnerRoutes` của `routes_test.go`.
- [ ] **Step 6: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 7: Commit** `feat(listing): listing core with partner CRUD and edit log`.

---

### Task 2: Ảnh và chính sách hủy

**Files:**
- Modify: `internal/platform/db/migrations/00010_listings.sql` (thêm bảng), `internal/listing/queries.sql`, `internal/listing/http_partner.go`, `cmd/api/routes_test.go`
- Create: `internal/listing/images.go`, `internal/listing/cancellation.go`, cùng test

**Schema** (thêm vào `00010`):
- `listing_images`: `id`, `listing_id` FK, `file_id` FK files, `sort_order int NOT NULL CHECK (sort_order >= 0)`, `UNIQUE(listing_id, sort_order)`, `UNIQUE(listing_id, file_id)`.
- `listing_cancellation_tiers`: `id`, `listing_id` FK, `hours_before int NOT NULL CHECK (hours_before BETWEEN 0 AND 8760)`, `refund_bps int NOT NULL CHECK (refund_bps BETWEEN 0 AND 10000)`, `UNIQUE(listing_id, hours_before)`.

**Interfaces:**
- Produces:
  - `func (s *Service) SetImages(ctx context.Context, partnerID, actor, id uuid.UUID, fileIDs []uuid.UUID, coverFileID *uuid.UUID) (Listing, error)`
    - Thay **toàn bộ** danh sách ảnh theo đúng thứ tự gửi lên (`sort_order` = vị trí trong danh sách).
    - Tối đa 20 ảnh, không được trùng.
    - Mỗi file qua `Files.Owned(..., actor, "listing_image")`.
    - `coverFileID` phải nằm trong danh sách. Nil thì lấy ảnh đầu tiên, danh sách rỗng thì cover là NULL.
    - Ghi `listing_audit_log` (`action = "images"`) với old/new là danh sách id.
  - `listing.Image{FileID uuid.UUID; SortOrder int}` và `func (s *Service) Images(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) ([]Image, error)`.
  - `listing.CancellationTier{HoursBefore int; RefundBps int}`
  - `func ValidateTiers(t []CancellationTier) ([]CancellationTier, error)`:
    - Tối đa 10 mốc, `hours_before` trong khoảng 0..8760 và không trùng, `refund_bps` trong khoảng 0..10000.
    - Sau khi sắp `hours_before` giảm dần, `refund_bps` phải **không tăng** (càng sát giờ thì hoàn càng ít hoặc bằng).
    - Sai thì 422 `invalid_cancellation_policy` với message nêu lỗi cụ thể.
    - Trả về danh sách đã sắp `hours_before` giảm dần.
  - `func RefundBps(tiers []CancellationTier, untilStart time.Duration) int`:
    - Hàm thuần. Chọn mốc có `hours_before` lớn nhất mà `untilStart >= hours_before × 1 giờ`.
    - Không có mốc nào khớp, hoặc `untilStart < 0`, thì trả 0.
    - Plan 4 dùng hàm này với bản chụp chính sách đã lưu trên booking.
  - `func (s *Service) SetCancellationPolicy(ctx, partnerID, actor, id uuid.UUID, tiers []CancellationTier) ([]CancellationTier, error)`: thay toàn bộ các mốc, ghi audit `cancellation_policy`. Danh sách rỗng là hợp lệ, nghĩa là không hoàn tiền.
  - `func (s *Service) CancellationPolicy(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) ([]CancellationTier, error)`: sắp `hours_before` giảm dần.
  - Cả hai thao tác ghi đều đi qua `Status(lock=true)` (suspended thì 403) và `lockOwned` (sai chủ thì 404).
  - Routes đối tác:
    - `PUT /v1/partner/listings/{id}/images`, body `{file_ids:[...], cover_file_id?}`, trả 200 `{images:[...], cover_file_id}`
    - `GET /v1/partner/listings/{id}/cancellation-policy`
    - `PUT /v1/partner/listings/{id}/cancellation-policy`, body `{tiers:[{hours_before, refund_bps}]}`

- [ ] **Step 1: Viết test fail.**
  - `cancellation_test.go`, dạng bảng:
    - `TestValidateTiers`: hợp lệ với `[{48,10000},{24,5000}]`. Trùng mốc 422. % tăng dần (`[{48,5000},{24,10000}]`) 422. 11 mốc 422. `refund_bps` 10001 cho 422.
    - `TestRefundBps_Boundaries`: với `[{48,10000},{24,5000}]`, `untilStart` 48h cho 10000, 47h59m cho 5000, 24h cho 5000, 23h59m cho 0, −1m cho 0. Danh sách rỗng cho 0.
    - `TestSetCancellationPolicy_ReplacesAndAudits`.
  - `images_test.go`:
    - `TestSetImages_OrderAndCover`: cover mặc định là ảnh đầu tiên.
    - `TestSetImages_CoverMustBeInList`: 422.
    - `TestSetImages_FileOfOtherUser422`.
    - `TestSetImages_WrongPurpose422`: dùng file `business_license`.
    - `TestSetImages_Max20`.
    - `TestSetImages_ReplaceShrinks`.
  - HTTP: `TestHTTP_ImagesAndPolicyOtherPartner404`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(listing): images and cancellation policy`.

---

### Task 3: Chi tiết khu du lịch — loại vé, khung giờ, ngày nghỉ

**Files:**
- Create: `internal/platform/db/migrations/00011_attractions.sql`, `internal/listing/attraction.go`, `internal/listing/attraction_test.go`
- Modify: `internal/listing/queries.sql`, `internal/listing/http_partner.go`, `cmd/api/routes_test.go`

**Schema** (DB §5.4):
- `attractions`: `listing_id uuid PRIMARY KEY REFERENCES listings`, `opening_note text NULL`, `checkin_instructions text NULL`, `created_at`, `updated_at`.
- `attraction_ticket_types`: `id`, `listing_id` FK attractions, `name text NOT NULL`, `description text NULL`, `audience text CHECK IN ('child','adult')`, `price bigint NOT NULL CHECK (price > 0)`, `active bool NOT NULL DEFAULT true`, `sort_order int NOT NULL DEFAULT 0`, `UNIQUE(id, listing_id)`.
- `attraction_time_slots`: `id`, `listing_id` FK, `start_time time NOT NULL`, `end_time time NOT NULL CHECK (end_time > start_time)`, `weekdays smallint NOT NULL CHECK (weekdays BETWEEN 1 AND 127)` (bit 0 = thứ Hai … bit 6 = Chủ nhật), `active bool NOT NULL DEFAULT true`, `UNIQUE(id, listing_id)`.
- `attraction_closed_dates`: PK `(listing_id, date)`, `reason text NULL`, `created_at`, `updated_at`.
- Mọi bảng con đều có `CHECK` hoặc trigger đảm bảo Listing có `service_type = 'attraction_ticket'`. **[Builder]** Cách làm: tạo một hàm kiểm tra rồi dùng `CHECK` qua hàm đó. Đơn giản hơn là kiểm tra ở app, kèm một test DB chèn trực tiếp để chứng minh app chặn được. Implementer chọn cách nào thì ghi lại trong báo cáo.

**Interfaces:**
- Produces (mọi thao tác ghi đi qua `Status(lock=true)` và `lockOwned`, Listing phải là loại `attraction_ticket`, nếu không thì 422 `wrong_service_type`, và đều ghi `listing_audit_log`):
  - `func (s *Service) SetAttractionInfo(ctx, partnerID, actor, id uuid.UUID, openingNote, checkinInstructions *string) (AttractionInfo, error)`: upsert dòng `attractions`. Mỗi trường tối đa 2000 rune.
  - `listing.TicketType{ID uuid.UUID; Name string; Description *string; Audience string; Price int64; Active bool; SortOrder int}`
  - `func (s *Service) CreateTicketType(ctx, partnerID, actor, listingID uuid.UUID, in TicketTypeInput) (TicketType, error)` và `UpdateTicketType(ctx, partnerID, actor, listingID, ticketTypeID uuid.UUID, in TicketTypePatch) (TicketType, error)`:
    - `Name` 1..100 rune, `Price` từ 1.000 đến 100.000.000 VND, `Audience` là `child` hoặc `adult`.
    - Tạo loại vé tự tạo dòng `attractions` nếu chưa có.
    - Không có xóa. Muốn bỏ thì patch `active = false`.
  - `listing.TimeSlot{ID uuid.UUID; StartTime, EndTime string ("HH:MM"); Weekdays int; Active bool}`
  - `func (s *Service) CreateTimeSlot(...)` và `UpdateTimeSlot(...)`: giờ theo dạng `HH:MM`, `end > start`, `weekdays` trong khoảng 1..127. **[Builder]** Không cho hai khung **đang active** trùng giờ (chồng lấn) mà có chung ngày trong tuần, sai thì 422 `overlapping_time_slot`.
  - `func (s *Service) AddClosedDate(ctx, partnerID, actor, listingID uuid.UUID, date time.Time, reason *string) error`: trùng thì 409, ngày trong quá khứ (theo `clock.VN`) thì 422. `RemoveClosedDate(...)`: không có thì 404.
  - `func (s *Service) AttractionDetail(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, includeInactive bool) (AttractionDetail, error)`: gồm info, loại vé (sắp theo `sort_order` rồi `name`), khung giờ (sắp theo `start_time`) và các ngày nghỉ từ hôm nay trở đi.
  - **Cho plan 4:**
    - `func (s *Service) SlotsForDate(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, date time.Time) ([]TimeSlot, error)`: trả các khung đang active mà có bit ngày-trong-tuần của `date` (theo `clock.VN`). Nếu `date` là ngày nghỉ thì trả danh sách rỗng.
    - `func (s *Service) TicketTypesForBooking(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]TicketType, error)`: chỉ lấy loại vé **active** và thuộc đúng Listing. Thiếu loại nào thì 422 `invalid_ticket_type`.
  - Routes đối tác:
    - `PUT /v1/partner/listings/{id}/attraction`
    - `GET /v1/partner/listings/{id}/attraction`
    - `POST /v1/partner/listings/{id}/ticket-types`, `PATCH /v1/partner/listings/{id}/ticket-types/{ticketTypeID}`
    - `POST /v1/partner/listings/{id}/time-slots`, `PATCH /v1/partner/listings/{id}/time-slots/{slotID}`
    - `PUT /v1/partner/listings/{id}/closed-dates/{date}` (body `{reason?}`), `DELETE /v1/partner/listings/{id}/closed-dates/{date}`

- [ ] **Step 1: Viết migration `00011` và queries, chạy `make sqlc`.**
- [ ] **Step 2: Viết test fail** `attraction_test.go`:
  - `TestTicketType_CreateUpdateDeactivate`.
  - `TestTicketType_Validation`: giá 999 cho 422, tên rỗng cho 422.
  - `TestTicketType_WrongServiceType422`: tạo trên Listing `self_drive_rental`.
  - `TestTimeSlot_Overlap422`: 08:00–10:00 (T2–T6) và 09:00–11:00 (T2) cho 422. 09:00–11:00 chỉ T7 thì được.
  - `TestTimeSlot_InactiveDoesNotBlockOverlap`.
  - `TestSlotsForDate_WeekdayAndClosed`: 2026-11-09 là thứ Hai.
  - `TestSlotsForDate_UsesVietnamDate`: truyền `2026-11-08T17:30:00Z` thì được xử lý như thứ Hai 2026-11-09.
  - `TestTicketTypesForBooking_InactiveOrForeign422`.
  - `TestClosedDate_PastRejected`.
  - `TestAttraction_OtherPartner404` (HTTP).
  - Mỗi thao tác ghi đều có 1 dòng `listing_audit_log`.
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt.** Cập nhật `partnerRoutes`.
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(listing): attraction ticket types, time slots, closed dates`.

---

### Task 4: Quy trình duyệt — gửi duyệt, duyệt, từ chối, ẩn, publish

**Files:**
- Create: `internal/listing/review.go`, `internal/listing/http_admin.go`, `internal/listing/review_test.go`
- Modify: `internal/listing/queries.sql`, `internal/listing/http_partner.go`, `cmd/api/routes.go`, `cmd/api/routes_test.go`

**Máy trạng thái:**

| Thao tác | Ai | Điều kiện | Kết quả |
|---|---|---|---|
| `Submit` | đối tác | `review_status` ∈ {`draft`, `rejected`}, đủ điều kiện hoàn chỉnh (bên dưới) | `pending_review`, `submitted_at = now`, xóa `rejection_reason` |
| `Approve` | super_admin | `pending_review` | `approved`, `reviewed_at`/`reviewed_by`, `visibility` giữ `published` |
| `Reject` | super_admin | `pending_review`, `reason` 1..500 rune | `rejected`, `rejection_reason` |
| `Hide` | super_admin | `approved`, `reason` 1..500 rune | `visibility = hidden_by_admin`, `hidden_*` |
| `Unhide` | super_admin | `visibility = hidden_by_admin` | `published`, xóa `hidden_*` |
| `Unpublish` | đối tác | `approved`, `visibility = published` | `unpublished_by_partner` |
| `Publish` | đối tác | `approved`, `visibility = unpublished_by_partner` | `published` (không gỡ được `hidden_by_admin`) |

Sai điều kiện thì 409 `invalid_listing_state`. Admin thao tác trên id không tồn tại thì 404.

**Điều kiện hoàn chỉnh khi gửi duyệt** **[Builder]**: có `title`, `description`, `province_code`/`ward_code`, `address`, ít nhất 1 ảnh. Với `attraction_ticket` thì thêm: có ít nhất 1 loại vé active và ít nhất 1 khung giờ active. Thiếu thì 422 `listing_incomplete`, kèm danh sách trường thiếu trong message. Không bắt buộc có chính sách hủy, vì không có nghĩa là không hoàn tiền.

**Interfaces:**
- Produces:
  - `func (s *Service) Submit(ctx, partnerID, actor, id uuid.UUID) (Listing, error)`: kiểm tra suspended (403) và chủ sở hữu (404). Ghi `listing_audit_log` (`action = "submit"`).
  - `func (s *Service) Publish(...)` và `Unpublish(...)`: phía đối tác, ghi `listing_audit_log`.
  - `func (s *Service) Approve(ctx, actor, id uuid.UUID) (Listing, error)`, `Reject(ctx, actor, id uuid.UUID, reason string)`, `Hide(ctx, actor, id, reason)`, `Unhide(ctx, actor, id)`: khóa listing bằng `FOR NO KEY UPDATE`, ghi `audit.Record` (`listing.approve`, `listing.reject`, `listing.hide`, `listing.unhide`) kèm before/after là trạng thái.
  - `func (s *Service) AdminList(ctx context.Context, f AdminFilter) ([]Listing, *uuid.UUID, error)`: `AdminFilter{ReviewStatus, Visibility, ServiceType *string; PartnerID *uuid.UUID; Limit int; Cursor *uuid.UUID}`. Riêng khi lọc `pending_review` thì sắp theo `submitted_at` tăng dần (cũ nhất trước).
  - `func (s *Service) AdminGet(ctx, id uuid.UUID) (AdminListingDetail, error)`: gồm Listing, ảnh, chính sách hủy, chi tiết theo loại dịch vụ, và 20 dòng `listing_audit_log` gần nhất.
  - Routes đối tác: `POST /v1/partner/listings/{id}/submit`, `.../publish`, `.../unpublish`.
  - Routes admin: `GET /v1/admin/listings`, `GET /v1/admin/listings/{id}`, `POST /v1/admin/listings/{id}/approve`, `.../reject` `{reason}`, `.../hide` `{reason}`, `.../unhide`.

- [ ] **Step 1: Viết test fail** `review_test.go`:
  - `TestSubmit_IncompleteListsMissingFields`: Listing attraction không ảnh, không loại vé, kỳ vọng 422 và message chứa cả hai.
  - `TestFullFlow_SubmitApprove`.
  - `TestReject_ThenEditAndResubmit`.
  - `TestApprovedEditStaysLiveAndIsLogged` (Review Focus 5).
  - `TestHide_PartnerCannotPublish`: 409.
  - `TestUnhide`.
  - `TestUnpublishPublish`.
  - `TestInvalidTransitions409`: approve một bản draft, reject một bản đã approved.
  - `TestAdminActionsAudited`.
  - `TestAdminList_PendingOldestFirst`.
  - `TestSubmit_SuspendedPartner403`.
  - HTTP: `TestHTTP_AdminReviewFlow`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật `adminRoutes` và `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(listing): review workflow and admin moderation`.

---

### Task 5: Tìm kiếm public, trang chi tiết, kiểm tra đặt được

**Files:**
- Create: `internal/listing/public.go`, `internal/listing/http_public.go`, `internal/listing/public_test.go`
- Modify: `internal/listing/queries.sql`, `cmd/api/routes.go`, `cmd/api/routes_test.go` (thêm vào `TestPublicRoutes`)

**Interfaces:**
- Produces:
  - Điều kiện hiển thị dùng chung một mệnh đề SQL: `l.review_status = 'approved' AND l.visibility = 'published' AND p.status = 'active' AND EXISTS (partner_service_types pst đang bật cho đúng loại)`. **[Builder]** Truy vấn public đọc thẳng bảng `partners` và `partner_service_types` để join. Đây là phép join chỉ-đọc xuyên module được chấp nhận, ghi chú trong code giống Ruling 12 của plan 2. Lý do là tìm kiếm cần một câu SQL duy nhất.
  - `listing.SearchFilter{ServiceType, ProvinceCode, Q *string; Limit (mặc định 20, tối đa 50), Offset (tối đa 1000) int}`
    - `Q` được trim, tối đa 100 rune. Khớp nếu `f_unaccent(lower(title)) ILIKE '%' || f_unaccent(lower(q)) || '%'` (đã escape `%`, `_`, `\`), hoặc độ tương đồng trigram ≥ 0,3.
    - Sắp xếp: có `q` thì theo `similarity` giảm dần rồi `id` giảm dần. Không có `q` thì theo `id` giảm dần.
  - `listing.PublicCard{Slug, Title, ServiceType string; Summary *string; ProvinceCode *string; CoverURL *string; RatingAvg *float64 (nil khi count = 0); RatingCount int; PriceFrom *int64}`. `PriceFrom` với attraction là giá thấp nhất trong các loại vé active.
  - `func (s *Service) Search(ctx context.Context, f SearchFilter) ([]PublicCard, error)`
  - `listing.PublicDetail{PublicCard; Description, Address, WardCode *string; Latitude, Longitude *float64; ImageURLs []string; CancellationPolicy []CancellationTier; Attraction *AttractionDetail (chỉ phần active)}`
  - `func (s *Service) PublicDetail(ctx context.Context, slug string) (PublicDetail, error)`: Listing không hiển thị hoặc không tồn tại thì 404. URL ảnh lấy qua `Files` (file public nên là URL cố định). Thêm phương thức cần thiết vào interface `FileChecker`, ví dụ `PublicURL(ctx, tx, id) (string, error)`.
  - **Cho plan 4:** `func (s *Service) BookableListing(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) (Listing, error)`
    - Thứ tự: đọc `partner_id` → `Partners.Status(ctx, tx, partnerID, true)` (`FOR SHARE`) → `SELECT ... FROM listings ... FOR SHARE` → kiểm tra điều kiện hiển thị và `IsServiceTypeEnabled`.
    - Không đạt thì 422 `ErrListingNotBookable` ("Dịch vụ này hiện không nhận đặt chỗ").
    - Ghi trong doc comment: plan 4 phải gọi hàm này trong transaction tạo booking.
  - Routes public: `GET /v1/listings?service_type=&province=&q=&limit=&offset=` và `GET /v1/listings/{slug}`, cùng header `Cache-Control: public, max-age=60`.

- [ ] **Step 1: Viết test fail** `public_test.go`:
  - `TestSearch_OnlyVisible`: tạo 5 Listing ở các trạng thái `draft`, `pending_review`, `approved`, `hidden_by_admin`, `unpublished_by_partner`, kỳ vọng chỉ thấy Listing `approved`.
  - `TestSearch_HidesSuspendedPartnerAndDisabledType` (Review Focus 2).
  - `TestSearch_UnaccentedQuery`: title "Khu du lịch Vũng Tàu Paradise", `q = "vung tau"` thì tìm ra.
  - `TestSearch_WildcardsLiteral`: `q = "%"` không trả về tất cả.
  - `TestSearch_FilterByProvinceAndType`.
  - `TestSearch_PriceFromIsMinActive`.
  - `TestPublicDetail_404WhenNotVisible`.
  - `TestPublicDetail_OnlyActiveTicketTypes`.
  - `TestBookableListing_OK`, `TestBookableListing_SuspendedNotBookable`, `TestBookableListing_HiddenNotBookable`.
  - `TestBookableListing_BlocksConcurrentSuspend`: tx A gọi `BookableListing` rồi giữ transaction. Gọi `Suspend` trong goroutine, kỳ vọng vẫn chưa xong sau 200ms. Commit A, kỳ vọng `Suspend` xong trong vòng 5 giây.
  - HTTP: `TestHTTP_PublicSearchAndDetail`, có kiểm tra header cache.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Thêm route vào `TestPublicRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Kiểm tra tay** trên compose dev của worktree (Postgres 5434, MinIO 9010):
  - Tạo admin bằng `create-admin` với email mới.
  - Đặt commission cho `attraction_ticket`.
  - Tạo đối tác có loại `attraction_ticket`.
  - Đăng nhập bằng owner, upload 1 ảnh `listing_image`.
  - Tạo Listing, đặt ảnh, chính sách hủy, 1 loại vé, 1 khung giờ, rồi gửi duyệt.
  - Admin duyệt.
  - `GET /v1/listings?q=...` thấy Listing, `GET /v1/listings/{slug}` có ảnh và giá.

  Không xóa dữ liệu nào.
- [ ] **Step 6: Commit** `feat(listing): public search, detail and bookable check`.

---

## Không làm trong plan này

- Chi tiết xe tuyến, xe bao chuyến, thuê xe (plan 6–8). Các plan đó thêm bảng con và nhánh điều kiện hoàn chỉnh khi gửi duyệt cho loại của mình.
- Booking (plan 4).
- Review/điểm đánh giá thật (plan 9). Cột `rating_*` đã có nhưng giữ bằng 0.
- Trang tỉnh/điểm đến do LunaIn biên tập. **[Chốt]** Không có.
