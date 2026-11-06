# Plan 8 — Private Charter (xe bao chuyến): báo giá theo km, phụ phí, API bản đồ — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Nhà xe khai báo khu vực phục vụ, các loại xe (giá mở cửa, giá mỗi km) và phụ phí. Khách chọn điểm đón/điểm trả trên bản đồ, xem báo giá của từng loại xe (quãng đường do server hỏi API bản đồ, làm tròn lên 0,1 km), rồi gửi yêu cầu. Đối tác chấp nhận hoặc từ chối, khách trả tiền trong 2 giờ. Tiền về đúng hạn hay trễ (xác nhận lại nếu còn ≥ 3 giờ trước giờ đón), hủy, đón khách bằng QR, "hoàn thành chuyến", No-show và tự kết thúc đều đi qua vòng đời Request Booking và các đường tiền đã có từ plan 4–7. Xe bao chuyến **không giữ sức chứa nào**.

**Architecture:**

- `internal/platform/maps` (mới): interface `Provider` (một hàm `Route`: quãng đường + thời gian chạy giữa hai tọa độ), `Fake` tất định cho test/dev, client `Goong` thật có timeout, lớp `Cached` trong bộ nhớ. Chọn bằng `MAP_PROVIDER` (giống `PAYMENT_PROVIDER`: `fake` bị cấm khi `APP_ENV=prod`). Chưa có key: chạy `fake`.
- `listing` sở hữu `charter_services`, `charter_service_areas`, `charter_vehicle_classes`, `charter_surcharges`. Nó có thêm:
  - API đối tác cho dịch vụ (khu vực, km tối đa, khung giờ đêm), loại xe, phụ phí; điều kiện gửi duyệt cho `private_charter`; chi tiết public.
  - **Cách tính giá thuần** `PriceCharter` (không I/O) và `CharterQuoteTx` (đọc bảng giá + ngày lễ trong `tx` của người gọi). Báo giá public `POST /v1/charter-quotes` trả giá của mọi loại xe đang hoạt động sau **một** lần gọi bản đồ.
  - `CharterRoute`: gọi provider **ngoài** transaction. Deps có thêm `Maps maps.Provider` và `Holidays HolidayChecker` (`*settings.Service`).
- `booking` sở hữu `booking_charter_details`. Nó có thêm:
  - `create_charter.go`: gọi `CharterRoute` trước, rồi một transaction tính lại giá, so với `quoted_total` khách đã xem, tạo đơn `requested` qua `insertRequestedBooking`/`requestResponseDue` của plan 7.
  - `charter_lifecycle.go`: đón khách bằng QR (`confirmed → in_service`), "hoàn thành chuyến", No-show, job `FinishCharters`.
  - Sửa chỗ còn giả định "thuê xe" của plan 7 (liệt kê ở mục "Chỗ phải sửa trong vòng đời chung"). `request.go`, `AcceptRequest`, `RejectRequest`, `withdrawRequest`, `ExpireRequests`, `ExpireHolds`, `Pay`, `cancel` **dùng nguyên**: với charter `lockCheckoutCapacity`, `releaseHold`, `keepHoldUntil`, `holdForService` đều không làm gì.

**Tech Stack:** như plan 1–7 (Go 1.27, PostgreSQL 17, pgx/v5, sqlc, goose, River, `net/http` ServeMux). PayOS và bản đồ chỉ có Fake cho tới khi có key (phụ lục cuối).

**Spec:**
- [spec hệ thống](../specs/2026-11-04-lunain-travel-backend-design.md) §5.7, §5.9, §6.2, §6.3, §7, §8, §10
- [thiết kế DB](../specs/2026-11-04-database-schema.md) §1.1, §1.2, §4 (`platform_settings`, `holidays`), §5.6 (xe bao chuyến), §6 (`bookings`, mốc thời gian theo loại, `booking_items`, `booking_charter_details`, "Hạn của Request Booking")
- [ADR 0003](../../adr/0003-listing-base-plus-type-tables.md), [ADR 0004](../../adr/0004-partner-ledger.md), [GLOSSARY](../../../GLOSSARY.md) (Private Charter, Request Booking, No-show)
- Header plan 4–7. Rulings 1–26 của plan 4 vẫn ràng buộc (đặc biệt 9–17 định tuyến tiền, 21 hủy tới `service_end_at`, 25 xác nhận lại đúng thứ đã bán).

## Global Constraints

- Mọi Global Constraints của plan 1–7 vẫn áp dụng:
  - uuid v7, clock được inject (`clock.VN`), lỗi JSON tiếng Việt.
  - Path id sai định dạng hoặc không tồn tại thì 404. Body sai thì 422. Body có trường lạ thì 400. Query param sai thì 400.
  - Tiền là `bigint` VND, làm tròn nửa lên (`money.MulBps`, thêm `money.MulTenths` ở Task 4). Σ `booking_items.line_total` = `gross_amount`.
  - Test chạy trên DB riêng clone từ template đã migrate, assert đúng số tiền và gọi `ledger.Verify`.
  - Migration trên `main` bất biến: file mới là `00019_charter.sql`, rồi chạy `make migrations-sum`. Mỗi worktree dùng DB riêng (`make db-branch`).
  - Mỗi route `/v1/admin/*`, `/v1/partner/*`, route khách và route public mới phải có trong danh sách tương ứng của `cmd/api/routes_test.go`.
  - Thao tác admin gọi `audit.Record` trong cùng transaction; thao tác đối tác trên listing ghi `listing_audit_log` (`logChange`).
  - Commit không có trailer `Co-Authored-By` hay dòng ghi công AI nào.
- **Thứ tự khóa (DB §1.1):** `users → partners → listings → checkouts → bookings → dòng con (booking_charter_details, settlements, refunds) → payments (FOR NO KEY UPDATE) → payouts → partner_balances`.
  - Charter **không có dòng sức chứa**: không khóa gì giữa `listings` và `checkouts`. Bảng giá (`charter_*`) chỉ được đọc khi đã giữ `listings FOR SHARE` (`BookableListing`); đối tác sửa bảng giá giữ `listings FOR NO KEY UPDATE`, nên hai bên xếp hàng.
  - Transaction chỉ khóa booking (QR, hoàn thành, No-show, `FinishCharters`) khóa thẳng `bookings` rồi kiểm tra lại trạng thái.
  - **Không bao giờ gọi provider (bản đồ hay PayOS) khi đang mở transaction.**
- **Công thức giá [Chốt] (spec §5.7):** `giá = base_fare + distance_km × price_per_km + Σ phụ phí áp dụng`.
  - `distance_km` = quãng đường **một chiều** từ API bản đồ, **làm tròn lên 0,1 km**: `tenths = ceil(distance_m / 100)` (30.200 m → 30,2; 30.201 m → 30,3). Lưu ở `booking_charter_details.distance_km numeric(7,1)`.
  - Khứ hồi: quãng đường tính tiền = `2 × distance_km` (một dòng `charter_distance` có `quantity = 2 × distance_km`), cộng phụ phí `round_trip_waiting`.
  - `distance_total = round_half_up(price_per_km × billed_km)` = `money.MulTenths(price_per_km, billed_tenths)`.
  - **Cước** `fare = base_fare + distance_total`. Phụ phí `percent` = `money.MulBps(fare, amount_bps)` (không tính trên phụ phí khác, mặc định C3).
- **Phụ phí (spec §5.7, DB §5.6):** `kind ∈ {night, weekend, holiday, round_trip_waiting}`, `calc ∈ {fixed, percent}`, `amount bigint ≥ 0` (VND nếu `fixed`, bps nếu `percent`, `percent` ≤ 10000), `UNIQUE(vehicle_class_id, kind)`.
  - `night` và `round_trip_waiting` chỉ được `fixed` (CHECK). `weekend`/`holiday` được `fixed` hoặc `percent`.
  - **Mặc định 0đ [Chốt]:** không có dòng, dòng `active = false` hoặc `amount = 0` thì không áp và không sinh dòng `booking_items`.
  - `night`: giờ đón (giờ VN) nằm trong `[night_start, night_end)` của `charter_services`; khung qua nửa đêm khi `night_start > night_end` (22:00–05:00: 22:00 tính, 04:59 tính, 05:00 không). Không có khung đêm thì không áp. Chỉ xét giờ đón, tính một lần kể cả khứ hồi (mặc định C2).
  - `weekend`: ngày đón (giờ VN) là thứ Bảy hoặc Chủ nhật. `holiday`: ngày đón (giờ VN) có trong `holidays` (`settings.IsHoliday`).
  - **Cuối tuần trùng ngày lễ:** không cộng dồn, chỉ áp khoản **lớn hơn** (**quyết định C1, đã chốt**). Bằng nhau thì ghi `holiday`.
  - `round_trip_waiting`: cộng cố định khi `is_round_trip`.
- **Dòng `booking_items` (DB §6):** theo thứ tự `charter_base` (`unit_price = base_fare`, `quantity = 1`, bỏ nếu 0), `charter_distance` (`unit_price = price_per_km`, `quantity = billed_km` một chữ số thập phân; luôn có vì `price_per_km > 0`), rồi mỗi phụ phí > 0 một dòng `surcharge` (`unit_price` = số tiền đã tính, `quantity = 1`) theo thứ tự `night, weekend|holiday, round_trip_waiting`. CHECK `line_total = round(unit_price × quantity)` của `00012` phải đúng.
- **Tọa độ và địa chỉ (mặc định C5):** FE dùng bản đồ chọn điểm, gửi `lat`, `lng`, `address` cho điểm đón và điểm trả, cộng `province_code` của điểm đón. Server không geocode. Tọa độ phải trong khung Việt Nam `lat ∈ [8.0, 23.5]`, `lng ∈ [102.0, 110.0]`, làm tròn 6 chữ số khi lưu (`numeric(9,6)`). Địa chỉ 1..500 rune.
- **Khu vực và km tối đa (quyết định C4, đã chốt):** tỉnh điểm đón phải thuộc `charter_service_areas`, nếu không thì 422 `outside_service_area`. Điểm trả không giới hạn tỉnh. `max_distance_km` (NULL = không giới hạn) so với quãng đường **một chiều**, vượt thì 422 `distance_exceeds_max`.
- **Báo giá không lưu (mặc định C6):** mỗi lần gửi yêu cầu, server gọi lại bản đồ (qua cache) và tính lại giá. Body có `quoted_total` (bắt buộc, > 0); lệch với giá vừa tính thì 409 `quote_changed`, không tạo gì. Đơn đã tạo giữ snapshot (`booking_items` + `quote jsonb`); đối tác sửa giá sau đó không ảnh hưởng.
- **Thời gian (quyết định C7 đã chốt; C8 là mặc định):**
  - `pickup_at`, `return_at` tròn phút; `pickup_at ≤ now + 180 ngày`; `pickup_at ≥ now + request_payment_hours + 1h` (cùng luật plan 7), nếu không thì 422 `pickup_too_soon`.
  - Một chiều: `return_at` phải vắng. `service_end_at = pickup_at + ceil(duration_s / 60) phút` (DB §6 bảng mốc thời gian).
  - Khứ hồi: `return_at ≥ pickup_at + ceil(duration_s / 60) phút` và `≤ pickup_at + 7 ngày`. `service_end_at = return_at` (DB §6).
  - `service_start_at = original_service_start_at = pickup_at`.
- **Request Booking (spec §6.2, DB §6), settings đã có, không thêm setting mới:** `request_response_hours` 12, `request_payment_hours` 2, `hold_grace_minutes` 5, `charter_late_confirm_min_hours` **3**, `charter_auto_complete_hours` **24**, `release_delay_days` 1.
  - `response_due_at = min(now + 12h, pickup_at − 2h)`. Chấp nhận: `payment_due_at = hold_expires_at = accepted_at + 2h`, `hold_release_at = + 5 phút`. Rút miễn phí ở `requested`/`awaiting_payment`. Đã chấp nhận thì đối tác không rút lại được.
  - **Tiền về trễ (spec §8):** đơn trễ (`expired`, hoặc `awaiting_payment` quá `hold_release_at`) được xác nhận lại khi: đã được chấp nhận (`accepted_at` có), listing còn bán được (`BookableListing`), và `pickup_at − now ≥ charter_late_confirm_min_hours` (đúng 3h00 thì xác nhận, 2h59 thì hoàn). Không thỏa: `expired`, hoàn `late_payment` 100% `awaiting_bank_info`, không bút toán.
- **Sau thanh toán (mặc định C10):**
  - Đón khách: QR chuyển `confirmed → in_service` trong `[pickup_at − 2h, service_end_at + charter_auto_complete_hours)`, ghi `in_service_at = checked_in_at = now`.
  - "Hoàn thành chuyến": `in_service → completed` bất kỳ lúc nào; đã `completed` thì trả 200 không đổi.
  - No-show: `confirmed` và `now ≥ pickup_at`.
  - Job `FinishCharters` (mỗi phút): `in_service` có `service_end_at + charter_auto_complete_hours ≤ now` → `completed`; `confirmed` cùng mốc → `no_show`. Actor system.
  - Mọi đường kết thúc: `scheduleRelease(now)` (release `release_delay_days` sau). Không hoàn tiền khi No-show.
- **Hủy (spec §6.3):** `cancelBlock` có sẵn: khách/Super Admin hủy đơn `confirmed` tới `service_end_at`, % hoàn theo `original_service_start_at = pickup_at`. Force majeure hoàn 100% và miễn hoa hồng. `in_service`/`completed`/`no_show` không hủy được. `can_reschedule = false`.
- **API bản đồ:** gọi với timeout **5 giây**, không retry trong request. Lỗi mạng, HTTP 5xx hoặc timeout → 503 `map_unavailable`. Không có đường (status phần tử khác `OK`, hoặc quãng đường 0) → 422 `no_route`. API key **không bao giờ** xuất hiện trong log hay chuỗi lỗi. Cache trong bộ nhớ theo cặp tọa độ làm tròn 5 chữ số, TTL **6 giờ**, tối đa **10.000** mục, không cache lỗi.
- **Một yêu cầu = một loại xe, một xe** (mặc định C9).
- **Giá loại xe (ruling R1 của controller):** `price_per_km > 0` (1..1.000.000), `base_fare ≥ 0` (0..50.000.000), nên báo giá không bao giờ là 0đ. `price_from` của charter trong tìm kiếm/chi tiết public = `min(base_fare + price_per_km)` của các loại xe active (giá cho 1 km đầu, luôn > 0, dùng được sentinel 0 = NULL như các loại khác).
- **Tiền về trễ khi đối tác bị đình chỉ hoặc listing bị ẩn (ruling R2, spec §8 thắng):** đơn trễ không được xác nhận lại mà hoàn `late_payment` 100%, không bút toán (giống thuê xe). Đối tác bị đình chỉ vẫn chấp nhận/từ chối được yêu cầu đang chờ (DB §6), nhưng tiền về **đúng hạn** vẫn xác nhận bình thường.
- **Provider bản đồ (quyết định C11, đã chốt):** Goong là client thật duy nhất; `MAP_PROVIDER ∈ {fake, goong}`.
- **Thông báo:** chỉ email qua outbox (`mailer.EnqueueTx`), email đối tác qua `emailPartner`.

## Review Focus

1. **Giá đổi giữa lúc xem và lúc gửi:** khách xem báo giá 563.600 (Xe 7 chỗ, một chiều, thứ Tư 11/11 08:00), rồi đối tác tăng `price_per_km` 12.000 → 13.000 (hoặc admin thêm 11/11 vào ngày lễ). Gửi yêu cầu với `quoted_total = 563.600` → 409 `quote_changed`, không có checkout/booking/idempotency response nào. Gửi lại với giá mới → 201. Sau khi đã tạo đơn, đối tác sửa giá thì `gross_amount`, `booking_items`, `quote` của đơn **không đổi**, và đơn được chấp nhận/thanh toán theo giá cũ. Test `TestCreateCharter_QuoteChanged409` và `TestCreateCharter_PriceEditAfterRequestKeepsSnapshot` ở Task 5.
2. **Khung giờ đêm qua nửa đêm và múi giờ:** khung 22:00–05:00. Giờ đón 22:00 VN, 04:59 VN, `2026-11-11T15:00:00Z` (= 22:00 VN) đều có phụ phí đêm; 05:00 VN và 21:59 VN thì không. Khung không qua nửa đêm (00:00–05:00) chạy đúng. Không có khung đêm thì không áp dù đã bật phụ phí. Test `TestPriceCharter_NightWindow` và `TestInNightWindow` ở Task 4.
3. **Cuối tuần trùng ngày lễ, ngày theo giờ VN:** thứ Bảy 14/11 là ngày lễ, weekend 10%, holiday 30% → chỉ cộng 169.080 (khoản lớn hơn), không cộng 225.440. Holiday tắt (0) mà weekend bật → cộng weekend. Giờ đón `2026-11-13T17:30:00Z` là 00:30 thứ Bảy theo giờ VN → có weekend **và** night. Test `TestPriceCharter_WeekendHolidayTakeLarger` và `TestCharterQuote_VNDateForWeekendAndHoliday` ở Task 4.
4. **Bản đồ chậm hoặc lỗi:** provider timeout/5xx khi báo giá hoặc gửi yêu cầu → 503 `map_unavailable` trong ≤ 5 giây, không mở transaction, không giữ Idempotency-Key, nên gửi lại cùng key sau khi bản đồ chạy lại → 201 một đơn. Không có đường → 422 `no_route`. Replay cùng key **không** gọi bản đồ. Lỗi không bị cache. Test `TestGoong_TimeoutIsUnavailable` (Task 2) và `TestCreateCharter_MapDown503ThenRetry`, `TestCreateCharter_ReplayDoesNotCallMap` (Task 5).
5. **Tiền về trễ đúng 3 giờ trước giờ đón:** đơn đón 10/11 15:10, chấp nhận 09:30, hết hạn trả 11:30, nhả 11:35. Tiền về 12:10 (còn đúng 3h00) → `confirmed` lại, có bút toán. Tiền về 12:11 (còn 2h59) → vẫn `expired`, hoàn `late_payment` 563.600 `awaiting_bank_info`, không `ledger_entries`. Đơn hết hạn khi đối tác **chưa** chấp nhận thì không bao giờ được xác nhận lại. Test `TestCharterLatePaid_ThreeHourBoundary` ở Task 6.

---

## Chỗ phải sửa trong vòng đời chung của plan 7

Đọc `request.go`, `capacity.go`, `confirm.go`, `checkin.go`, `rental_lifecycle.go`, `jobs.go`. Phần chung chạy đúng cho charter, trừ các chỗ sau:

1. `reconfirmChecks`/`HandlePaid`: đơn charter trễ hiện chỉ cần listing bán được, **không** kiểm `accepted_at` và mốc 3 giờ → thêm `charterChecks` (Task 6).
2. `checkIn`: `switch b.ServiceType` không có nhánh charter → rơi xuống `ErrCheckInNotAllowed` → thêm `checkInCharter` (Task 7).
3. `endRental` thực ra là chung (`releaseHold` + `scheduleRelease`) → đổi tên `endService` và dùng cho cả hai (Task 7).
4. Route `POST /v1/partner/bookings/{id}/no-show` gọi `MarkRentalNoShow` (lỗi `errNotRental` cho charter) → handler gọi `MarkNoShow` rẽ nhánh theo loại (Task 7).
5. `FinishRentals`/`ListRentalAuto*` lọc `self_drive_rental` và dùng hằng 24h → job riêng `FinishCharters` đọc `charter_auto_complete_hours` (Task 7).
6. `listing.CheckRentalPeriod` gói cả luật "đủ thời gian phản hồi + trả tiền" → charter có `CheckCharterTime` riêng; `ErrPickupTooSoon` có chữ "nhận xe" → thêm `ErrCharterPickupTooSoon` cùng code `pickup_too_soon` (Task 4).
7. `insertItem` chỉ nhận `quantity` nguyên → thêm `insertItemTenths` và query `InsertBookingItemTenths` (Task 5).
8. `views.go` chưa có nhánh charter (Task 5). `MarkNoShows`, `CompleteRides`, `rentalChecks`, `ExpireHolds` đã lọc theo loại hoặc chung, không cần sửa.

---

## Cấu trúc file

```
internal/platform/db/migrations/00019_charter.sql        # charter_services, charter_service_areas, charter_vehicle_classes, charter_surcharges, booking_charter_details
internal/platform/maps/{maps.go,fake.go,goong.go,cache.go,*_test.go}
internal/platform/config/{config.go,maps_test.go}
internal/platform/money/{money.go,money_test.go}         # MulTenths
internal/listing/{charter.go,charter_classes.go,charter_quote.go,http_charter.go,deps.go,errors.go,review.go,public.go,http_public.go,queries.sql}
internal/listing/{charter_test.go,charter_classes_test.go,charter_quote_test.go}
internal/booking/{create_charter.go,charter_lifecycle.go,capacity.go,confirm.go,checkin.go,rental_lifecycle.go,views.go,jobs.go,http_customer.go,http_partner.go,queries.sql}
internal/booking/{charter_helpers_test.go,create_charter_test.go,confirm_charter_test.go,charter_lifecycle_test.go,schema_test.go}
cmd/api/{main.go,routes.go,routes_test.go,e2e_charter_test.go}
docs/superpowers/specs/2026-11-04-database-schema.md (§1.1, §5.6, §6), GLOSSARY.md, docs/superpowers/plans/README.md
```

---

### Task 1: Schema xe bao chuyến

**Files:**
- Create: `internal/platform/db/migrations/00019_charter.sql` (cập nhật `CHECKSUMS` bằng `make migrations-sum`)
- Modify: `internal/listing/queries.sql`, `internal/booking/queries.sql` (mỗi bên tối thiểu một query chạm bảng mới để sqlc sinh model), `internal/booking/schema_test.go`

**Schema** (đúng DB §5.6 và §6, kèm phần bổ sung):

- **`charter_services`:** `listing_id` PK FK `listings`, `max_distance_km numeric(7,1) NULL CHECK (max_distance_km > 0)`, `night_start time NULL`, `night_end time NULL`, `CHECK ((night_start IS NULL) = (night_end IS NULL))`, `CHECK (night_start <> night_end)`.
- **`charter_service_areas`:** PK `(listing_id, province_code)`, `listing_id` FK `charter_services(listing_id)`, `province_code` FK `provinces`.
- **`charter_vehicle_classes`:** `id`, `listing_id` FK `charter_services(listing_id)`, `name` (`btrim <> ''`), `seats int CHECK BETWEEN 1 AND 50`, `base_fare bigint CHECK >= 0`, `price_per_km bigint CHECK > 0` (ruling R1), `active bool NOT NULL DEFAULT true`, **`UNIQUE(id, listing_id)`** (DB §1.2), index `(listing_id)`.
- **`charter_surcharges`:** `id`, `vehicle_class_id` FK, `kind CHECK IN ('night','weekend','holiday','round_trip_waiting')`, `calc CHECK IN ('fixed','percent')`, `amount bigint CHECK >= 0`, `active bool NOT NULL DEFAULT true`, `UNIQUE(vehicle_class_id, kind)`. **Bổ sung:** `CHECK (kind IN ('weekend','holiday') OR calc = 'fixed')`, `CHECK (calc <> 'percent' OR amount <= 10000)`.
- **`booking_charter_details`:**
  - `booking_id` PK, `listing_id`, `vehicle_class_id`, `pickup_at timestamptz`, `pickup_address text`, `pickup_lat/pickup_lng numeric(9,6)`, `dropoff_address`, `dropoff_lat/dropoff_lng numeric(9,6)` (lat `BETWEEN -90 AND 90`, lng `BETWEEN -180 AND 180`, địa chỉ `btrim <> ''`), `distance_km numeric(7,1) CHECK > 0`, `is_round_trip bool NOT NULL`, `return_at timestamptz NULL`, `quote jsonb NOT NULL`.
  - **Bổ sung (snapshot):** `pickup_province_code text NOT NULL FK provinces`, `duration_minutes int NOT NULL CHECK > 0`, `vehicle_class_name text NOT NULL`, `seats int NOT NULL`.
  - `CHECK (is_round_trip = (return_at IS NOT NULL))`, `CHECK (return_at IS NULL OR return_at > pickup_at)`.
  - FK kép `(booking_id, listing_id) → bookings(id, listing_id)`, `(vehicle_class_id, listing_id) → charter_vehicle_classes(id, listing_id)`. Index `(vehicle_class_id)`.
  - Job `FinishCharters` dùng `bookings_service_end_idx` (đã có ở `00012`), không cần index mới.
- Down: drop theo thứ tự ngược lại.

- [ ] **Step 1: Viết test fail** trong `schema_test.go`, chèn trực tiếp bằng SQL:
  - `TestCharterSurchargeCalcRules`: `night` + `percent` thì 23514; `round_trip_waiting` + `percent` thì 23514; `weekend` + `percent` + `amount = 10001` thì 23514; hai dòng cùng `(class, kind)` thì 23505.
  - `TestCharterClassPricePerKmPositive`: `price_per_km = 0` (kể cả khi `base_fare = 500.000`) thì 23514; `base_fare = 0, price_per_km = 12.000` được; `seats = 0` thì 23514.
  - `TestCharterServiceNightPair`: chỉ có `night_start` thì 23514; `night_start = night_end` thì 23514.
  - `TestCharterDetailClassMustMatchListing`: `vehicle_class_id` của listing khác thì 23503.
  - `TestCharterDetailRoundTripNeedsReturn`: `is_round_trip = true, return_at NULL` thì 23514; `false` mà có `return_at` thì 23514; `return_at = pickup_at` thì 23514.
  - `TestCharterDetailDistancePositive`: `distance_km = 0` thì 23514; `distance_km = 30.25` được lưu thành `30.3` (numeric(7,1)).
- [ ] **Step 2: Chạy** `go test ./internal/booking -run 'TestCharter'`. Kỳ vọng FAIL.
- [ ] **Step 3: Viết migration**, rồi `make migrations-sum` và `make sqlc`. Query đọc tọa độ và km cast `::float8`; query ghi nhận `float8` và `tenths int` (Task 5), nên không cần override kiểu `numeric` trong `sqlc.yaml`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó `go vet ./... && go test -race ./...` (gồm `TestMigrationChecksums`).
- [ ] **Step 5: Commit** `feat(db): private charter schema`.

---

### Task 2: Provider bản đồ (Fake, Goong, cache) và cấu hình `MAP_PROVIDER`

**Files:**
- Create: `internal/platform/maps/{maps.go,fake.go,goong.go,cache.go,fake_test.go,goong_test.go,cache_test.go}`, `internal/platform/config/maps_test.go`
- Modify: `internal/platform/config/config.go`, `cmd/api/main.go` (hàm `mapProvider(cfg)`, log `MapWarnings()`; chưa truyền vào service nào)

**Interfaces:**
- Produces:
  - `type Point struct { Lat, Lng float64 }`, `type Route struct { DistanceMeters, DurationSeconds int64 }`.
  - `type Provider interface { Name() string; Route(ctx context.Context, from, to Point) (Route, error) }`.
  - `var ErrNoRoute = errors.New("maps: no route")`, `var ErrUnavailable = errors.New("maps: provider unavailable")`. Mọi lỗi trả về `errors.Is` được một trong hai.
  - `func NewFake() *Fake`; `(*Fake).SetRoute(from, to Point, r Route)`; `(*Fake).SetErr(err error)` (nil để bỏ); `(*Fake).Calls() int`; `Name() = "fake"`. Không có route đặt sẵn: `DistanceMeters = round(haversine(from, to) × 1.3)`, `DurationSeconds = round(DistanceMeters / 40 km/h)`; hai điểm trùng nhau → `ErrNoRoute`. Tất định, an toàn khi chạy song song.
  - `type GoongConfig struct { APIKey, BaseURL string; Timeout time.Duration /*0 → 5s*/ }`, `func NewGoong(cfg GoongConfig, hc *http.Client) *Goong`, `Name() = "goong"`. Gọi `GET {BaseURL}/DistanceMatrix?origins={lat},{lng}&destinations={lat},{lng}&vehicle=car&api_key=…` (tọa độ format `%.6f`), đọc `rows[0].elements[0]`: `status = "OK"` thì lấy `distance.value` (m), `duration.value` (s); status khác hoặc `distance.value = 0` → `ErrNoRoute`; HTTP ≠ 200, JSON lỗi, lỗi mạng, timeout → `ErrUnavailable`. Chuỗi lỗi không chứa URL đầy đủ (key nằm trong query). Kiểm tra lại tên path và trường JSON với docs.goong.io trước khi bật thật.
  - `func NewCached(p Provider, ttl time.Duration, max int, now func() time.Time) Provider`: key = 4 tọa độ làm tròn 5 chữ số; không cache lỗi; đầy thì bỏ mục cũ nhất; `Name()` của provider bên trong.
  - Config: `MapProvider string` (`fake|goong`), `GoongAPIKey`, `GoongBaseURL` (env `MAP_PROVIDER`, `GOONG_API_KEY`, `GOONG_BASE_URL` mặc định `https://rsapi.goong.io`). Unset: có `GOONG_API_KEY` thì `goong`, không thì `fake` (prod thì `goong`). `fake` khi `APP_ENV=prod` → lỗi "MAP_PROVIDER must be goong when APP_ENV=prod". `goong` thiếu key → lỗi. Prod mà base URL không `https://` → lỗi. `func (c Config) MapWarnings() []string`: `fake` thì cảnh báo "map provider is FAKE — distances are estimates".
  - `cmd/api`: `func mapProvider(cfg config.Config) maps.Provider` trả `maps.NewCached(fake hoặc goong, 6*time.Hour, 10000, time.Now)`.

- [ ] **Step 1: Viết test fail:**
  - `TestFake_DeterministicAndOverrides`: Hà Nội (21.028511, 105.804817) → Hải Phòng (20.844912, 106.688084) gọi hai lần ra cùng `Route`, `DistanceMeters` trong [110.000, 130.000]; `SetRoute` thắng; `SetErr(ErrUnavailable)` trả lỗi; hai điểm trùng → `ErrNoRoute`.
  - `TestGoong_ParsesDistanceMatrix` (httptest): body `{"rows":[{"elements":[{"status":"OK","distance":{"value":30240},"duration":{"value":2700}}]}]}` → `Route{30240, 2700}`; request có `origins=16.054400,108.202200`, `vehicle=car`, `api_key=k`.
  - `TestGoong_NoRoute`: `"status":"ZERO_RESULTS"` → `ErrNoRoute`; `rows: []` → `ErrNoRoute`.
  - `TestGoong_TimeoutIsUnavailable` (Review Focus 4): server ngủ 200ms, `Timeout = 50ms` → `ErrUnavailable` trong < 150ms; HTTP 500 → `ErrUnavailable`; chuỗi lỗi không chứa `"secret-key"`.
  - `TestCached_HitsAndExpiry`: hai lần cùng điểm (khác nhau ở chữ số thứ 6) → provider gọi 1 lần; sau TTL → 2 lần; lỗi không cache (lần sau gọi lại); `max = 2` với 3 cặp → mục cũ nhất bị bỏ.
  - `TestConfig_MapProvider`: dev không key → `fake`; dev có key → `goong`; prod không key → lỗi chứa `GOONG_API_KEY`; prod + `MAP_PROVIDER=fake` → lỗi; `MAP_PROVIDER=google` → lỗi "must be goong or fake".
- [ ] **Step 2: Chạy** `go test ./internal/platform/maps ./internal/platform/config`. Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Haversine dùng bán kính 6.371.000 m.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(maps): route provider with fake, Goong client and cache`.

---

### Task 3: Dịch vụ xe bao chuyến, loại xe, phụ phí, điều kiện gửi duyệt

**Files:**
- Create: `internal/listing/{charter.go,charter_classes.go,http_charter.go,charter_test.go,charter_classes_test.go}`
- Modify: `internal/listing/{errors.go,review.go,public.go,http_partner.go,queries.sql}`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: `lockOwned`, `requireActivePartner`, `logChange`, `fieldDiff`, `withMessage`, `hhmm`, `clockText`, `pgTime`, `minutes` (đã có).
- Produces:
  - `const charterServiceType = "private_charter"`; `supportedServiceType` trả `true` cho loại này.
  - Lỗi: `ErrNotCharterListing` (422 `wrong_service_type`, "Thao tác này chỉ áp dụng cho Listing xe bao chuyến"), `ErrInvalidCharter` (422 `invalid_charter`), `ErrCharterServiceRequired` (422 `charter_service_required`, "Cần khai báo khu vực phục vụ trước"), `ErrCharterClassNotFound` (404 `not_found`).
  - `type CharterServiceInput struct { ProvinceCodes []string; MaxDistanceKm *float64; NightStart, NightEnd *string /*"HH:MM"*/ }`: 1..34 tỉnh có thật (bỏ trùng), `MaxDistanceKm` 1.0..2000.0 và tối đa 1 chữ số thập phân, giờ đêm cả hai hoặc không, khác nhau.
  - `type CharterService struct { ListingID uuid.UUID; ProvinceCodes []string; MaxDistanceTenths *int64; NightStart, NightEnd *string }` (json `max_distance_km` là số km).
  - `type CharterClassInput struct { Name string; Seats int; BaseFare, PricePerKm int64 }`: tên 1..100 rune, chỗ 1..50, `base_fare` 0..50.000.000, `price_per_km` 1..1.000.000 (ruling R1; `0` → 422 "Giá mỗi km phải lớn hơn 0").
  - `type CharterClassPatch struct { Name *string; Seats *int; BaseFare, PricePerKm *int64; Active *bool }`.
  - `type Surcharge struct { Kind, Calc string; Amount int64; Active bool }`; `type SurchargeInput = Surcharge`. Hằng `SurchargeNight = "night"`, `SurchargeWeekend = "weekend"`, `SurchargeHoliday = "holiday"`, `SurchargeRoundTripWaiting = "round_trip_waiting"`, `CalcFixed = "fixed"`, `CalcPercent = "percent"`. Kiểm tra như CHECK ở Task 1; `fixed` 0..5.000.000.
  - `type CharterClass struct { ID, ListingID uuid.UUID; Name string; Seats int; BaseFare, PricePerKm int64; Active bool; Surcharges []Surcharge }`.
  - `type CharterDetail struct { Service *CharterService; Classes []CharterClass }`.
  - `func (s *Service) SetCharterService(ctx, partnerID, actor, listingID uuid.UUID, in CharterServiceInput) (CharterService, error)`: upsert dịch vụ, thay toàn bộ khu vực, audit `charter_service`.
  - `func (s *Service) CreateCharterClass(ctx, partnerID, actor, listingID uuid.UUID, in CharterClassInput) (CharterClass, error)` (chưa có dịch vụ → `ErrCharterServiceRequired`) và `func (s *Service) UpdateCharterClass(ctx, partnerID, actor, listingID, classID uuid.UUID, in CharterClassPatch) (CharterClass, error)`. Audit `charter_class`.
  - `func (s *Service) SetCharterSurcharges(ctx, partnerID, actor, listingID, classID uuid.UUID, in []SurchargeInput) (CharterClass, error)`: mỗi `kind` tối đa một lần (trùng → 422); upsert theo `(class, kind)`; kind không có trong body → `active = false` (không xóa cứng). Audit `charter_surcharges`.
  - Mọi lệnh sửa: partner `FOR SHARE` → listing `FOR NO KEY UPDATE` (`lockOwned`) → dòng con. Sửa giá chỉ áp cho báo giá mới.
  - `func (s *Service) GetCharter(ctx, partnerID, listingID uuid.UUID) (CharterDetail, error)` và `func (s *Service) charterDetail(ctx, dbtx store.DBTX, listingID uuid.UUID, includeInactive bool) (CharterDetail, error)`.
  - `requireComplete` cho `private_charter`: **giữ** ba mục vị trí (địa chỉ nhà xe), thêm "khu vực phục vụ (ít nhất 1 tỉnh)" và "ít nhất 1 loại xe đang hoạt động".
  - `PublicDetail.Charter *CharterDetail` (chỉ loại xe active, chỉ phụ phí active và > 0). `AdminListingDetail` có thêm `Charter`.
  - Routes đối tác (`withID`, listing phải là `private_charter`, đăng ký qua `s.charterRoutes(mux, withID)`):
    - `GET /v1/partner/listings/{id}/charter`
    - `PUT /v1/partner/listings/{id}/charter-service`
    - `POST /v1/partner/listings/{id}/charter-classes`
    - `PATCH /v1/partner/listings/{id}/charter-classes/{classID}`
    - `PUT /v1/partner/listings/{id}/charter-classes/{classID}/surcharges` (body `{"surcharges":[…]}`)

- [ ] **Step 1: Viết test fail** `charter_test.go`, `charter_classes_test.go`. Clock `2026-11-10T09:00:00+07:00`. Listing "Xe bao chuyến Đà Nẵng" (48), khu vực {48, 46}, km tối đa 300.0, đêm 22:00–05:00. Loại "Xe 7 chỗ" (7 chỗ, 200.000, 12.000/km), phụ phí night fixed 100.000, weekend percent 1000, holiday percent 3000, round_trip_waiting fixed 150.000. Loại "Xe 16 chỗ" (400.000, 18.000/km), không phụ phí.
  - `TestSetCharterService_UpsertAreasAndAudit`: lần 2 đổi khu vực {48} → {48, 46, 56}, audit có diff.
  - `TestSetCharterService_Validation422`: 0 tỉnh, tỉnh `"99"`, `max_distance_km = 30.25`, chỉ có `night_start`, `"25:00"`.
  - `TestSetCharterService_OnRental422`: `wrong_service_type`.
  - `TestCreateCharterClass_NeedsService422`, `TestCreateCharterClass_Validation422` (51 chỗ, `price_per_km = 1.000.001`, `base_fare = 50.000.001`).
  - `TestCharterClass_PricePerKmMustBePositive422` (ruling R1): tạo với `price_per_km = 0, base_fare = 500.000` → 422; PATCH `price_per_km = 0` → 422; `base_fare = 0, price_per_km = 12.000` → 201.
  - `TestSetCharterSurcharges_Rules`: `night` percent → 422; `weekend` percent 10001 → 422; trùng kind → 422; gửi lại chỉ `weekend` thì ba kind kia `active = false`.
  - `TestUpdateCharterClass_OtherPartner404`, `TestSuspendedPartnerCannotEditCharter` (403 `partner_suspended`).
  - `TestSubmit_CharterCompleteness`: chưa có gì → message liệt kê đủ hai mục charter (cùng mục chung còn thiếu); loại xe `active = false` không tính.
  - `TestPublicDetail_Charter`: loại xe tắt và phụ phí tắt/0 không hiện.
  - HTTP: `TestHTTP_CharterRoutes_MalformedID404`, `TestHTTP_CharterService_UnknownField400`, `TestHTTP_CharterClass_OtherListing404`.
- [ ] **Step 2: Chạy** `go test ./internal/listing -run 'Charter|Submit_Charter'`. Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Thêm 5 route vào `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(listing): charter service, vehicle classes, surcharges and submit completeness`.

---

### Task 4: Tính giá, báo giá public, tìm kiếm

**Files:**
- Create: `internal/listing/{charter_quote.go,charter_quote_test.go}`
- Modify: `internal/platform/money/{money.go,money_test.go}`, `internal/listing/{deps.go,http_charter.go,public.go,queries.sql}`, `cmd/api/{main.go,routes.go,routes_test.go}`

**Interfaces:**
- Consumes: Task 2 (`maps.Provider`, `maps.ErrNoRoute`, `maps.ErrUnavailable`), Task 3.
- Produces:
  - `func MulTenths(amount, tenths int64) int64` trong `money`: `(amount × tenths + 5) / 10`, panic nếu âm hoặc tràn (như `MulBps`).
  - Deps `listing` thêm `Maps maps.Provider` và `Holidays HolidayChecker` với `type HolidayChecker interface { IsHoliday(ctx context.Context, tx pgx.Tx, d time.Time) (bool, error) }` (`*settings.Service`; `main` tạo `settingsSvc` trước `listingSvc`).
  - Lỗi: `ErrMapUnavailable` (503 `map_unavailable`, "Chưa tính được quãng đường lúc này, vui lòng thử lại sau ít phút"), `ErrNoRoute` (422 `no_route`, "Không tìm được đường đi giữa điểm đón và điểm trả"), `ErrOutsideServiceArea` (422 `outside_service_area`, "Điểm đón nằm ngoài khu vực phục vụ của nhà xe"), `ErrDistanceExceedsMax` (422 `distance_exceeds_max`), `ErrInvalidLocationPoint` (422 `invalid_location`, "Tọa độ phải nằm trong lãnh thổ Việt Nam"), `ErrInvalidCharterTime` (422 `invalid_charter_time`), `ErrCharterPickupTooSoon` (422 `pickup_too_soon`, "Giờ đón quá gần, cần để đủ thời gian nhà xe phản hồi và bạn thanh toán").
  - `type CharterPlace struct { Address string; Lat, Lng float64; ProvinceCode string /*chỉ điểm đón*/ }` và `func (p CharterPlace) normalize(needProvince bool) (CharterPlace, error)`.
  - `type CharterRoute struct { Provider string; DistanceMeters, DurationSeconds, DistanceTenths int64; DurationMinutes int }` với `DistanceTenths = ceil(m/100)`, `DurationMinutes = ceil(s/60)`.
  - `func (s *Service) CharterRoute(ctx context.Context, pickup, dropoff CharterPlace) (CharterRoute, error)`: **không mở transaction**; map `maps.ErrUnavailable` → `ErrMapUnavailable`, `maps.ErrNoRoute` → `ErrNoRoute`.
  - `func CheckCharterTime(pickup time.Time, roundTrip bool, ret *time.Time, durationMinutes int, now time.Time, paymentHours int) (serviceEnd time.Time, err error)`: luật ở Global Constraints. Hằng `charterHorizonDays = 180`, `maxCharterReturnSpan = 7 * 24 * time.Hour`; dùng lại `minPartnerResponse`.
  - `func InNightWindow(start, end string /*"HH:MM"*/, t time.Time) bool` (giờ VN, `[start, end)`, qua nửa đêm khi `start > end`).
  - `type SurchargeLine struct { Kind, Calc string; Rate int64; Amount int64 }` (json snake_case).
  - `type CharterPricing struct { BaseFare, PricePerKm int64; Surcharges []Surcharge /*active*/; NightStart, NightEnd *string }`, `type PricingInput struct { OneWayTenths int64; RoundTrip bool; PickupAt time.Time; Holiday bool }`.
  - `type CharterQuote struct { VehicleClassID uuid.UUID; ClassName string; Seats int; Provider string; DistanceMeters, DurationSeconds int64; DistanceTenths, BilledTenths int64; DurationMinutes int; IsRoundTrip bool; BaseFare, PricePerKm, DistanceTotal, Fare int64; Surcharges []SurchargeLine; Total int64 }` (json: `distance_km`, `billed_km` là số km một chữ số thập phân). Đây cũng là nội dung `booking_charter_details.quote`.
  - `func PriceCharter(p CharterPricing, in PricingInput) CharterQuote`: hàm thuần. `BilledTenths = OneWayTenths × (2 nếu khứ hồi)`, `DistanceTotal = MulTenths(PricePerKm, BilledTenths)`, `Fare = BaseFare + DistanceTotal`. Phụ phí theo Global Constraints (cuối tuần/lễ lấy khoản lớn hơn, bằng nhau ghi `holiday`), chỉ khoản > 0 vào `Surcharges`, thứ tự `night, weekend|holiday, round_trip_waiting`.
  - `type CharterTrip struct { Pickup, Dropoff CharterPlace; PickupAt time.Time; RoundTrip bool; ReturnAt *time.Time }`.
  - `func (s *Service) CharterQuoteTx(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, classID *uuid.UUID, trip CharterTrip, route CharterRoute) ([]CharterQuote, error)`: đọc dịch vụ (không có → `ErrListingNotFound`), tỉnh đón thuộc khu vực (`ErrOutsideServiceArea`), `DistanceTenths ≤ max` (`ErrDistanceExceedsMax`), `Holidays.IsHoliday(pickup)`; `classID = nil` → mọi loại active, sắp theo `seats`, rồi `id`; có `classID` → đúng loại đó, phải active và thuộc listing, nếu không thì `ErrCharterClassNotFound`. Người gọi đã giữ listing `FOR SHARE` (booking) hoặc đọc trong tx chỉ đọc (báo giá public).
  - `type CharterQuoteInput struct { ListingID uuid.UUID; Pickup, Dropoff CharterPlace; PickupAt time.Time; IsRoundTrip bool; ReturnAt *time.Time }` và `func (s *Service) QuoteCharter(ctx context.Context, in CharterQuoteInput) (CharterQuoteResult, error)` với `CharterQuoteResult{ DistanceKm float64; DurationMinutes int; Quotes []CharterQuote }`: chuẩn hóa → listing phải visible và là `private_charter` (không thì 404) → `CharterRoute` (ngoài tx) → `CheckCharterTime` (settings đọc trong tx) → `CharterQuoteTx` trong tx chỉ đọc.
  - Route public `POST /v1/charter-quotes` đăng ký bằng `func (s *Service) CharterQuoteRoutes(mux httpx.Router, clientIP func(*http.Request) string)` với `httpx.RateLimit(20, 20, clientIP)` (gọi bản đồ tốn tiền); `routes.go` gọi với `d.ClientIP`. Header `Cache-Control: no-store`.
  - Tìm kiếm: `SearchPublicListings` lọc `province_code` khớp **hoặc** (với `private_charter`) có trong `charter_service_areas`; ẩn charter không có loại xe active (như rental ẩn khi không có điểm nhận). `price_from` của charter (cả `SearchPublicListings` và `GetPublicListingBySlug`) = `min(base_fare + price_per_km)` của loại xe active (ruling R1).

- [ ] **Step 1: Viết test fail** `money_test.go`: `TestMulTenths`: `(12000, 303) = 363600`, `(12345, 303) = 374054` (374.053,5 làm tròn lên), `(1, 4) = 0`, `(1, 5) = 1`.
- [ ] **Step 2: Viết test fail** `charter_quote_test.go` (fixture Task 3, Fake route sân bay Đà Nẵng (16.0544, 108.2022) → Hội An (15.8801, 108.3380) = 30.240 m / 2.700 s; now thứ Ba 10/11/2026 09:00):
  - `TestCharterRoute_RoundsUp`: 30.240 m → 30,3 km, 45 phút; 30.200 m → 30,2; 30.201 m → 30,3; 2.701 s → 46 phút.
  - `TestPriceCharter_OneWayWeekday`: 08:00 thứ Tư 11/11 → `distance_total 363.600`, `total 563.600`, không phụ phí. Xe 16 chỗ → `945.400`.
  - `TestPriceCharter_RoundTrip`: `billed_km 60,6`, `distance_total 727.200`, + `round_trip_waiting 150.000` → `1.077.200`.
  - `TestPriceCharter_NightWindow` (Review Focus 2): 22:00 → `663.600`; 04:59 → có night; 05:00 và 21:59 → `563.600`; `2026-11-11T15:00:00Z` → có night; khung 00:00–05:00: 00:00 có, 05:00 không; dịch vụ không có khung đêm → không áp.
  - `TestInNightWindow`: bảng giá trị cho cả khung qua nửa đêm và không qua.
  - `TestPriceCharter_WeekendHolidayTakeLarger` (Review Focus 3): thứ Bảy 14/11 08:00 → weekend `56.360`, total `619.960`; thêm 14/11 là ngày lễ → chỉ holiday `169.080`, total `732.680`; holiday 0 + weekend 10% → weekend; thứ Tư là ngày lễ → `732.680`.
  - `TestPriceCharter_PercentOnFareIncludingBothLegs`: khứ hồi thứ Bảy → weekend `92.720`, total `1.169.920`.
  - `TestPriceCharter_ZeroSurchargesNoLines`: phụ phí `active=false` hoặc `amount=0` → `Surcharges` rỗng.
  - `TestCharterQuote_VNDateForWeekendAndHoliday` (Review Focus 3): `2026-11-13T17:30:00Z` (00:30 thứ Bảy VN) → có night + weekend → `719.960`.
  - `TestCheckCharterTime`: pickup 11:59 hôm nay → `pickup_too_soon`, 12:00 → OK; có giây → 422; sau 180 ngày → 422; một chiều có `return_at` → 422; khứ hồi `return_at = pickup + 44 phút` (chạy 45 phút) → 422, `+45 phút` → OK, `+7 ngày 1 phút` → 422; `serviceEnd` một chiều = pickup + 45 phút, khứ hồi = `return_at`.
  - `TestCharterQuoteTx_AreaAndMaxKm`: tỉnh đón 79 → `outside_service_area`; route 300.000 m → OK, 300.001 m (300,1 km) → `distance_exceeds_max`; khứ hồi so với một chiều; loại xe tắt → 404.
  - `TestQuoteCharter_AllActiveClasses`: hai báo giá sắp theo số chỗ; listing ẩn hoặc không phải charter → 404; tọa độ (1.0, 100.0) → `invalid_location`; Fake `SetErr(maps.ErrUnavailable)` → 503 `map_unavailable`.
  - `TestHTTP_CharterQuote`: trường lạ → 400; thiếu địa chỉ → 422; quá 20 lần/phút cùng IP → 429.
  - `TestSearch_CharterByServiceArea`: listing ở 48 có khu vực 46 → tìm `province=46` thấy; `price_from = 212.000` (Xe 7 chỗ 200.000 + 12.000, rẻ hơn Xe 16 chỗ 418.000); tắt hết loại xe → không thấy.
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt.** Thêm `POST /v1/charter-quotes` vào danh sách route public của `routes_test.go`.
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(listing): charter pricing, public quotes and search by service area`.

---

### Task 5: Gửi yêu cầu xe bao chuyến, các màn xem

**Files:**
- Create: `internal/booking/{create_charter.go,charter_helpers_test.go,create_charter_test.go}`
- Modify: `internal/booking/{views.go,http_customer.go,queries.sql}`, `cmd/api/routes_test.go` (thêm `POST /v1/bookings/charter`)

**Interfaces:**
- Consumes: Task 4 (`CharterRoute`, `CheckCharterTime`, `CharterQuoteTx`, `CharterQuote`); plan 7 (`requestResponseDue`, `insertRequestedBooking`, `emailPartner`); `BookableListing`, `CancellationPolicy`, `CurrentCommissionRate`, `requestHash`, `replayed`, `storeIdempotentResponse`, `insertCheckout`, `normalizeContact`, `randomToken`.
- Produces:
  - Hằng: `serviceTypeCharter = "private_charter"`, `lineKindCharterBase = "charter_base"`, `lineKindCharterDistance = "charter_distance"`, `lineKindSurcharge = "surcharge"`, `endpointCharter = "POST /v1/bookings/charter"`.
  - `ErrQuoteChanged` (409 `quote_changed`, "Giá chuyến đi đã thay đổi, vui lòng xem lại báo giá").
  - `type CreateCharterInput struct { ListingID, VehicleClassID uuid.UUID; Pickup, Dropoff listing.CharterPlace; PickupAt time.Time; IsRoundTrip bool; ReturnAt *time.Time; QuotedTotal int64; ContactName, ContactPhone, ContactEmail string; Note *string }` (json snake_case; `CharterPlace` json `address`, `lat`, `lng`, `province_code`).
  - Query `InsertBookingItemTenths` (`quantity = @quantity_tenths::bigint::numeric / 10`) và `func insertItemTenths(ctx, q *store.Queries, bookingID uuid.UUID, kind, desc string, unit, tenths, total int64, now time.Time) error`.
  - Query `InsertCharterDetail` (tọa độ `@lat::float8`, km `@distance_tenths::int::numeric / 10`), `GetCharterDetails` (tọa độ và km `::float8`).
  - `func (s *Service) CreateCharter(ctx context.Context, customerID uuid.UUID, idemKey string, in CreateCharterInput) (BookingView, error)`, theo đúng thứ tự:
    1. Key hợp lệ, `requestHash`, chuẩn hóa input (liên hệ, địa điểm, `QuotedTotal > 0`); lỗi → 422, chưa I/O.
    2. Đọc **không khóa** response idempotent đã lưu (`GetIdempotentResponse`); có và cùng hash → trả booking đó, **không gọi bản đồ**; khác hash → 422 như cũ.
    3. `s.listings.CharterRoute(ctx, in.Pickup, in.Dropoff)` **ngoài** transaction (lỗi 503/422 trả luôn, key chưa bị giữ).
    4. Một transaction: giữ Idempotency-Key (đã có người giữ → replay như `CreateRental`), `FillProfileIfEmpty`, `BookableListing` (phải `private_charter`, không thì `listing.ErrWrongServiceType`), `settings.GetTx`, `CheckCharterTime` → `serviceEnd`, `CharterQuoteTx(listing, &classID, trip, route)` → `quote`; `quote.Total != in.QuotedTotal` → `ErrQuoteChanged` (rollback, key không bị giữ).
    5. Hoa hồng `private_charter`, chính sách hủy, `gross = quote.Total`. Insert checkout (`total = gross`), booking `requested` (`service_start_at = pickup_at`, `service_end_at = serviceEnd`, `response_due_at = requestResponseDue`), `booking_items` theo Global Constraints (mô tả: `"Giá mở cửa {loại xe}"`, `"Quãng đường 30,3 km"` / `"Quãng đường khứ hồi 2 × 30,3 km"`, `"Phụ phí ban đêm"`, `"Phụ phí cuối tuần (10%)"`, `"Phụ phí ngày lễ (30%)"`, `"Phụ phí chờ khứ hồi"`), `booking_charter_details` (`quote` = JSON của `CharterQuote`), history (actor customer).
    6. **Không** tạo payment, không giữ gì. Email khách ("đã gửi yêu cầu, chờ nhà xe xác nhận trước {response_due_at}") và đối tác ("có yêu cầu xe bao chuyến mới", tóm tắt điểm đón/trả, giờ, loại xe, km, tổng tiền, hạn phản hồi).
  - Views: `BookingView.Charter *CharterView` với `CharterView{VehicleClassID uuid.UUID; VehicleClassName string; Seats int; PickupAt time.Time; PickupAddress string; PickupLat, PickupLng float64; PickupProvinceCode, DropoffAddress string; DropoffLat, DropoffLng, DistanceKm float64; DurationMinutes int; IsRoundTrip bool; ReturnAt *time.Time; Quote json.RawMessage}`. `can_reschedule = false`; `can_cancel` như thuê xe (rút ở `requested`/`awaiting_payment`, `cancelBlock` khi `confirmed`). `Request` và `payment`/`qr_token` theo luật plan 7.
  - Route khách: `POST /v1/bookings/charter` (header `Idempotency-Key`, 201 `BookingView`).

- [ ] **Step 1: Viết helper** `charter_helpers_test.go`: đối tác và listing như Task 3 đã duyệt, hoa hồng `private_charter` 1000 bps, chính sách hủy `[{48,10000},{24,5000}]`, `maps.Fake` với route Task 4; helper `charterInput(class uuid.UUID, pickup time.Time, roundTrip bool, ret *time.Time, quoted int64)`, `createCharter`, `charterDetail(t, bookingID)`.
- [ ] **Step 2: Viết test fail** `create_charter_test.go` (now thứ Ba 10/11 09:00):
  - `TestCreateCharter_Requested`: Xe 7 chỗ, một chiều, thứ Tư 11/11 08:00, `quoted_total 563.600`. Kỳ vọng `status = requested`, `booking_mode = request`, `gross 563.600`, `commission 56.360`, hai item (`charter_base` 200.000 × 1, `charter_distance` 12.000 × 30,3 = 363.600), `distance_km 30,3`, `duration_minutes 45`, `service_end_at = 08:45`, `response_due_at = 10/11 21:00`, **không** có `payments`, checkout `open`, `quote.total = 563.600`, email cho khách và đối tác.
  - `TestCreateCharter_RoundTripNightItems`: khứ hồi 22:00 thứ Tư, về 12/11 06:00 → items `charter_base`, `charter_distance` (quantity 60,6, 727.200), `surcharge` 100.000 (đêm), `surcharge` 150.000 (chờ) → gross `1.177.200`; `service_end_at = return_at`; Σ items = gross.
  - `TestCreateCharter_QuoteChanged409` (Review Focus 1): sau khi đối tác đổi `price_per_km` → 409, không có checkout, gửi lại cùng key với giá mới → 201.
  - `TestCreateCharter_PriceEditAfterRequestKeepsSnapshot` (Review Focus 1): tạo đơn, đối tác đổi giá, xem đơn → gross, items, `quote` không đổi.
  - `TestCreateCharter_MapDown503ThenRetry` (Review Focus 4): Fake lỗi → 503, không có dòng `idempotency_keys`; bỏ lỗi, cùng key → 201.
  - `TestCreateCharter_ReplayDoesNotCallMap` (Review Focus 4): lần 2 cùng key → cùng booking, `Fake.Calls()` không tăng; cùng key khác body → 422.
  - `TestCreateCharter_Validation`: `pickup_too_soon` (11:59), ngoài khu vực (79) → 422, quá km → 422, loại xe tắt → 404, loại xe của listing khác → 404, listing thuê xe → 422 `wrong_service_type`, đối tác bị đình chỉ → 422 `listing_not_bookable`.
  - `TestCharterView_RequestedHasNoPaymentNoQR`: `payment = null`, không `qr_token`, `can_cancel = true`, `can_reschedule = false`, `charter.distance_km = 30.3`, `request.response_due_at` có.
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt.**
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(booking): private charter requests with server-side quote`.

---

### Task 6: Chấp nhận, từ chối, rút, hết hạn và tiền về trễ cho charter

**Files:**
- Create: `internal/booking/confirm_charter_test.go`
- Modify: `internal/booking/{confirm.go,capacity.go}`

**Interfaces:**
- Consumes: Task 5; `AcceptRequest`, `RejectRequest`, `withdrawRequest`, `ExpireRequests`, `ExpireHolds`, `Pay`, `HandlePaid` (plan 7, không đổi chữ ký).
- Produces:
  - `func charterChecks(rows []store.ListCheckoutBookingsRow, rebookable map[uuid.UUID]bool, minLead time.Duration, now time.Time) error` (trong `capacity.go`, cạnh `rentalChecks`), ghi một mục cho mỗi booking charter **trễ** (`lateBooking`): được khi `rebookable[b.ID]` (listing bán được, do `reconfirmChecks`; thiếu → `errStateChanged`) **và** `b.AcceptedAt != nil` **và** `!b.ServiceStartAt.Before(now.Add(minLead))`. Booking charter `awaiting_payment` chưa trễ không ghi (mặc định `eligible` = được).
  - `HandlePaid`: sau khối thuê xe, nếu checkout có booking `private_charter` thì đọc `settings.GetTx` và gọi `charterChecks(rows, rebookable, charter_late_confirm_min_hours × giờ, now)`. Không khóa thêm gì trước checkout. Cập nhật doc comment của `HandlePaid`.
  - Email đối tác khi xác nhận (và xác nhận lại) dùng `confirmBooking` có sẵn.

- [ ] **Step 1: Viết test fail** `confirm_charter_test.go` (đơn R như Task 5, chấp nhận lúc 10:00):
  - `TestCharterAccept_OpensPayment`: `awaiting_payment`, `payment_due_at = 12:00`, `hold_release_at = 12:05`, payment `pending` 563.600 có `checkout_url`.
  - `TestCharterReject_AndWithdraw`: từ chối → `rejected`, checkout `closed`, không tiền; đơn khác rút ở `awaiting_payment` → `cancelled`/`customer_withdraw`, payment `cancelled`.
  - `TestCharterExpireRequests_AtResponseDue`: 20:59 còn `requested`, 21:00 → `expired`.
  - `TestCharterPaid_Confirms`: tiền về 11:00 → `confirmed`, pending đối tác `507.240`, sổ sàn `+56.360`, `paid_payment_id` được đặt, `Verify` không lỗi, email đối tác.
  - `TestCharterPaid_InGrace`: 12:03 → xác nhận bình thường.
  - `TestCharterLatePaid_Reconfirms`: job đã chạy (12:05 → `expired`), tiền về 12:10 → `confirmed`, history lý do "thanh toán trễ, xác nhận lại", có bút toán.
  - `TestCharterLatePaid_ThreeHourBoundary` (Review Focus 5): đơn đón 10/11 15:10, tạo 09:00, chấp nhận 09:30; job hết hạn lúc 11:35; tiền về 12:10 → `confirmed`; đơn thứ hai giống hệt nhưng tiền về 12:11 → `expired`, refund `late_payment` 563.600 `awaiting_bank_info`, không `ledger_entries`, email khách. Biến thể job chưa chạy (đơn còn `awaiting_payment` quá 11:35): 12:11 → `expired` trong tx webhook, refund như trên.
  - `TestCharterLatePaid_SettingRespected`: `charter_late_confirm_min_hours = 0` → tiền về 15:09 vẫn xác nhận; 15:10 (đúng giờ đón) cũng xác nhận (`≥ 0`).
  - `TestCharterLatePaid_ListingHiddenRefunds` và `TestCharterLatePaid_PartnerSuspendedRefunds` (ruling R2): R chấp nhận lúc 10:00, admin ẩn listing (hoặc đình chỉ đối tác) lúc 12:06, tiền về 12:10 (còn ≫ 3 giờ) → R `expired`, refund `late_payment` 563.600 `awaiting_bank_info`, không `ledger_entries`, `Verify` không lỗi.
  - `TestCharterPaidOnTime_PartnerSuspendedStillConfirms` (ruling R2): đình chỉ lúc 10:30, tiền về 11:00 (đúng hạn) → `confirmed`, có bút toán.
  - `TestCharterPaidAfterWithdraw_RefundedInFull`: `duplicate_payment` 563.600, đơn vẫn `cancelled`, không bút toán.
  - `TestCharterWebhook_DuplicateDelivery`: một lần xác nhận, một cặp bút toán.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL (chủ yếu `ThreeHourBoundary` và `SettingRespected`: hiện đơn trễ luôn được xác nhận lại).
- [ ] **Step 3: Cài đặt.** Toàn bộ `confirm*_test.go`, `request_test.go` cũ phải vẫn PASS.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): charter late payment re-confirm with minimum lead before pickup`.

---

### Task 7: Đón khách bằng QR, hoàn thành chuyến, No-show, tự kết thúc, hủy

**Files:**
- Create: `internal/booking/{charter_lifecycle.go,charter_lifecycle_test.go}`
- Modify: `internal/booking/{rental_lifecycle.go,checkin.go,jobs.go,http_partner.go,queries.sql}`, `cmd/api/{main.go,routes_test.go}` (đăng ký `FinishChartersWorker` ngay task này)

**Interfaces:**
- Consumes: Task 5–6; `scheduleRelease`, `insertHistory`, `CompleteRide`/`MarkNoShow`/`HandOverRental` (UPDATE chung), `cancelBlock`.
- Produces:
  - Đổi tên `endRental` → `func (s *Service) endService(ctx, tx pgx.Tx, b store.Booking, to, actorKind string, actorUser *uuid.UUID, reason *string, now time.Time) error` (nội dung không đổi), dùng cho thuê xe và charter.
  - `const charterPickupEarly = 2 * time.Hour`. `ErrPickupOutsideWindow` (422 `pickup_outside_window`, "Chỉ đón khách được từ 2 giờ trước giờ đón đến khi chuyến tự kết thúc").
  - `func (s *Service) checkInCharter(ctx, tx pgx.Tx, q *store.Queries, b store.Booking, userID uuid.UUID, now time.Time) (CheckInResult, error)`: `confirmed` (không thì `ErrCheckInNotAllowed`), `now ∈ [pickup_at − 2h, service_end_at + charter_auto_complete_hours)` (không thì `ErrPickupOutsideWindow`), `HandOverRental` (đặt `in_service`, `in_service_at = checked_in_at = now`), history actor partner. `checkIn` thêm `case serviceTypeCharter`.
  - `func (s *Service) CompleteCharter(ctx, partnerID, userID, bookingID uuid.UUID) (BookingView, error)`: đọc không khóa (khác đối tác → 404, không phải charter → 409 `invalid_booking_state` "Thao tác này chỉ dành cho đơn xe bao chuyến"), `LockBooking`, `in_service` → `endService(completed)`; đã `completed` → 200 không đổi; khác → 409.
  - `func (s *Service) MarkNoShow(ctx, partnerID, userID, bookingID uuid.UUID) (BookingView, error)`: rẽ nhánh `self_drive_rental` → `MarkRentalNoShow`; `private_charter` → `confirmed` và `now ≥ pickup_at` (sớm hơn → `ErrNoShowTooEarly` với message "Chỉ đánh dấu khách không đến sau giờ đón/nhận xe"), `endService(no_show)`; loại khác → 409. Handler `/no-show` gọi hàm này.
  - Route đối tác mới: `POST /v1/partner/bookings/{id}/complete` (không body, 200 `BookingView`).
  - Job: `FinishChartersArgs{}` (Kind `booking_finish_charters`), `FinishChartersWorker{Service *Service}`, `func (s *Service) FinishCharters(ctx context.Context) (int, error)`, thêm vào `PeriodicJobs()`:
    - `cutoff = now − charter_auto_complete_hours` (đọc settings mỗi lần chạy). Query `ListCharterAutoFinish(cutoff, lim, skip)`: `status IN ('confirmed','in_service') AND service_type = 'private_charter' AND service_end_at <= cutoff`, dùng `bookings_service_end_idx`.
    - Mỗi booking một tx: `LockBooking`, kiểm tra lại; `in_service` → `completed` (lý do "tự hoàn thành sau {N} giờ quá giờ kết thúc chuyến"), `confirmed` → `no_show` (lý do "nhà xe không xác nhận đón khách trước khi chuyến tự kết thúc"), actor system, `endService`. Tracker backoff mới `charterFinishBackoff`, `beforeItem` cho test.
  - Hủy: không đổi code; test chứng minh `cancel()` chạy đúng cho charter.

- [ ] **Step 1: Viết test fail** `charter_lifecycle_test.go` (R một chiều đã `confirmed`, đón 11/11 08:00, `service_end_at` 08:45, gross 563.600):
  - `TestCharterPickup_Window`: 11/11 05:59 → 422 `pickup_outside_window`; 06:00 → `in_service`, `in_service_at` có; quét lại → `Already`. Đơn khác chưa quét: 12/11 08:44 → được; 12/11 08:45 → 422.
  - `TestCharterPickup_NotConfirmed409`: đơn `awaiting_payment`.
  - `TestCompleteCharter`: `in_service` → `completed`, `release_due_at = now + 1 ngày`; gọi lại → 200 không đổi; đơn `confirmed` → 409; đơn thuê xe → 409; đối tác khác → 404. Sau `ReleaseDue`: available `507.240`, `Verify` không lỗi.
  - `TestCharterNoShow`: 11/11 07:59 → 409 `no_show_too_early`; 08:00 → `no_show`, không refund; route `/no-show` vẫn chạy đúng cho thuê xe (`TestRentalNoShow` cũ PASS).
  - `TestFinishCharters_Boundary`: `in_service` → 12/11 08:44 chưa đổi, 08:45 → `completed`, `release_due_at = 13/11 08:45`; `confirmed` chưa quét → 12/11 08:45 → `no_show`. `charter_auto_complete_hours = 48` → mốc dời sang 13/11 08:45.
  - `TestFinishCharters_ManualWins`: đối tác bấm hoàn thành 11/11 09:00 → job không đổi gì.
  - `TestFinishCharters_BackoffSkip`, `TestFinishRentals_IgnoresCharters` và `TestFinishCharters_IgnoresRentals`.
  - `TestCancelCharter_CustomerPolicy`: đơn đón thứ Sáu 13/11 08:00 (gross 563.600), khách hủy 11/11 20:00 (còn 36h) → hoàn 50% = **281.800** `pending` (có bank); available đối tác `563.600 − 56.360 − 281.800 = 225.440`; `Verify` không lỗi.
  - `TestCancelCharter_AdminForceMajeure`: refund 563.600, đối tác 0, sàn net 0, có audit.
  - `TestCancelCharter_InService409`, `TestReschedule_CharterRejected409`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Thêm `POST /v1/partner/bookings/{id}/complete` vào `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS, kể cả `rental_lifecycle_test.go`, `checkin_test.go`, `cancel*_test.go` cũ. Sau đó `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): charter QR pickup, complete, no-show, auto-finish and cancel`.

---

### Task 8: Nối dây, kiểm thử trọn luồng, tài liệu

**Files:**
- Create: `cmd/api/e2e_charter_test.go`
- Modify: `cmd/api/{main.go,routes.go,routes_test.go}`, `docs/superpowers/specs/2026-11-04-database-schema.md` (§1.1, §5.6, §6), `GLOSSARY.md`, `docs/superpowers/plans/README.md`

**Interfaces:**
- Consumes: mọi task trước.
- Produces:
  - `main`: `settingsSvc` tạo trước `listingSvc`; `listing.Deps{…, Maps: mapProvider(cfg), Holidays: settingsSvc}`; log `cfg.MapWarnings()`. `buildMux` không đổi chữ ký; `routes.go` gọi `d.Listing.CharterQuoteRoutes(rr, d.ClientIP)`.
  - Tài liệu DB:
    - Chỗ thiếu của DB doc đã gặp (ruling R3): `UNIQUE(id, listing_id)` của `charter_vehicle_classes` (đích FK kép DB §1.2); `night`/`round_trip_waiting` chỉ `fixed` và `percent ≤ 10000`; `price_per_km > 0`; các cột snapshot của `booking_charter_details` (`pickup_province_code`, `duration_minutes`, `vehicle_class_name`, `seats`).
    - §5.6 ✅ plan 8: CHECK `calc`, `price_from` charter = `min(base_fare + price_per_km)`, khung đêm qua nửa đêm, công thức và thứ tự dòng, luật cuối tuần/lễ lấy khoản lớn hơn, percent trên cước, khu vực theo tỉnh điểm đón, km tối đa so với một chiều.
    - §6: cột bổ sung của `booking_charter_details`; dòng Private Charter trong bảng mốc thời gian (QR trong `[pickup − 2h, service_end + N giờ)`, "hoàn thành chuyến", No-show đối tác bấm, `FinishCharters`); luật tiền về trễ 3 giờ; `quoted_total`.
    - §1.1: charter không có dòng sức chứa; bảng giá đọc dưới `listings FOR SHARE`; không gọi bản đồ trong transaction.
  - `GLOSSARY.md`: Private Charter có quote theo km; No-show gồm "khách không có mặt ở điểm đón".
  - `plans/README.md`: thêm link plan 8, đánh dấu xong.

- [ ] **Step 1: Viết test fail** `e2e_charter_test.go`, qua mux thật với `payment.Fake` và `maps.Fake` (route Task 4):
  1. Admin đặt hoa hồng `private_charter` 10%, tạo đối tác, bật `private_charter`.
  2. Đối tác tạo listing (48), `PUT charter-service`, loại Xe 7 chỗ, phụ phí, ảnh, gửi duyệt, admin duyệt.
  3. Khách `POST /v1/charter-quotes` → Xe 7 chỗ `total 563.600`, `distance_km 30.3`. `POST /v1/bookings/charter` với `quoted_total 563.600` → 201 `requested`, `payment = null`.
  4. Clock 10:00: đối tác `POST /accept`. Khách thấy `payment.checkout_url`.
  5. `simulate-paid` một lần → `confirmed`; `GET /v1/partner/balance` có `pending = 507.240`.
  6. Clock 11/11 07:30: đối tác check-in QR → `in_service`. 09:00: `POST /complete` → `completed`.
  7. `ReleaseDue` lúc 12/11 09:00 → `available = 507.240`, `pending = 0`, `ledger.Verify` không lỗi.
  8. Đơn thứ hai được chấp nhận, thanh toán, không quét QR; clock qua `service_end_at + 24h`: `FinishCharters` → `no_show`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Nối dây** và cập nhật tài liệu.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó `go vet ./... && go test -race ./...` và `go tool sqlc diff` (không có diff).
- [ ] **Step 5: Kiểm tra tay** trên DB riêng của nhánh (`make db-branch`, migrate từ đầu), `PAYMENT_PROVIDER=fake`, `MAP_PROVIDER=fake`:
  1. Tạo listing charter đầy đủ, duyệt; báo giá một chiều, khứ hồi, ban đêm, thứ Bảy, thêm ngày lễ qua `POST /v1/admin/holidays` và báo giá lại.
  2. Đổi giá loại xe rồi gửi yêu cầu với giá cũ → 409; gửi lại với giá mới.
  3. Chấp nhận, để quá 2h05 cho job hết hạn, `simulate-paid` → xác nhận lại.
  4. Quét QR, bấm hoàn thành, xem số dư. Ghi kết quả vào báo cáo task.
  5. Nếu đã có key Goong (phụ lục): `MAP_PROVIDER=goong`, báo giá hai điểm thật, so `distance_km` với app Goong Maps.
- [ ] **Step 6: Commit** `feat(charter): wire private charter; e2e request-to-complete flow`.

---

## Không làm trong plan này

- Geocode/autocomplete phía server, reverse geocode để kiểm tỉnh theo tọa độ (FE map picker làm, mặc định C5). Client bản đồ Google (chỉ có hướng dẫn tạo key ở phụ lục).
- Phí phát sinh không biết trước (chờ thêm giờ, cầu đường ngoài dự kiến): đối tác ghi ở mô tả, thu ngoài sàn (spec §5.7).
- Nhiều điểm dừng, đặt nhiều xe trong một yêu cầu (mặc định C9), lưu báo giá có hạn (mặc định C6).
- Giữ sức chứa cho charter (spec §6.2: việc nhận chuyến là quyết định của đối tác).
- Chuông thông báo trong web (plan 9). Báo cáo §13 (plan 10).

## Các quyết định đã chốt

- **C1.** Ngày đón vừa là cuối tuần vừa là ngày lễ: **chỉ áp khoản lớn hơn**, không cộng dồn. *(đã chốt với người dùng)*
- **C4.** Khu vực phục vụ chỉ ràng **tỉnh điểm đón**; điểm trả tự do; `max_distance_km` so với quãng đường **một chiều**. *(đã chốt với người dùng)*
- **C7.** Thời gian đặt trước như thuê xe: giờ đón ≥ now + 3 giờ (2h trả tiền + 1h phản hồi), trong 180 ngày, tròn phút. *(đã chốt với người dùng)*
- **C11.** Provider bản đồ: **Goong** (dữ liệu Việt Nam tốt, rẻ hơn Google); báo giá public, giới hạn 20 lần/phút/IP. *(đã chốt với người dùng)*
- **R1 (controller).** Loại xe phải có `price_per_km > 0` (`base_fare ≥ 0`), nên báo giá không bao giờ 0đ; `price_from` charter = `min(base_fare + price_per_km)` của loại xe active.
- **R2 (controller).** Spec §8 thắng khi tiền về trễ: đối tác bị đình chỉ hoặc listing bị ẩn → hoàn `late_payment`, không xác nhận lại (như thuê xe).
- **R3 (controller).** Chỗ thiếu của DB doc (`UNIQUE(id, listing_id)`, cột snapshot, `night`/`round_trip_waiting` chỉ `fixed`) được ghi vào DB doc ở Task 8.

## Mặc định của builder (chưa có phản hồi, đang dùng)

- **C2.** Phụ phí đêm chỉ xét **giờ đón** (giờ VN) trong `[night_start, night_end)`, khung qua nửa đêm được; tính một lần cho cả khứ hồi. Cuối tuần/lễ cũng theo ngày đón.
- **C3.** Phụ phí % tính trên **cước** (`base_fare` + tiền km, gồm cả hai chiều khi khứ hồi), không tính trên phụ phí khác.
- **C5.** FE map picker gửi `lat/lng + address` (và `province_code` điểm đón); server chỉ kiểm khung Việt Nam và tỉnh có thật, không geocode. Đối tác vẫn xem từng yêu cầu và từ chối được nếu sai khu vực.
- **C6.** Báo giá không lưu, không có hạn: server tính lại khi gửi yêu cầu, client gửi `quoted_total`, lệch thì 409 `quote_changed`. Tuyến được cache 6 giờ.
- **C8.** Khứ hồi: `return_at` ≥ giờ đón + thời gian chạy một chiều và ≤ giờ đón + 7 ngày; `service_end_at = return_at` (DB §6). Một chiều: `service_end_at = pickup_at + thời gian chạy`.
- **C9.** Một yêu cầu chỉ một loại xe, một xe.
- **C10.** QR đón khách trong `[pickup_at − 2h, service_end_at + charter_auto_complete_hours)`; "hoàn thành chuyến" bất kỳ lúc nào khi `in_service`; No-show khi `confirmed` và `now ≥ pickup_at`; job: `in_service` quá mốc → `completed`, `confirmed` quá mốc → `no_show` (như quyết định 4 của plan 7).

## Mâu thuẫn và chỗ spec chưa nói đã gặp

- **DB §5.6** cho mọi `kind` dùng `fixed` hoặc `percent`; **spec §5.7** nói `night` và `round_trip_waiting` cộng cố định. Plan thêm CHECK chỉ cho `weekend`/`holiday` dùng `percent`, và `percent ≤ 10000`.
- **DB §5.6** không ghi `UNIQUE(id, listing_id)` cho `charter_vehicle_classes` nhưng **DB §1.2** yêu cầu và `booking_charter_details` cần cho FK kép. Plan thêm.
- **DB §5.6** cho `price_per_km ≥ 0`, nên một loại xe có thể giá 0đ (checkout 0đ, PayOS không tạo được link). Theo ruling R1, plan bắt `price_per_km > 0`; `price_from` = `min(base_fare + price_per_km)`.
- **DB §6** `booking_charter_details` thiếu tỉnh điểm đón, thời gian chạy, tên/số chỗ loại xe; cần cho kiểm khu vực, `service_end_at` và hiển thị khi đối tác đổi tên loại xe. Plan bổ sung.
- **DB §6** đặt `service_end_at = return_at` cho khứ hồi, tức chưa tính thời gian chạy chiều về. Plan giữ theo DB doc; QR và tự kết thúc dời thêm `charter_auto_complete_hours` nên không bị cắt ngang chuyến.
- **Spec §8** cho charter xác nhận lại khi tiền về ≥ 3 giờ trước giờ đón, đồng thời đòi "Listing vẫn hiển thị, đối tác chưa bị đình chỉ". **DB §6** cho đối tác bị đình chỉ vẫn chấp nhận yêu cầu đang chờ. Theo ruling R2 (spec §8 thắng, như thuê xe): đối tác bị đình chỉ vẫn chấp nhận được, nhưng tiền về **trễ** khi đối tác bị đình chỉ hoặc listing bị ẩn thì hoàn tiền.
- **Spec §6.2** sơ đồ không có `no_show` cho charter nhưng đoạn chữ cho đối tác đánh dấu. Plan thêm `confirmed → no_show` (đối tác bấm hoặc job), giống thuê xe.
- **DB doc** đánh số Xe bao chuyến là §5.6 và Thuê xe là §5.7, ngược với spec (§5.7 charter, §5.8 rental). Plan trích theo số của từng tài liệu.
- Mã lỗi `pickup_too_soon` của plan 7 có chữ "nhận xe"; charter dùng cùng code với message riêng.

## Phụ lục: Hướng dẫn tạo key Goong/Google

Khi chưa có key, hệ thống chạy `MAP_PROVIDER=fake`: quãng đường là đường chim bay × 1,3, thời gian chạy theo 40 km/h. Số này chỉ để dev và test, **không** dùng để bán thật; `fake` bị chặn khi `APP_ENV=prod`. Key bản đồ là key **server**: chỉ nằm trong `.env` của backend, không bao giờ gửi xuống FE. FE map picker dùng key riêng, giới hạn theo domain.

**Goong (plan này cài client Goong):**
1. Đăng ký tại https://account.goong.io và xác thực email.
2. Vào mục **Keys**, tạo một **API key** (key REST API, dùng cho Distance Matrix). Key **Maptiles** là để FE hiển thị bản đồ, không dùng cho backend.
3. Xem hạn mức miễn phí và bảng giá trên trang giá của Goong; mỗi lần báo giá hoặc gửi yêu cầu tốn tối đa một lượt Distance Matrix (cache 6 giờ giúp giảm số lượt).
4. Thử key: `curl "https://rsapi.goong.io/DistanceMatrix?origins=16.054400,108.202200&destinations=15.880100,108.338000&vehicle=car&api_key=KEY"` phải trả `"status":"OK"` cùng `distance.value` và `duration.value`. Nếu docs.goong.io đổi path hoặc trường JSON, sửa `goong.go` và test trước khi bật.

**Điền vào `.env`** (không commit):
```
MAP_PROVIDER=goong
GOONG_API_KEY=...
GOONG_BASE_URL=https://rsapi.goong.io
```

**Google (chỉ để tham khảo nếu sau này đổi provider; plan này chưa cài client Google):**
1. Vào https://console.cloud.google.com, tạo project và bật **Billing**.
2. **APIs & Services → Library**: bật **Routes API** (Google đã chuyển Distance Matrix sang nhóm Legacy; dự án mới nên dùng Routes API `computeRoutes`/`computeRouteMatrix`).
3. **APIs & Services → Credentials → Create credentials → API key**. Giới hạn key: chỉ Routes API, chỉ IP của server.
4. Khi đó cần thêm `maps.Google` cài `Provider` và giá trị `MAP_PROVIDER=google` trong config.

**Kiểm tra sau khi có key:**
1. Báo giá hai điểm thật (sân bay Đà Nẵng → Hội An): `distance_km` gần với số trên app Goong Maps, làm tròn lên 0,1 km.
2. Tắt mạng ra ngoài (hoặc đặt key sai): báo giá trả 503 `map_unavailable` trong ≤ 5 giây, log không có key.

**Nguồn:** [Goong docs](https://docs.goong.io), [Goong account](https://account.goong.io), [Google Routes API](https://developers.google.com/maps/documentation/routes).
