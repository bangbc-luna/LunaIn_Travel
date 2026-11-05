# Plan 7 — Self-Drive Rental (thuê xe tự lái) + vòng đời Request Booking — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cửa hàng thuê xe đăng được cửa hàng, mẫu xe (kèm số lượng) và điểm nhận xe. Khách tìm mẫu xe còn trống theo tỉnh và khoảng [giờ nhận, giờ trả), gửi yêu cầu thuê; xe được giữ ngay từ lúc gửi. Đối tác chấp nhận hoặc từ chối; chấp nhận rồi thì khách có 2 giờ để trả tiền, và được rút yêu cầu miễn phí khi chưa trả. Tiền về đúng hạn hay trễ, hủy, giao xe bằng QR, nhận lại xe (sớm hoặc đúng hạn) và No-show đều đi qua các đường tiền và sổ cái đã có. Vòng đời Request Booking được viết chung để plan 8 (Private Charter) dùng lại.

**Architecture:**

- `listing` sở hữu ba bảng mới `rental_stores`, `rental_vehicle_models`, `rental_pickup_points`. Nó có thêm:
  - API đối tác cho cửa hàng, mẫu xe, điểm nhận xe; điều kiện gửi duyệt cho `self_drive_rental`.
  - Tìm mẫu xe còn trống public (`GET /v1/rental-vehicles`).
  - Hàm cho `booking` nhận `tx` của người gọi: `LockRentalModels`, `RentalStoreOf`, `PickupPointForBooking`, `RentalModelListingID`. `booking` không bao giờ ghi bảng của `listing`.
  - `BookingGuard` có thêm hai hàm đếm xe đang giữ (`booking.Guard` cài đặt), dùng cho tìm kiếm và cho việc chặn giảm `quantity` (giống mẫu plan 4 Task 9).
- `booking` sở hữu `booking_rental_details`. Nó có thêm:
  - `request.go`: vòng đời Request Booking **chung**: tạo đơn `requested`, đối tác `AcceptRequest`/`RejectRequest`, khách rút yêu cầu (đi qua `CancelByCustomer`), job `ExpireRequests`. Không có chữ "rental" trong file này; phần riêng của loại dịch vụ đi qua `capacity.go`.
  - `capacity.go`: gom các bước sức chứa theo loại dịch vụ: `lockCheckoutCapacity` (thay `lockCheckoutTrips`), `releaseHold` (thay các lời gọi `releaseRideSeats` ở đường hết hạn/hủy), `setRentalHold`, `rentalChecks`. Plan 8 thêm nhánh charter (không có sức chứa) mà không sửa `request.go`.
  - `create_rental.go`, `rental_lifecycle.go` (giao xe, nhận lại xe, No-show do đối tác bấm).
  - `HandlePaid`, `ExpireHolds`, `Pay`, `cancel`, views mở rộng cho `awaiting_payment`/`in_service` thay vì viết đường riêng.
- Bất biến khóa mới: **mọi transaction khóa checkout có booking thuê xe đều khóa dòng `rental_vehicle_models` của nó trước** (DB §1.1, dòng sức chứa đứng trước checkouts), giống luật xe tuyến của plan 6.

**Tech Stack:** như plan 1–6 (Go 1.27, PostgreSQL 17, pgx/v5, sqlc, goose, River, `net/http` ServeMux). PayOS vẫn chỉ có Fake. Cần extension `btree_gist` (đã bật ở `00001`).

**Spec:**
- [spec hệ thống](../specs/2026-11-04-lunain-travel-backend-design.md) §5.4, §5.8, §5.9, §6.2, §6.3, §7, §8, §10
- [thiết kế DB](../specs/2026-11-04-database-schema.md) §1.1, §1.2, §4 (`platform_settings`), §5.7, §6 (`bookings`, mốc thời gian theo loại, `booking_items`, `booking_rental_details`, "Giữ xe thuê"), §10
- [ADR 0002](../../adr/0002-availability-by-overlap.md), [ADR 0003](../../adr/0003-listing-base-plus-type-tables.md), [ADR 0004](../../adr/0004-partner-ledger.md), [GLOSSARY](../../../GLOSSARY.md) (Self-Drive Rental, Vehicle Model, Rental Terms, Request Booking, Hold)
- Header plan 4–6. Rulings 1–26 của plan 4 vẫn ràng buộc, đặc biệt Ruling 9–17 (định tuyến tiền), Ruling 21 (hủy tới `service_end_at`), Ruling 25 (xác nhận lại phải đúng thứ đã bán).

## Global Constraints

- Mọi Global Constraints của plan 1–6 vẫn áp dụng:
  - uuid v7, clock được inject (`clock.VN`), lỗi JSON tiếng Việt.
  - Path id sai định dạng hoặc không tồn tại thì 404. Body sai thì 422. Body có trường lạ thì 400. Query param sai thì 400.
  - Tiền là `bigint` VND, nhân tỷ lệ bằng `money.MulBps`.
  - Test chạy trên DB riêng clone từ template đã migrate, assert đúng số tiền và gọi `ledger.Verify`.
  - Migration trên `main` bất biến: file mới bắt đầu từ `00018`, rồi chạy `make migrations-sum`. Mỗi worktree dùng DB riêng tạo bằng `make db-branch`.
  - Mỗi route `/v1/admin/*` và `/v1/partner/*` mới phải có trong `adminRoutes`/`partnerRoutes` của `cmd/api/routes_test.go`.
  - Thao tác admin gọi `audit.Record` trong cùng transaction. `checkouts.paid_payment_id` chỉ ghi qua `SetCheckoutPaid`. Payment khóa `FOR NO KEY UPDATE`. Phát lại link sau khi trả thiếu không kéo dài hold.
- **Thứ tự khóa (DB §1.1):**
  ```
  users → partners → listings → ride_trips / rental_vehicle_models (id tăng dần) → checkouts (id tăng dần)
        → bookings (id tăng dần) → dòng con của booking (booking_rental_details, settlements, refunds)
        → payments (FOR NO KEY UPDATE) → payouts → partner_balances
  ```
  - Mọi transaction khóa một checkout có booking thuê xe **phải** khóa trước dòng `rental_vehicle_models` của nó: tạo, đối tác chấp nhận/từ chối, khách rút, `ExpireRequests`, `ExpireHolds`, `HandlePaid`, hủy. Transaction chỉ khóa booking (nhận lại xe, No-show) cũng khóa mẫu xe trước booking.
  - Nếu chỉ biết id booking: đọc `vehicle_model_id` không khóa, khóa mẫu xe, rồi mới khóa checkout và booking và kiểm tra lại trạng thái.
  - Một checkout chỉ có một loại dịch vụ; nếu sau này có cả hai thì `ride_trips` trước `rental_vehicle_models`.
  - Ngoại lệ đã biết của plan 6 (`ReissueAfterMismatch`, `replaceLostLink` khóa checkout mà không khóa sức chứa) giữ nguyên và cũng áp cho thuê xe.
  - Không bao giờ gọi provider khi đang mở transaction.
- **Giữ xe (spec §5.8, §7, DB §6 "Giữ xe thuê"), nguyên văn:**
  - **Đỉnh sử dụng cùng lúc (quyết định 5, controller):** khi đang giữ khóa `rental_vehicle_models FOR UPDATE`, tính `peak` = số đơn đang giữ xe cùng lúc nhiều nhất tại một thời điểm bất kỳ trong [nhận, trả); nhận đơn khi `peak + 1 ≤ quantity`. Không dùng số đơn giao nhau thô. Ví dụ: `quantity = 2`, A [T2, T3), B [T4, T5), yêu cầu [T2, T5) → `peak = 1` → nhận; A [T2, T4), B [T3, T5), yêu cầu [T3, T4) → `peak = 2` → từ chối.
  - Một hàm duy nhất `peakRentalHolds` (booking) / `RentalPeakHeld` (`BookingGuard`) dùng cho tạo đơn, xác nhận lại khi tiền về trễ, tìm kiếm (`units_left = quantity − peak`) và chặn giảm `quantity` (khoảng [now, ∞)).
  - Đang giữ xe: `holds_vehicle AND (hold_until IS NULL OR hold_until > $now) AND period && tstzrange($pickup, $return, '[)')`. Index `USING gist (vehicle_model_id, period) WHERE holds_vehicle`.
  - `holds_vehicle = true` ở `requested`, `awaiting_payment`, `confirmed`, `in_service`; `false` ở mọi trạng thái khác. Cột đổi **trong cùng transaction** đổi trạng thái.
  - `hold_until` = `response_due_at` khi `requested`; = `hold_release_at` (= `payment_due_at` + grace) khi `awaiting_payment`; `NULL` khi `confirmed`/`in_service`. Tiền về trễ được xác nhận lại thì đặt lại `holds_vehicle = true`, `hold_until = NULL`.
  - Khoảng nửa mở `[)`, **không có thời gian đệm giữa hai lượt thuê** (DB doc không có; mặc định B3): trả 10:00 thì lượt sau nhận 10:00 được.
  - Xe giữ **từ lúc `requested`** (spec §6.2 [Builder]), nên đối tác không bao giờ chấp nhận được hai đơn cho cùng một chiếc.
- **Số lượng (spec §6.1, §7):** `rental_vehicle_models.quantity int CHECK (quantity >= 0)`, `price_per_day bigint CHECK (price_per_day > 0)`. Đối tác không giảm `quantity` xuống dưới **đỉnh sử dụng cùng lúc trong [now, ∞)** (`RentalPeakHeld` với `to = nil`), lỗi 409 `quantity_below_held`.
- **Ngày thuê (spec §5.8, quyết định 2 đã chốt):** `days = ceil((return_at − pickup_at) / 24h)`, tối thiểu 1, tối đa **30**. 24h00 = 1 ngày, 24h01 = 2 ngày. Giờ nhận trong vòng **180 ngày** tới.
- **Điểm nhận (quyết định 1 đã chốt):** khách bắt buộc chọn một điểm nhận đang hoạt động trong danh sách của đối tác; mỗi điểm có `delivery_fee` riêng, tính **một lần**; trả xe tại chính điểm đó; không có địa chỉ giao tự do.
- **Trả sớm/trả muộn (quyết định 3 đã chốt):** trả sớm không hoàn tiền, xe trống lại ngay khi đơn rời `in_service`; trả muộn do cửa hàng xử lý, LunaIn không can thiệp. Giờ nhận và giờ trả phải tròn phút và nằm trong giờ giao nhận của cửa hàng tính theo giờ VN, hai biên đều tính (mặc định B2).
- **Giá:** `gross = price_per_day × days + delivery_fee` của điểm nhận đã chọn. `booking_items`: một dòng `rental_day` (`unit_price = price_per_day`, `quantity = days`) và, nếu phí > 0, một dòng `delivery_fee` (`quantity = 1`). Hoa hồng tính trên toàn bộ `gross`. Không hoàn khi trả xe sớm; trả trễ do cửa hàng thu ngoài sàn (quyết định 3).
- **Hạn của Request Booking (spec §6.2, §7, DB §4), settings đã có từ `00005`:**
  - `request_response_hours` mặc định **12**, `request_payment_hours` mặc định **2**, `hold_grace_minutes` mặc định **5**, `release_delay_days` **1**. **Không thêm setting mới.**
  - Tạo đơn: `pickup_at ≥ now + request_payment_hours + 1h` (hằng `minPartnerResponse = time.Hour`), nếu không thì 422 `pickup_too_soon`. `response_due_at = min(now + request_response_hours, pickup_at − request_payment_hours)` (mặc định B1).
  - Chấp nhận: `payment_due_at = hold_expires_at = accepted_at + request_payment_hours`, `hold_release_at = payment_due_at + hold_grace_minutes`. Link PayOS hết hạn đúng `payment_due_at`.
  - Đối tác chỉ từ chối ở `requested`. Đã chấp nhận thì **không** có đường nào cho đối tác rút lại hay hủy.
  - Khách rút miễn phí ở `requested` hoặc `awaiting_payment`: `cancelled`, `cancel_kind = customer_withdraw`, nhả xe ngay, không refund, không bút toán.
- **Mốc thời gian (DB §6):** `service_start_at = original_service_start_at = pickup_at`, `service_end_at = return_at` (giờ trả theo lịch). Giao xe (quét QR) chuyển `confirmed → in_service` trong khoảng `[pickup_at − 2h, return_at)` (mặc định B2). Đối tác bấm "đã nhận lại xe" chuyển `in_service → completed` bất kỳ lúc nào (sớm được). Đối tác bấm No-show khi `confirmed` và `now ≥ pickup_at`.
- **Tự kết thúc (quyết định 4 đã chốt), job `FinishRentals` mỗi phút:**
  - `in_service` mà `now ≥ service_end_at + 24h` (hằng `rentalAutoCompleteAfter = 24 * time.Hour`) → `completed` (actor system).
  - `confirmed` (chưa giao xe) mà `now ≥ service_end_at` → `no_show` (actor system), tiền xử lý như No-show.
  - Cả hai: `holds_vehicle = false`, `scheduleRelease(now)` (release `release_delay_days` sau). Đối tác bấm trước thì thắng (job kiểm tra lại trạng thái dưới khóa). Index dùng `bookings_service_end_idx` (đã có ở `00012`, `WHERE status IN ('confirmed','checked_in','in_service')`).
  - Release của `completed`/`no_show` do đối tác bấm cũng là `release_delay_days` sau lúc bấm.
- **Ràng buộc DB (DB §1.2, §5.7, §6):** `UNIQUE(id, listing_id)` trên `rental_vehicle_models` và `rental_pickup_points`. `booking_rental_details`: FK kép `(booking_id, listing_id) → bookings`, `(vehicle_model_id, listing_id) → rental_vehicle_models`, `(pickup_point_id, listing_id) → rental_pickup_points`. `bookings_instant_only_status_check` cấm `checked_in` ở Request Booking, nên thuê xe dùng `in_service`.
- **Không đổi lịch (spec §6.4):** thuê xe luôn `can_reschedule = false`; `POST /reschedule` trả 409 `reschedule_not_allowed` (đã có trong `rescheduleBlock`).
- **`visible_listings`:** plan này không thêm cột vào `listings`, view giữ nguyên. Vị trí cửa hàng là `listings.province_code/ward_code/address`.
- **Thông báo:** chỉ email qua outbox (`mailer.EnqueueTx`). Email đối tác gửi tới `partner.OwnerEmail`.
- **Commit:** không thêm trailer `Co-Authored-By` hay bất kỳ dòng ghi công AI nào.

## Review Focus

1. **Hai khách giành chiếc xe cuối:** Vios `quantity = 1`, hai `CreateRental` chạy song song với khoảng giao nhau. Kỳ vọng đúng một 201 (`requested`) và một 409 `vehicle_unavailable`, đúng một dòng `holds_vehicle = true`. Vì xe giữ từ `requested`, đối tác không bao giờ thấy hai yêu cầu cùng chiếm một chiếc để mà chấp nhận cả hai. Test `TestCreateRental_LastUnitRace` ở Task 5.
2. **Đối tác giảm số lượng khi còn yêu cầu đang chờ:** Vision `quantity = 2`, R1 `requested` [07/11 08:00, 09/11 08:00) và R2 `awaiting_payment` [08/11 08:00, 10/11 08:00) → đỉnh 2, giảm còn 1 thì 409 `quantity_below_held`. Nếu R2 là [09/11 08:00, 11/11 08:00) (nối tiếp) thì đỉnh 1, giảm còn 1 được. Yêu cầu đã quá `response_due_at` mà job chưa quét không tính. Test `TestUpdateRentalModel_QuantityVsPeakHolds` ở Task 3.
3. **Tiền về trễ khi xe đã vào tay đơn khác:** R chấp nhận lúc 10:00, hạn trả 12:00, nhả 12:05. 12:06 khách khác gửi yêu cầu trùng khoảng trên cùng Vios → 201. 12:10 tiền của R về → R `expired`, refund `late_payment` 2.500.000 `awaiting_bank_info`, không có `ledger_entries`, hold của đơn kia giữ nguyên. Nếu không có ai chen vào thì R được xác nhận lại, `holds_vehicle = true`, `hold_until = NULL`, pending đối tác 2.250.000. Test `TestRentalLatePaid_*` ở Task 7.
4. **Khách rút đúng lúc đối tác chấp nhận:** chạy song song `AcceptRequest` và `CancelByCustomer` trên cùng đơn `requested`. Hai đường cùng khóa mẫu xe → checkout → booking nên luôn tuần tự. Kỳ vọng cuối cùng đơn `cancelled` (`customer_withdraw`), không còn payment `pending` (payment do accept tạo bị `cancelled`, link bị hủy sau commit), `holds_vehicle = false`; accept thua thì 409 `request_not_pending`. Tiền về sau đó bị hoàn đủ. Test `TestAcceptVsWithdraw_Race` ở Task 6.
5. **Trả xe sớm, trả muộn và lượt kế tiếp, kể cả qua nửa đêm/múi giờ:** Vios R1 [07/11 08:00, 09/11 10:00) đã giao (`in_service`). Yêu cầu [09/11 10:00, 10/11 10:00) → 201 (nối tiếp); [08/11 12:00, 09/11 10:00) → 409. Đối tác bấm nhận lại xe lúc 08/11 10:00 → R1 `completed`, `holds_vehicle = false`, yêu cầu [08/11 12:00, 09/11 10:00) giờ → 201. R1 quên bấm và còn `in_service` sau `return_at` thì không chặn lượt sau `return_at`. Giờ gửi bằng UTC (`2026-11-07T15:00:00Z`) được hiểu là 22:00 VN khi so giờ giao nhận. Test ở Task 5 và Task 8.

---

## Cấu trúc file

```
internal/platform/db/migrations/00018_rental.sql          # rental_stores, rental_vehicle_models, rental_pickup_points, booking_rental_details, CHECK bookings
internal/listing/{rental.go,rental_models.go,rental_search.go,http_rental.go,deps.go,errors.go,review.go,public.go,http_public.go,queries.sql}
internal/listing/{rental_test.go,rental_models_test.go,rental_search_test.go}
internal/booking/{capacity.go,request.go,create_rental.go,rental_lifecycle.go,guard.go}
internal/booking/{create.go,confirm.go,cancel.go,checkin.go,jobs.go,ride_seats.go,views.go,http_*.go,queries.sql}
internal/booking/{rental_helpers_test.go,create_rental_test.go,request_test.go,confirm_rental_test.go,rental_lifecycle_test.go,schema_test.go,guard_test.go}
cmd/api/{main.go,routes.go,routes_test.go,e2e_rental_test.go}
docs/superpowers/specs/2026-11-04-database-schema.md (§1.1, §5.7, §6), GLOSSARY.md (No-show), docs/superpowers/plans/README.md
```

---

### Task 1: Schema thuê xe

**Files:**
- Create: `internal/platform/db/migrations/00018_rental.sql` (cập nhật `CHECKSUMS` bằng `make migrations-sum`)
- Modify: `internal/listing/queries.sql`, `internal/booking/queries.sql` (mỗi bên tối thiểu một query chạm bảng mới để sqlc sinh model), `internal/booking/schema_test.go`

**Schema** (đúng DB §5.7 và §6, kèm các phần bổ sung):

- **`rental_stores`:** `listing_id` PK FK `listings`, `handover_open time`, `handover_close time`, `CHECK (handover_close > handover_open)` (không có khung qua đêm), `delivery_available bool NOT NULL DEFAULT false`, `deposit_terms`, `documents_terms`, `late_fee_terms`, `allowed_area_terms`, `other_terms` (text NULL).
- **`rental_vehicle_models`:** `id`, `listing_id` FK `rental_stores(listing_id)` (phải có cửa hàng trước), `vehicle_kind CHECK IN ('car','motorbike')`, `name` (`btrim <> ''`), `model_year smallint CHECK BETWEEN 1950 AND 2100`, `seats int CHECK BETWEEN 1 AND 50`, `included_items text NULL`, `tags text[] NOT NULL DEFAULT '{}'` `CHECK (cardinality(tags) <= 10)`, `price_per_day bigint CHECK > 0`, `quantity int CHECK >= 0`, `cover_file_id uuid NULL FK files`, `active bool NOT NULL DEFAULT true`, `UNIQUE(id, listing_id)`, index `(listing_id)`.
- **`rental_pickup_points`:** `id`, `listing_id` FK `rental_stores(listing_id)`, `name`, `address` (`btrim <> ''`), **bổ sung** `province_code` + `ward_code` với FK `(ward_code, province_code) → wards(code, province_code)`, `delivery_fee bigint NOT NULL DEFAULT 0 CHECK >= 0`, `active bool`, `UNIQUE(id, listing_id)`.
- **`booking_rental_details`:**
  - `booking_id` PK, `listing_id`, `vehicle_model_id`, `pickup_point_id` **NOT NULL** (quyết định 1), `pickup_at`, `return_at` timestamptz, `CHECK (return_at > pickup_at)`, `period tstzrange GENERATED ALWAYS AS (tstzrange(pickup_at, return_at, '[)')) STORED`, `days int CHECK > 0`.
  - **Bổ sung (snapshot lúc đặt):** `model_name`, `pickup_point_name`, `pickup_address` text NOT NULL; `destination_province_code text NOT NULL FK provinces` (mặc định B4).
  - `holds_vehicle bool NOT NULL`, `hold_until timestamptz NULL`, `CHECK (holds_vehicle OR hold_until IS NULL)`.
  - FK kép như Global Constraints. `CREATE INDEX booking_rental_details_hold_idx ON booking_rental_details USING gist (vehicle_model_id, period) WHERE holds_vehicle`.
  - Không có cột `delivery_address` (quyết định 1). Job `FinishRentals` dùng `bookings_service_end_idx` đã có, không cần index mới.
- **`bookings`:**
  - `bookings_in_service_at_check CHECK (status <> 'in_service' OR in_service_at IS NOT NULL)`.
  - `bookings_accepted_check CHECK (status <> 'awaiting_payment' OR (accepted_at IS NOT NULL AND payment_due_at IS NOT NULL))`.
  - `bookings_reject_reason_check CHECK (status <> 'rejected' OR reject_reason IS NOT NULL)`.
- Down: drop theo thứ tự ngược lại.

- [ ] **Step 1: Viết test fail** trong `schema_test.go`, chèn trực tiếp bằng SQL:
  - `TestRentalModelQuantityCheck`: `quantity = -1` thì 23514, `price_per_day = 0` thì 23514.
  - `TestRentalDetailModelMustMatchListing`: `vehicle_model_id` của listing khác thì 23503; tương tự `pickup_point_id` (`TestRentalDetailPointMustMatchListing`).
  - `TestRentalDetailPeriodGenerated`: chèn pickup 08:00, return 10:00 hôm sau; `SELECT period && tstzrange('...10:00','...11:00','[)')` là false (nửa mở), `&& tstzrange('...09:59', ...)` là true.
  - `TestRentalDetailHoldUntilNeedsHolds`: `holds_vehicle = false, hold_until = now` thì 23514.
  - `TestRentalDetailReturnAfterPickup`: `return_at = pickup_at` thì 23514.
  - `TestBookingInServiceNeedsTimestamp`, `TestBookingAwaitingNeedsAcceptance`, `TestBookingRejectedNeedsReason`: 23514.
  - `TestRentalStoreHandoverOrder`: `handover_close = handover_open` thì 23514.
- [ ] **Step 2: Chạy** `go test ./internal/booking -run 'TestRental|TestBookingInService|TestBookingAwaiting|TestBookingRejected'`. Kỳ vọng FAIL.
- [ ] **Step 3: Viết migration**, rồi chạy `make migrations-sum` và `make sqlc`. Cột `period` ra `pgtype.Range[pgtype.Timestamptz]`; query không bao giờ ghi nó.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...` (gồm `TestMigrationChecksums`).
- [ ] **Step 5: Commit** `feat(db): self-drive rental schema`.

---

### Task 2: Cửa hàng, điểm nhận xe, điều kiện gửi duyệt

**Files:**
- Create: `internal/listing/{rental.go,rental_test.go,http_rental.go}`
- Modify: `internal/listing/{errors.go,review.go,public.go,http_partner.go,queries.sql}`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: `LocationValidator.ValidateLocation`, `lockOwned`, `requireActivePartner`, `logChange`, `fieldDiff`, `withMessage`, `hhmm`, `clockText`, `pgTime`, `minutes` (đã có trong `listing`).
- Produces:
  - `const rentalServiceType = "self_drive_rental"`. `supportedServiceType` trả `true` cho loại này.
  - `ErrNotRentalListing` (422 `wrong_service_type`, "Thao tác này chỉ áp dụng cho Listing thuê xe tự lái"), `ErrInvalidRental` (422 `invalid_rental`), `ErrStoreRequired` (422 `rental_store_required`, "Cần khai báo thông tin cửa hàng trước"), `ErrPickupPointNotFound` (404 `not_found`).
  - `type RentalStoreInput struct { HandoverOpen, HandoverClose string /*"HH:MM"*/; DeliveryAvailable bool; DepositTerms, DocumentsTerms, LateFeeTerms, AllowedAreaTerms, OtherTerms *string }` (json snake_case). Mỗi điều khoản tối đa 2000 rune, chuỗi rỗng sau trim thành `nil`. `HandoverClose > HandoverOpen`.
  - `type RentalStore struct { ListingID uuid.UUID; RentalStoreInput }` và `func (st RentalStore) CoversHandover(t time.Time) bool`: giờ:phút VN của `t` nằm trong `[open, close]`, hai biên tính.
  - `type PickupPoint struct { ID, ListingID uuid.UUID; Name, Address, ProvinceCode, WardCode string; DeliveryFee int64; Active bool }`.
  - `type PickupPointInput struct { Name, Address, ProvinceCode, WardCode string; DeliveryFee int64 }`: tên 1..200 rune, địa chỉ 1..500, `ValidateLocation`, phí 0..5.000.000.
  - `type PickupPointPatch struct { Name, Address *string; DeliveryFee *int64; Active *bool }`. Có `province_code`/`ward_code` trong body thì 422: muốn đổi chỗ thì tạo điểm mới.
  - `type RentalDetail struct { Store *RentalStore; PickupPoints []PickupPoint }` (Task 3 thêm trường `Models []RentalModel`).
  - `func (s *Service) SetRentalStore(ctx, partnerID, actor, listingID uuid.UUID, in RentalStoreInput) (RentalStore, error)`: upsert, audit `rental_store`. Đổi giờ giao nhận không ảnh hưởng đơn đã có.
  - `func (s *Service) GetRental(ctx, partnerID, listingID uuid.UUID) (RentalDetail, error)` và `func (s *Service) RentalDetail(ctx, tx pgx.Tx, listingID uuid.UUID, includeInactive bool) (RentalDetail, error)`.
  - `func (s *Service) CreatePickupPoint(ctx, partnerID, actor, listingID uuid.UUID, in PickupPointInput) (PickupPoint, error)` (chưa có cửa hàng thì `ErrStoreRequired`) và `func (s *Service) UpdatePickupPoint(ctx, partnerID, actor, listingID, pointID uuid.UUID, in PickupPointPatch) (PickupPoint, error)`. Audit `rental_pickup_point`. Tắt điểm không ảnh hưởng đơn đã có (đơn chụp lại tên và địa chỉ).
  - Cho `booking`: `func (s *Service) RentalStoreOf(ctx, tx pgx.Tx, listingID uuid.UUID) (RentalStore, error)` và `func (s *Service) PickupPointForBooking(ctx, tx pgx.Tx, listingID, pointID uuid.UUID) (PickupPoint, error)` (điểm phải `active` và thuộc listing, nếu không thì `ErrPickupPointNotFound`).
  - `requireComplete` cho `self_drive_rental`: **giữ** ba mục vị trí của listing (đó là địa chỉ cửa hàng), thêm "thông tin cửa hàng (giờ giao nhận)", "ít nhất 1 mẫu xe đang cho thuê" (`active AND quantity > 0`), "ít nhất 1 điểm nhận xe đang hoạt động".
  - Routes đối tác (`withID`, listing phải là `self_drive_rental`, nếu không thì `ErrNotRentalListing`), đăng ký qua `s.rentalRoutes(mux, withID)`:
    - `GET /v1/partner/listings/{id}/rental`
    - `PUT /v1/partner/listings/{id}/rental-store`
    - `POST /v1/partner/listings/{id}/rental-pickup-points`
    - `PATCH /v1/partner/listings/{id}/rental-pickup-points/{pointID}`

- [ ] **Step 1: Viết test fail** `rental_test.go`. Clock cố định `2026-11-05T09:00:00+07:00`. Listing "Thuê xe Đà Lạt" ở Lâm Đồng (68).
  - `TestSetRentalStore_UpsertAndAudit`: lần 2 đổi `handover_close` 22:00 → 23:00, audit có diff `handover_close`.
  - `TestSetRentalStore_BadHours422`: `"25:00"`, close ≤ open.
  - `TestSetRentalStore_OnRide422`: `wrong_service_type`.
  - `TestRentalStore_CoversHandover`: 06:00–22:00; `2026-11-07T15:00:00Z` (22:00 VN) true, 22:01 VN false, 05:59 VN false.
  - `TestCreatePickupPoint_NeedsStore422`, `TestCreatePickupPoint_WardNotInProvince422`, `TestCreatePickupPoint_FeeBounds` (−1 và 5.000.001 thì 422).
  - `TestUpdatePickupPoint_LocationImmutable422` và `TestUpdatePickupPoint_DeactivateOK`.
  - `TestPickupPointForBooking_InactiveOrOtherListing404`.
  - `TestSubmit_RentalCompleteness`: chưa có gì thì message liệt kê đúng ba mục cửa hàng/mẫu xe/điểm nhận (cùng các mục chung còn thiếu). Mẫu xe `quantity = 0` không tính.
  - HTTP: `TestHTTP_RentalRoutes_MalformedID404`, `TestHTTP_RentalStore_UnknownField400`, `TestHTTP_PickupPoint_OtherPartner404`.
- [ ] **Step 2: Chạy** `go test ./internal/listing -run 'Rental|PickupPoint|Submit_Rental'`. Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Thêm 4 route vào `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(listing): rental store, pickup points and submit completeness`.

---

### Task 3: Mẫu xe, chặn giảm số lượng, hàm giữ xe cho booking

**Files:**
- Create: `internal/listing/{rental_models.go,rental_models_test.go}`
- Modify: `internal/listing/{deps.go,rental.go,http_rental.go,review.go,queries.sql}`, `internal/booking/{guard.go,guard_test.go,queries.sql}`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: Task 2; `FileChecker.Owned(…, "listing_image")`.
- Produces:
  - `BookingGuard` thêm **một** hàm (`booking.Guard` cài đặt, đọc `booking_rental_details` bằng `tx` của người gọi):
    - `RentalPeakHeld(ctx, tx pgx.Tx, modelIDs []uuid.UUID, from time.Time, to *time.Time, now time.Time) (map[uuid.UUID]int, error)`: đỉnh số đơn đang giữ xe cùng lúc (điều kiện ở Global Constraints) trong `[from, to)`, `to = nil` là vô hạn; mẫu không có đơn thì vắng mặt (= 0). Gọi `peakRentalHolds` bên dưới.
    - Booking dùng nội bộ `func peakRentalHolds(ctx, q *store.Queries, modelIDs []uuid.UUID, from time.Time, to *time.Time, now time.Time, except uuid.UUID) (map[uuid.UUID]int, error)`. Cắt mỗi khoảng đang giữ theo cửa sổ; đỉnh luôn rơi vào điểm đầu của một khoảng đã cắt, nên query `PeakRentalHolds`:
      ```sql
      WITH win AS (SELECT tstzrange(@from::timestamptz, sqlc.narg(to)::timestamptz, '[)') AS r),
      held AS (
        SELECT d.vehicle_model_id AS m, d.period * win.r AS p
        FROM booking_rental_details d, win
        WHERE d.vehicle_model_id = ANY(@model_ids::uuid[]) AND d.holds_vehicle
          AND (d.hold_until IS NULL OR d.hold_until > @now) AND d.booking_id <> @except
          AND d.period && win.r)
      SELECT h1.m AS vehicle_model_id,
             max((SELECT count(*) FROM held h2 WHERE h2.m = h1.m AND h2.p @> lower(h1.p)))::int AS peak
      FROM held h1 GROUP BY h1.m;
      ```
    - Deps `BookingGuard == nil` (test không có booking) thì coi như 0.
  - `type RentalModel struct { ID, ListingID uuid.UUID; VehicleKind, Name string; ModelYear, Seats int; IncludedItems *string; Tags []string; PricePerDay int64; Quantity int; CoverFileID *uuid.UUID; Active bool }`.
  - `type RentalModelInput struct { VehicleKind, Name string; ModelYear, Seats int; IncludedItems *string; Tags []string; PricePerDay int64; Quantity int; CoverFileID *uuid.UUID }`: kind `car|motorbike`, tên 1..200, năm 1990..(năm VN hiện tại + 1), chỗ 1..50, đồ kèm ≤ 1000 rune, tối đa 10 nhãn mỗi nhãn 1..50 rune (trim, bỏ trùng), giá 10.000..100.000.000, số lượng 0..1000, ảnh phải là file `listing_image` của chính người gọi.
  - `type RentalModelPatch struct { Name *string; ModelYear, Seats *int; IncludedItems *string; Tags *[]string; PricePerDay *int64; Quantity *int; CoverFileID *uuid.UUID; Active *bool }`. `vehicle_kind` trong body thì 422. Đổi giá chỉ áp cho yêu cầu mới (đơn đã có chụp giá).
  - `ErrRentalModelNotFound` (404 `not_found`), `ErrQuantityBelowHeld` (409 `quantity_below_held`, "Số lượng mới nhỏ hơn số xe đang được giữ cho các đơn").
  - `func (s *Service) CreateRentalModel(ctx, partnerID, actor, listingID uuid.UUID, in RentalModelInput) (RentalModel, error)` (chưa có cửa hàng thì `ErrStoreRequired`).
  - `func (s *Service) UpdateRentalModel(ctx, partnerID, actor, listingID, modelID uuid.UUID, in RentalModelPatch) (RentalModel, error)`: khóa partner `FOR SHARE` → listing `FOR NO KEY UPDATE` → mẫu xe `FOR UPDATE`. Khi `Quantity` giảm: `RentalPeakHeld([model], now, nil, now)[model] > quantity mới` thì `ErrQuantityBelowHeld`. Audit `rental_model`. Tắt mẫu (`active=false`) luôn được: chỉ chặn yêu cầu mới.
  - Cho `booking` (nhận `tx` của người gọi):
    - `func (s *Service) RentalModelListingID(ctx, tx pgx.Tx, modelID uuid.UUID) (uuid.UUID, error)`: đọc không khóa, không có thì `ErrRentalModelNotFound`.
    - `func (s *Service) LockRentalModels(ctx, tx pgx.Tx, ids []uuid.UUID) ([]RentalModel, error)`: `FOR UPDATE ORDER BY id`, tự loại trùng, thiếu id thì `ErrRentalModelNotFound`.
  - `RentalDetail` có thêm `Models []RentalModel` (json `models`). `AdminGet` của listing có thêm `Rental *RentalDetail`.
  - Routes đối tác: `POST /v1/partner/listings/{id}/rental-models`, `PATCH /v1/partner/listings/{id}/rental-models/{modelID}`.

- [ ] **Step 1: Viết test fail** `rental_models_test.go` (dùng `booking.Guard{}` thật và chèn `booking_rental_details` bằng SQL qua helper):
  - `TestCreateRentalModel_Validation422`: kind `truck`, năm 1989, 11 nhãn, giá 9.999.
  - `TestCreateRentalModel_CoverOfOtherUser422`.
  - `TestUpdateRentalModel_QuantityVsPeakHolds` (Review Focus 2): đủ ba biến thể ở mục Review Focus, cộng biến thể R1 đã quá `response_due_at` → giảm còn 1 được.
  - `TestUpdateRentalModel_CompletedAndPastHoldsIgnored`: đơn `holds_vehicle=false`, hoặc `return_at ≤ now`, không tính vào đỉnh.
  - `TestUpdateRentalModel_KindImmutable422`, `TestUpdateRentalModel_OtherPartner404`, `TestSuspendedPartnerCannotEditModels` (403 `partner_suspended`).
  - `TestLockRentalModels_OrderedAndDeduped`.
- [ ] **Step 2: Viết test fail** `guard_test.go` (Vision `quantity = 2`, T2 = 2026-11-09 08:00):
  - `TestGuard_RentalPeakHeld_DisjointHolds`: A [09/11 08:00, 10/11 08:00), B [11/11 08:00, 12/11 08:00); cửa sổ [09/11 08:00, 12/11 08:00) → `peak = 1`.
  - `TestGuard_RentalPeakHeld_OverlappingHolds`: A [09/11 08:00, 11/11 08:00), B [10/11 08:00, 12/11 08:00); cửa sổ [10/11 08:00, 11/11 08:00) → `peak = 2`.
  - `TestGuard_RentalPeakHeld_Edges`: khoảng nối tiếp không tính; đơn `requested` quá `hold_until` không tính; `except` loại chính đơn; `to = nil` với ba đơn [1,3), [2,4), [3,5) ngày → 2; hai mẫu trả hai số.
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt.** Thêm 2 route vào `partnerRoutes`.
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(listing): rental vehicle models with held-quantity guard`.

---

### Task 4: Tìm xe còn trống public và trang chi tiết

**Files:**
- Create: `internal/listing/{rental_search.go,rental_search_test.go}`
- Modify: `internal/listing/{rental.go,public.go,http_public.go,queries.sql}`

**Interfaces:**
- Consumes: Task 2–3 (`RentalPeakHeld`).
- Produces:
  - Quy tắc khoảng thuê dùng chung cho tìm kiếm (Task 4) và tạo đơn (Task 5), trong `rental.go`:
    - `const maxRentalDays = 30`, `const rentalHorizonDays = 180`, `const minPartnerResponse = time.Hour`.
    - `func RentalDays(pickup, ret time.Time) int`: `ceil((ret − pickup) / 24h)`, tối thiểu 1.
    - `ErrInvalidRentalPeriod` (422 `invalid_rental_period`), `ErrPickupTooSoon` (422 `pickup_too_soon`), `ErrOutsideHandoverHours` (422 `outside_handover_hours`).
    - `func CheckRentalPeriod(pickup, ret, now time.Time, paymentHours int) (int, error)`: tròn phút, `ret > pickup`, `RentalDays ≤ 30`, `pickup ≤ now + 180 ngày` (sai thì `ErrInvalidRentalPeriod`); `pickup ≥ now + paymentHours·h + minPartnerResponse` (sai thì `ErrPickupTooSoon`). Trả về `days`.
  - `type RentalSearchFilter struct { ProvinceCode string; PickupAt, ReturnAt time.Time; VehicleKind *string; MinSeats, Limit, Offset int }`.
  - `type RentalVehicleCard struct { ModelID uuid.UUID; Listing ListingRef /*ID, Slug, Title*/; VehicleKind, Name string; ModelYear, Seats int; Tags []string; CoverURL *string; PricePerDay int64; Days int; RentalTotal int64 /*price × days, chưa gồm phí giao*/; UnitsLeft int; StoreAddress string; HandoverOpen, HandoverClose string }`.
  - `func (s *Service) SearchRentalVehicles(ctx context.Context, f RentalSearchFilter) ([]RentalVehicleCard, error)`:
    - Tỉnh bắt buộc; `CheckRentalPeriod` với `request_payment_hours` đọc từ `platform_settings` trong cùng câu lệnh (như tìm chuyến của plan 6); `MinSeats` 0..50; kind `car|motorbike`; limit mặc định 20 tối đa 50; offset ≤ 1000. Sai thì `httpx.ErrInvalidRequest` (400).
    - Ứng viên (`ListRentalCandidates`, tối đa `maxRentalCandidates = 1000`): listing trong `visible_listings`, `service_type = 'self_drive_rental'`, `province_code = f.ProvinceCode`, mẫu `active AND quantity > 0`, `seats ≥ MinSeats`, giờ VN của cả `PickupAt` và `ReturnAt` trong giờ giao nhận của cửa hàng.
    - `RentalPeakHeld(ứng viên, PickupAt, &ReturnAt, now)`; giữ mẫu có `UnitsLeft = quantity − peak ≥ 1`; sắp `price_per_day`, rồi `model id`; áp offset/limit sau khi lọc.
  - `price_from` của thuê xe trong `SearchPublicListings`/`GetPublicListingBySlug` là `min(price_per_day)` của các mẫu active có `quantity > 0`.
  - `PublicDetail` có thêm `Rental *RentalDetail` (chỉ mẫu và điểm active; điều khoản thuê hiển thị nguyên văn). Nhánh `self_drive_rental` trong `switch` không còn `ErrServiceTypeNotSupported`.
  - Route public: `GET /v1/rental-vehicles?province=&pickup_at=&return_at=&vehicle_kind=&min_seats=&limit=&offset=` (`pickup_at`/`return_at` RFC 3339), header `Cache-Control: no-store` (kết quả phụ thuộc giờ hiện tại và số xe).

- [ ] **Step 1: Viết test fail** `rental_search_test.go`. Now 2026-11-05 09:00, cửa hàng 06:00–22:00, Vios (`car`, 5 chỗ, 800.000, qty 1), Vision (`motorbike`, 2 chỗ, 150.000, qty 2).
  - `TestRentalDays`: 24h → 1, 24h01 → 2, 1h → 1, 48h → 2, 50h → 3.
  - `TestCheckRentalPeriod`: pickup 11:59 → `pickup_too_soon`, 12:00 → OK; 30 ngày OK, 30 ngày + 1 phút → 422; có giây → 422; pickup sau 180 ngày → 422.
  - `TestSearchRental_UnitsLeftByOverlap`: Vision có một đơn giữ trùng → `units_left = 1`; Vios có một đơn giữ trùng → không hiện; đơn nối tiếp `[return, …)` không trừ.
  - `TestSearchRental_PriceAndDays`: Vios 07/11 08:00 → 09/11 10:00 → `days = 3`, `rental_total = 2.400.000`; sắp Vision trước Vios.
  - `TestSearchRental_HandoverHours` (Review Focus 5): pickup `2026-11-07T15:00:00Z` (22:00 VN) hiện; 22:30 VN không hiện.
  - `TestSearchRental_HidesInvisibleAndInactive`: đối tác bị đình chỉ, listing ẩn, loại dịch vụ tắt, mẫu `active=false` hoặc `quantity=0`.
  - `TestSearchRental_KindAndSeatsFilter`, `TestSearchRental_BadParams400` (thiếu tỉnh, `return_at ≤ pickup_at`, `vehicle_kind=truck`).
  - `TestPublicDetail_Rental` và `TestSearch_RentalPriceFrom` (= 150.000).
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(listing): public rental vehicle availability search and detail`.

---

### Task 5: Gửi yêu cầu thuê (Request Booking chung + đơn thuê xe), các màn xem

**Files:**
- Create: `internal/booking/{capacity.go,request.go,create_rental.go,rental_helpers_test.go,create_rental_test.go}`
- Modify: `internal/booking/{create.go,ride_seats.go,jobs.go,cancel.go,confirm.go,views.go,http_customer.go,queries.sql}`, `cmd/api/routes_test.go` (danh sách route khách thêm `POST /v1/bookings/rental`)

**Interfaces:**
- Consumes: `listing.BookableListing`, `RentalModelListingID`, `LockRentalModels`, `RentalStoreOf`, `PickupPointForBooking`, `CheckRentalPeriod`, `CancellationPolicy`; `catalog.CurrentCommissionRate`; `settings.GetTx`; `requestHash`, `replayed`, `storeIdempotentResponse`, `insertCheckout`, `normalizeContact`, `randomToken`.
- Produces:
  - Hằng: `StatusRequested = "requested"`, `StatusAwaitingPayment = "awaiting_payment"`, `StatusInService = "in_service"`, `StatusRejected = "rejected"`, `bookingModeRequest = "request"`, `serviceTypeRental = "self_drive_rental"`, `lineKindRentalDay = "rental_day"`, `lineKindDeliveryFee = "delivery_fee"`, `endpointRental = "POST /v1/bookings/rental"`.
  - Deps có thêm `Partners PartnerContact` với `type PartnerContact interface { OwnerEmail(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID) (string, error) }` (cài bởi `*partner.Service`; `nil` thì chỉ log).
  - `capacity.go` (gom luật sức chứa theo loại):
    - `func (s *Service) lockCheckoutCapacity(ctx, tx pgx.Tx, checkoutID uuid.UUID) error`: khóa trip (`ListCheckoutRideTrips`) rồi mẫu xe (`ListCheckoutRentalModels`), đọc không khóa. **Thay** `lockCheckoutTrips` ở mọi chỗ gọi (`expireCheckout`, …).
    - `func (s *Service) releaseHold(ctx, tx pgx.Tx, b store.Booking) error`: xe tuyến thì `releaseRideSeats`; thuê xe thì `setRentalHold(false, nil)`; loại khác không làm gì. **Thay** `releaseRideSeats` trong `expireCheckout`, `confirmation.expireBooking`, `cancelLocked`.
    - `func (s *Service) setRentalHold(ctx, tx pgx.Tx, bookingID uuid.UUID, holds bool, until *time.Time) error` (query `SetRentalHold`).
    - Đếm xe dùng `peakRentalHolds` (Task 3).
  - `request.go` (phần chung, plan 8 dùng lại):
    - `func requestResponseDue(cfg settings.Settings, now, serviceStart time.Time) time.Time` = `min(now + request_response_hours, serviceStart − request_payment_hours)`.
    - `func insertRequestedBooking(ctx, q *store.Queries, p store.InsertRequestedBookingParams) (uuid.UUID, string, error)`: như `insertPendingBooking` nhưng `booking_mode = 'request'`, `status = 'requested'`, `response_due_at`, hold NULL.
    - `func (s *Service) emailPartner(ctx, tx pgx.Tx, partnerID uuid.UUID, subject, text string) error`.
  - `type CreateRentalInput struct { ListingID, VehicleModelID, PickupPointID uuid.UUID; PickupAt, ReturnAt time.Time; DestinationProvinceCode string; ContactName, ContactPhone, ContactEmail string; Note *string }` (json snake_case).
  - `ErrVehicleUnavailable` (409 `vehicle_unavailable`, "Mẫu xe này đã hết xe trống trong khoảng thời gian đã chọn"), `ErrInvalidDestination` (422 `invalid_destination`).
  - `func (s *Service) CreateRental(ctx, customerID uuid.UUID, idemKey string, in CreateRentalInput) (BookingView, error)`. Trong **một** transaction, theo đúng thứ tự:
    1. Giữ Idempotency-Key, rồi `FillProfileIfEmpty`.
    2. `RentalModelListingID` phải bằng `in.ListingID`, nếu không thì `ErrRentalModelNotFound` (404). `BookableListing` phải là `self_drive_rental`, nếu không thì `ErrWrongServiceType`.
    3. `settings.GetTx`; `CheckRentalPeriod(pickup, return, now, request_payment_hours)` → `days`. `RentalStoreOf`; cả hai mốc phải `CoversHandover`, nếu không thì `ErrOutsideHandoverHours`. `PickupPointForBooking`. Tỉnh đến phải có trong `provinces`, nếu không thì `ErrInvalidDestination`.
    4. `LockRentalModels([model])`. Mẫu phải `active`, nếu không thì `ErrVehicleUnavailable`. `peak + 1 > quantity` với `peak = peakRentalHolds([model], pickup, &return, now, uuid.Nil)[model]` thì `ErrVehicleUnavailable`.
    5. Hoa hồng `self_drive_rental`, chính sách hủy. `gross = price_per_day × days + delivery_fee`. Insert checkout (`total = gross`), booking `requested` (`service_start_at = pickup_at`, `service_end_at = return_at`, `response_due_at = requestResponseDue`), `booking_items` (mô tả `"Thuê {model} {days} ngày"` và `"Phí giao xe tại {điểm}"`), `booking_rental_details` (`holds_vehicle = true`, `hold_until = response_due_at`, snapshot), history (actor customer).
    6. **Không** tạo payment. Email khách ("đã gửi yêu cầu, chờ đối tác xác nhận trước {response_due_at}") và email đối tác ("có yêu cầu thuê mới, hạn phản hồi …").
    - Response lưu với key là `createdRef`; replay trả `BookingView` đọc lại.
  - `ListCheckoutBookings` có thêm `LEFT JOIN booking_rental_details`, trả thêm `vehicle_model_id`, `rental_pickup_at`, `rental_return_at`.
  - Views:
    - `BookingView.Rental *RentalView` với `RentalView{VehicleModelID uuid.UUID; ModelName string; PickupPointID uuid.UUID; PickupPointName, PickupAddress string; PickupAt, ReturnAt time.Time; Days int; DestinationProvinceCode string}`.
    - `BookingView.Request *RequestView` (chỉ Request Booking) với `RequestView{ResponseDueAt, PaymentDueAt, AcceptedAt, RejectedAt *time.Time; RejectReason *string}`.
    - `Payment` hiện cho khách khi `pending_payment` **hoặc** `awaiting_payment`. `qr_token` hiện khi `confirmed`, `checked_in`, `in_service`, `completed`.
    - Thuê xe `can_reschedule = false`. `can_cancel` của khách: `true` ở `requested`/`awaiting_payment` (rút miễn phí), còn `confirmed` theo `cancelBlock`.
  - Route khách: `POST /v1/bookings/rental` (header `Idempotency-Key`, trả 201 `BookingView`).

- [ ] **Step 1: Viết helper** `rental_helpers_test.go`:
  - Đối tác có listing "Thuê xe Đà Lạt" (68) đã duyệt, cửa hàng 06:00–22:00, hoa hồng `self_drive_rental` 1000 bps, chính sách hủy `[{48,10000},{24,5000}]`.
  - Mẫu Vios (800.000/ngày, qty 1) và Vision (150.000/ngày, qty 2). Điểm P0 "Cửa hàng" phí 0, P1 "Sân bay Liên Khương" phí 100.000.
  - Helper `rentalInput(model, point uuid.UUID, pickup, ret time.Time)` (tỉnh đến 68), `createRental`, `rentalHold(t, bookingID) (holds bool, until *time.Time)`.
- [ ] **Step 2: Viết test fail** `create_rental_test.go` (now 2026-11-05 09:00):
  - `TestCreateRental_RequestedHoldsVehicle`: Vios P1 07/11 08:00 → 09/11 10:00. Kỳ vọng `status = requested`, `booking_mode = request`, `gross 2.500.000`, `commission 250.000`, hai item (`rental_day` 800.000 × 3, `delivery_fee` 100.000 × 1), `response_due_at = 2026-11-05 21:00+07`, `holds_vehicle = true`, `hold_until = response_due_at`, **không** có dòng `payments`, checkout `open`, có email cho khách và đối tác.
  - `TestCreateRental_ResponseDueCappedBeforePickup`: pickup 12:00 hôm nay → `response_due_at = 10:00`.
  - `TestCreateRental_PickupTooSoon422`: 11:59.
  - `TestCreateRental_LastUnitRace` (Review Focus 1).
  - `TestCreateRental_PeakAcceptsDisjointHolds` (quyết định 5): Vision qty 2, A [09/11 08:00, 10/11 08:00), B [11/11 08:00, 12/11 08:00) → yêu cầu [09/11 08:00, 12/11 08:00) → 201.
  - `TestCreateRental_PeakRefusesOverlappingHolds` (quyết định 5): A [09/11 08:00, 11/11 08:00), B [10/11 08:00, 12/11 08:00) → yêu cầu [10/11 08:00, 11/11 08:00) → 409 `vehicle_unavailable`.
  - `TestCreateRental_BackToBackAllowed` và `TestCreateRental_OverlapRejected409` (Review Focus 5, phần trước khi giao xe).
  - `TestCreateRental_IgnoresOverdueHolds`: đơn khác `requested` đã quá `response_due_at` (job chưa quét) không chặn.
  - `TestCreateRental_HandoverHours422` (Review Focus 5): 22:30 VN thì 422, `2026-11-07T15:00:00Z` thì 201.
  - `TestCreateRental_InactiveModelOrPoint`: mẫu tắt → 409 `vehicle_unavailable`; điểm tắt → 404.
  - `TestCreateRental_ModelOfOtherListing404`, `TestCreateRental_PartnerSuspended422` (`listing_not_bookable`, không dòng nào được giữ).
  - `TestCreateRental_Idempotent`: cùng key thì cùng booking; cùng key khác body thì 422.
  - `TestRentalView_RequestedHasNoPaymentNoQR`: `payment = null`, không `qr_token`, `can_cancel = true`, `can_reschedule = false`, `request.response_due_at` có.
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt.** Toàn bộ test Attraction và xe tuyến cũ phải vẫn PASS sau khi đổi sang `lockCheckoutCapacity`/`releaseHold`.
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(booking): rental requests holding a vehicle from request time`.

---

### Task 6: Đối tác chấp nhận/từ chối, khách rút yêu cầu, hết hạn phản hồi và hết hạn thanh toán

**Files:**
- Create: `internal/booking/request_test.go`
- Modify: `internal/booking/{request.go,capacity.go,cancel.go,create.go,jobs.go,views.go,http_partner.go,queries.sql}`, `cmd/api/{main.go,routes_test.go}` (đăng ký `ExpireRequestsWorker` ngay task này, vì `PeriodicJobs()` đã có kind mới)

**Interfaces:**
- Consumes: Task 5; `payment.ReservePayment`/`ActivateLink`/`LockCheckoutPayments`/`CancelPending`; `cancelLink`.
- Produces:
  - `ErrRequestNotPending` (409 `request_not_pending`, "Yêu cầu này không còn chờ đối tác phản hồi"), `ErrRequestExpired` (409 `request_expired`, "Đã quá hạn phản hồi yêu cầu"), `ErrInvalidReject` (422 `invalid_reject`).
  - `func (s *Service) AcceptRequest(ctx, partnerID, userID, bookingID uuid.UUID) (BookingView, error)`:
    - Đọc booking không khóa (khác đối tác → `ErrBookingNotFound`). Trong tx: `BookableListing` (partners, listings `FOR SHARE`; đối tác bị đình chỉ → `listing_not_bookable`), `lockCheckoutCapacity`, `LockCheckout`, `LockBooking`, kiểm tra lại: `status = requested` (không thì `ErrRequestNotPending`), `now < response_due_at` (không thì `ErrRequestExpired`).
    - Query `AcceptRequest`: `awaiting_payment`, `accepted_at = now`, `payment_due_at = hold_expires_at = now + request_payment_hours`, `hold_release_at = + hold_grace_minutes`. `setRentalHold(true, &hold_release_at)`. `ReservePayment(checkout, total, payment_due_at)`. History (actor partner). Email khách ("đối tác đã xác nhận, vui lòng thanh toán trước {payment_due_at}").
    - Sau commit: `ActivateLink` (lỗi provider chỉ log; khách bấm `POST /pay` để tạo lại).
  - `func (s *Service) RejectRequest(ctx, partnerID, userID, bookingID uuid.UUID, reason string) (BookingView, error)`: `reason` 1..500 rune (sai → `ErrInvalidReject`). Cùng thứ tự khóa (không cần `BookableListing`). Chỉ `requested` (không thì `ErrRequestNotPending`; quá `response_due_at` vẫn từ chối được nếu job chưa quét). `rejected`, `rejected_at`, `reject_reason`, `releaseHold`, checkout `closed` khi mọi booking đã kết thúc, history, email khách.
  - Khách rút: `CancelByCustomer` rẽ nhánh khi `booking_mode = request` và status `requested`/`awaiting_payment` sang `func (s *Service) withdrawRequest(ctx, customerID, bookingID uuid.UUID) error`:
    - Tx: `lockCheckoutCapacity` → `LockCheckout` → `LockBooking` → kiểm tra lại: phải còn `requested`/`awaiting_payment`, nếu không thì 409 `invalid_booking_state` (đơn vừa được thanh toán thì khách hủy lại theo chính sách).
    - `CancelBooking` (`cancel_kind = customer_withdraw`, `cancelled_by = customer`), history, `releaseHold`, mọi payment `pending` của checkout → `CancelPending` (gom order code), checkout `closed`, email đối tác ("khách đã rút yêu cầu").
    - Không refund, không bút toán. `bank` trong body bị bỏ qua. Sau commit hủy link.
  - `ExpireRequestsArgs{}` (Kind `booking_expire_requests`), `ExpireRequestsWorker{Service *Service}`, `func (s *Service) ExpireRequests(ctx context.Context) (int, error)`:
    - `ListRequestsToExpire(now, lim, skip)`: `status = 'requested' AND response_due_at <= now` (`bookings_response_due_idx`).
    - Mỗi checkout một tx: `lockCheckoutCapacity` → `LockCheckout` → `LockCheckoutBookings`, kiểm tra lại, `ExpireBooking`, history "đối tác không phản hồi đúng hạn" (actor system), `releaseHold`, checkout `closed`, email khách. Tracker `requestBackoff` riêng. Thêm vào `PeriodicJobs()`.
  - `ExpireHolds`/`expireCheckout`: `ListHoldsToRelease` lấy `status IN ('pending_payment','awaiting_payment')`; điều kiện hết hạn và `done` tính cho cả hai; mỗi đơn hết hạn gọi `releaseHold`.
  - `Pay` nhận cả `awaiting_payment` (hạn là `hold_expires_at = payment_due_at`).
  - Routes đối tác: `POST /v1/partner/bookings/{id}/accept` (không body), `POST /v1/partner/bookings/{id}/reject` (body `{reason}`), trả 200 `BookingView`.

- [ ] **Step 1: Viết test fail** `request_test.go` (đơn R như Task 5, now 10:00 khi chấp nhận):
  - `TestAcceptRequest_OpensPayment`: `awaiting_payment`, `payment_due_at = hold_expires_at = 12:00`, `hold_release_at = 12:05`, `hold_until = 12:05`, một payment `pending` 2.500.000 hết hạn 12:00 có `checkout_url`, view của khách có `payment`.
  - `TestAcceptRequest_AfterResponseDue409` (`request_expired`), `TestAcceptRequest_Twice409` (`request_not_pending`), `TestAcceptRequest_OtherPartner404`, `TestAcceptRequest_PartnerSuspended422`.
  - `TestRejectRequest_ReleasesVehicle`: `rejected`, `holds_vehicle = false`, checkout `closed`, yêu cầu mới trùng khoảng giờ → 201. Lý do rỗng → 422.
  - `TestRejectAfterAccept409`: đối tác không rút được sau khi chấp nhận.
  - `TestWithdraw_Requested`: `cancelled`/`customer_withdraw`, không refund, không bút toán, xe được nhả.
  - `TestWithdraw_AwaitingPaymentCancelsPayment`: payment `cancelled`, link bị hủy ở Fake, checkout `closed`.
  - `TestWithdraw_ConfirmedGoesToPolicyCancel`: đơn đã `confirmed` đi đường hủy thường (Task 8).
  - `TestAcceptVsWithdraw_Race` (Review Focus 4): lặp 20 lần, kiểm tra bất biến ở mục Review Focus.
  - `TestExpireRequests_AtResponseDue`: 20:59 còn `requested`; 21:00 → `expired`, `holds_vehicle = false`, checkout `closed`, có email.
  - `TestExpireHolds_AwaitingPayment`: 12:04 còn `awaiting_payment`; 12:05 → `expired`, payment `expired`, `holds_vehicle = false`.
  - `TestPay_AwaitingPaymentRecreatesLink`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Thêm 2 route vào `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS, kể cả `jobs_test.go` và `jobs_poison_test.go` cũ. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): request booking accept, reject, withdraw and expiry`.

---

### Task 7: Xác nhận thanh toán và tiền về trễ cho Request Booking

**Files:**
- Create: `internal/booking/confirm_rental_test.go`
- Modify: `internal/booking/{confirm.go,capacity.go,queries.sql}`

**Interfaces:**
- Consumes: Task 5–6 (`lockCheckoutCapacity`, `setRentalHold`, `peakRentalHolds`, `releaseHold`), `listing.LockRentalModels`.
- Produces (sửa `HandlePaid`, không tạo đường mới):
  - `lateBooking`: `awaiting_payment` có `hold_release_at ≤ now` là trễ, giống `pending_payment`.
  - `confirmation.eligible` mặc định: `pending_payment` hoặc `awaiting_payment`. `canTakeMoney` chấp nhận booking `awaiting_payment` như `pending_payment`; booking `requested`, `rejected` hay `cancelled` (kể cả `customer_withdraw`) làm checkout không nhận được tiền → hoàn đủ `duplicate_payment` như hiện nay.
  - `reconfirmChecks`: thuê xe chỉ kiểm listing ở đây (như xe tuyến), không gọi `slotStillOffered`.
  - Thứ tự mới trong `HandlePaid`, sau nhánh xe tuyến: nếu checkout có booking thuê xe thì `LockRentalModels(các mẫu của checkout)`, đọc lại `ListCheckoutBookings`, rồi `rentalChecks`.
  - `func (s *Service) rentalChecks(ctx, tx pgx.Tx, rows []store.ListCheckoutBookingsRow, models map[uuid.UUID]listing.RentalModel, rebookable map[uuid.UUID]bool, now time.Time) error` ghi một mục cho **mọi** booking thuê xe:
    - `awaiting_payment` chưa trễ: được (xe vẫn đang giữ cho nó).
    - Trễ (`expired`, hoặc `awaiting_payment` đã qua `hold_release_at`): được khi listing bookable (bước `reconfirmChecks`), mẫu `active`, `pickup_at > now`, và `peakRentalHolds([model], pickup, &return, now, except = booking) + 1 ≤ quantity`. Booking trễ mà `reconfirmChecks` chưa thấy thì `errStateChanged`.
  - `confirm()`:
    - `ok && awaiting_payment`: `confirmBooking` rồi `setRentalHold(true, nil)`.
    - `ok && expired`: `setRentalHold(true, nil)` (bên cạnh `reserveRideSeats` của xe tuyến) rồi `confirmBooking` với lý do "thanh toán trễ, xác nhận lại".
    - Không được và còn `awaiting_payment`: `expireBooking` (gọi `releaseHold`), refund `late_payment`.
  - Email đối tác khi đơn thuê xe được xác nhận (kể cả xác nhận lại, câu "đơn đã được thanh toán trễ; bạn vẫn phải phục vụ vì đã chấp nhận").

- [ ] **Step 1: Viết test fail** `confirm_rental_test.go` (R chấp nhận lúc 10:00):
  - `TestRentalPaid_Confirms`: tiền về 11:00. Kỳ vọng `confirmed`, `hold_until = NULL`, `holds_vehicle = true`, pending đối tác `2.250.000`, sổ sàn `+250.000`, `checkouts.paid_payment_id` được đặt, `Verify` không lỗi, có email đối tác.
  - `TestRentalPaid_InGrace`: tiền về 12:03 → xác nhận bình thường (không qua đường trễ).
  - `TestRentalLatePaid_ReconfirmsWhenFree` (Review Focus 3): job đã chạy, R `expired`, tiền về 12:10 → `reconfirmed`, `holds_vehicle = true`, `hold_until = NULL`, có bút toán.
  - `TestRentalLatePaid_UnitTakenRefunds` (Review Focus 3).
  - `TestRentalLatePaid_PeakExcludesSelf`: Vision qty 2 với một đơn khác giao nhau → R `expired` vẫn được xác nhận lại (`peak` loại chính R).
  - `TestRentalLatePaid_PendingPastReleaseUnitTaken`: job chưa chạy, R vẫn `awaiting_payment` quá 12:05, đơn khác đã giữ → R `expired` trong tx webhook, refund `late_payment`, `holds_vehicle = false`.
  - `TestRentalLatePaid_ModelDeactivatedRefunds` và `TestRentalLatePaid_ListingHiddenRefunds`.
  - `TestRentalLatePaid_AfterPickupRefunds`: tiền về sau `pickup_at`.
  - `TestRentalPaidAfterWithdraw_RefundedInFull`: `duplicate_payment` 2.500.000, đơn vẫn `cancelled`, không bút toán.
  - `TestRentalWebhook_DuplicateDelivery`: một lần xác nhận, một cặp bút toán.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Toàn bộ `confirm*_test.go` của Attraction và xe tuyến phải vẫn PASS.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): request booking payment confirmation and late re-confirm for rentals`.

---

### Task 8: Hủy đơn đã xác nhận, giao xe bằng QR, nhận lại xe, No-show

**Files:**
- Create: `internal/booking/{rental_lifecycle.go,rental_lifecycle_test.go}`
- Modify: `internal/booking/{cancel.go,checkin.go,jobs.go,views.go,http_partner.go,queries.sql}`, `cmd/api/{main.go,routes_test.go}` (đăng ký `FinishRentalsWorker` ngay task này)

**Interfaces:**
- Consumes: Task 5–7; `cancelLocked`, `cancelBlock`, `scheduleRelease`, `insertHistory`.
- Produces:
  - `cancel()` (đường Attraction/thuê xe) gọi `lockCheckoutCapacity(b.CheckoutID)` đầu transaction, trước `LockCheckout`. `cancelLocked` đã gọi `releaseHold` (Task 5). Khách và admin hủy đơn thuê xe `confirmed` dùng nguyên `cancelBlock` (tới `service_end_at = return_at`), % hoàn theo `original_service_start_at = pickup_at`. Đơn `in_service`/`completed`/`no_show` không hủy được.
  - Giao xe: `checkIn` rẽ nhánh `self_drive_rental` sang `func (s *Service) checkInRental(ctx, tx pgx.Tx, q *store.Queries, b store.Booking, userID uuid.UUID, now time.Time) (CheckInResult, error)`:
    - `confirmed` (không thì `ErrCheckInNotAllowed`), `now ∈ [pickup_at − 2h, return_at)` (hằng `rentalHandoverEarly = 2 * time.Hour`), sai thì `ErrHandoverOutsideWindow` (422 `handover_outside_window`, "Chỉ giao xe được từ 2 giờ trước giờ nhận đến trước giờ trả").
    - Query `HandOverRental`: `in_service`, `in_service_at = checked_in_at = now` (để quét lại trả `Already`). History actor partner. Không release. Xe vẫn giữ.
  - `ErrNoShowTooEarly` (409 `no_show_too_early`, "Chỉ đánh dấu khách không đến sau giờ nhận xe").
  - `func (s *Service) MarkRentalNoShow(ctx, partnerID, userID, bookingID uuid.UUID) (BookingView, error)`: đọc không khóa (khác đối tác → 404, không phải thuê xe → `ErrInvalidBookingState`), `LockRentalModels` → `LockBooking` → kiểm tra lại: `confirmed` (không thì `ErrInvalidBookingState`), `now ≥ pickup_at` (không thì `ErrNoShowTooEarly`). `MarkNoShow`, `releaseHold`, `scheduleRelease(now)`, history actor partner.
  - `func (s *Service) MarkRentalReturned(ctx, partnerID, userID, bookingID uuid.UUID) (BookingView, error)`: cùng thứ tự khóa. `in_service` → `completed` (`CompleteRide` là UPDATE chung, dùng lại), `releaseHold`, `scheduleRelease(now)`, history actor partner. Đã `completed` thì trả 200 không đổi gì. Trạng thái khác → `ErrInvalidBookingState`.
  - Routes đối tác: `POST /v1/partner/bookings/{id}/no-show`, `POST /v1/partner/bookings/{id}/return` (không body, trả 200 `BookingView`). Giao xe dùng `POST /v1/partner/check-ins` đã có.
  - Job tự kết thúc (quyết định 4): `FinishRentalsArgs{}` (Kind `booking_finish_rentals`), `FinishRentalsWorker{Service *Service}`, `func (s *Service) FinishRentals(ctx context.Context) (int, error)`, thêm vào `PeriodicJobs()`:
    - Hai danh sách (`bookings_service_end_idx`), chung tracker `finishBackoff` (skip theo id như các job khác, `beforeItem` cho test):
      - `ListRentalAutoComplete(cutoff = now − rentalAutoCompleteAfter, lim, skip)`: `status = 'in_service' AND service_type = 'self_drive_rental' AND service_end_at <= cutoff`.
      - `ListRentalAutoNoShow(now, lim, skip)`: `status = 'confirmed' AND service_type = 'self_drive_rental' AND service_end_at <= now`.
    - Mỗi booking một tx: `LockRentalModels` → `LockBooking` → kiểm tra lại trạng thái và mốc; `CompleteRide`/`MarkNoShow`, `releaseHold`, `scheduleRelease(now)`, history actor system (lý do "tự hoàn thành sau 24 giờ quá giờ trả" / "khách không nhận xe trước giờ trả").

- [ ] **Step 1: Viết test fail** `rental_lifecycle_test.go` (R đã `confirmed`, gross 2.500.000, hoa hồng 250.000):
  - `TestCancelRental_CustomerPolicy`: hủy lúc 2026-11-05 10:30 (còn 45h30 tới 07/11 08:00) → hoàn 50% = **1.250.000** `pending` (có bank); available đối tác = 2.250.000 − 1.250.000 = **1.000.000**, pending 0, `holds_vehicle = false`, `Verify` không lỗi.
  - `TestCancelRental_AdminForceMajeure`: refund 2.500.000, đối tác 0, sàn net 0, có audit.
  - `TestCancelRental_InService409`.
  - `TestRentalHandover_Window`: 07/11 05:59 → 422; 06:00 → `in_service` với `in_service_at`; quét lại → `Already`. Đơn khác: tại `return_at` → 422.
  - `TestRentalHandover_NotConfirmed409`: đơn `awaiting_payment`.
  - `TestRentalReturned_EarlyFreesVehicle` (Review Focus 5): nhận lại lúc 08/11 10:00 → `completed`, `release_due_at = 09/11 10:00`, yêu cầu [08/11 12:00, 09/11 10:00) → 201. Sau `ReleaseDue` → available 2.250.000, `Verify` không lỗi.
  - `TestRentalReturned_LateDoesNotBlockNext` (Review Focus 5): R1 còn `in_service` lúc 09/11 11:00; yêu cầu [09/11 12:00, 10/11 12:00) → 201.
  - `TestRentalReturned_Idempotent` và `TestRentalReturned_NotInService409`.
  - `TestRentalNoShow`: 07/11 07:59 → 409 `no_show_too_early`; 08:00 → `no_show`, `holds_vehicle = false`, `release_due_at = 08/11 08:00`; không hoàn tiền.
  - `TestRentalNoShow_OtherPartner404`, `TestReschedule_RentalRejected409`, `TestAttractionAndRideUnchanged` (`MarkNoShows`/`CompleteRides` không chạm thuê xe).
  - `TestFinishRentals_AutoCompleteBoundary`: R giao xe, `return_at = 09/11 10:00`. Lúc 10/11 09:59 vẫn `in_service`; 10/11 10:00 → `completed`, `holds_vehicle = false`, `release_due_at = 11/11 10:00`; sau `ReleaseDue` available 2.250.000, `Verify` không lỗi.
  - `TestFinishRentals_AutoNoShowBoundary`: R `confirmed` chưa giao. 09/11 09:59 vẫn `confirmed`; 09/11 10:00 → `no_show`, `release_due_at = 10/11 10:00`, không refund.
  - `TestFinishRentals_ManualWins`: đối tác bấm nhận lại xe 08/11 10:00 → job lúc 10/11 10:00 không đổi gì (`completed_at`, `release_due_at` giữ của lần bấm); tương tự No-show bấm tay.
  - `TestFinishRentals_BackoffSkip`: `beforeItem` lỗi cho một booking → booking khác vẫn được xử lý, booking lỗi bị skip ở lần chạy kế tiếp trong thời gian backoff.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Thêm 2 route vào `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS, kể cả `cancel*_test.go`, `checkin_test.go`, `ride_lifecycle_test.go` cũ. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): rental cancel, QR handover, return, no-show and auto-finish job`.

---

### Task 9: Nối dây, kiểm thử trọn luồng, tài liệu

**Files:**
- Create: `cmd/api/e2e_rental_test.go`
- Modify: `cmd/api/{main.go,routes.go,routes_test.go}`, `docs/superpowers/specs/2026-11-04-database-schema.md` (§1.1, §5.7, §6), `GLOSSARY.md`, `docs/superpowers/plans/README.md`

**Interfaces:**
- Consumes: mọi task trước.
- Produces:
  - `main`: `booking.Deps.Partners = partnerSvc` (worker `ExpireRequests` đã đăng ký ở Task 6).
  - `buildMux` không đổi chữ ký. Route mới đi qua `Listing.PartnerRoutes/PublicRoutes` và `Booking.CustomerRoutes/PartnerRoutes`. Route public `GET /v1/rental-vehicles` và mọi route mới đã có trong danh sách của `routes_test.go`.
  - Tài liệu DB:
    - §5.7 ✅ plan 7: `province_code` của điểm nhận, snapshot và `destination_province_code` trong `booking_rental_details`, `pickup_point_id NOT NULL`, bỏ `delivery_address`, CHECK giờ giao nhận.
    - §6: CHECK mới của `bookings`, `in_service_at = checked_in_at` khi giao xe, luật hạn `response_due_at`/`payment_due_at`, No-show do đối tác bấm, dòng Self-Drive Rental trong bảng mốc thời gian có job `FinishRentals` (+24h tự hoàn thành, tự No-show tại giờ trả), luật đếm xe theo đỉnh sử dụng cùng lúc.
    - §1.1: "khóa mẫu xe trước mọi checkout có thuê xe".
  - `GLOSSARY.md`: No-show có thêm nghĩa "đối tác đánh dấu khách không đến nhận xe".
  - `plans/README.md`: đánh dấu plan 7 xong.

- [ ] **Step 1: Viết test fail** `e2e_rental_test.go`, qua mux thật với `payment.Fake`:
  1. Admin đặt hoa hồng `self_drive_rental` 10%, tạo đối tác, bật `self_drive_rental`.
  2. Đối tác tạo listing (68), cửa hàng 06:00–22:00, mẫu Vios (800.000, qty 1), điểm P0 và P1 (100.000), ảnh, gửi duyệt, admin duyệt.
  3. Khách `GET /v1/rental-vehicles` (Vios `units_left = 1`, `days = 3`), rồi `POST /v1/bookings/rental` P1 07/11 08:00 → 09/11 10:00 có `Idempotency-Key`. Kỳ vọng 201, `status = requested`, `gross_amount 2.500.000`, `payment = null`. Tìm lại thì Vios không hiện.
  4. Clock 10:00: đối tác `POST /accept`. Khách `GET /v1/bookings/{id}` có `payment.checkout_url`, `request.payment_due_at = 12:00`.
  5. `simulate-paid` một lần. Kỳ vọng `confirmed` và `GET /v1/partner/balance` có `pending = 2.250.000`.
  6. Clock 07/11 07:30: đối tác check-in bằng QR → `in_service`. Đối tác **không** bấm nhận lại xe. Clock 10/11 10:00: `FinishRentals` → `completed`.
  7. `ReleaseDue` lúc 11/11 10:00. Kỳ vọng `available = 2.250.000`, `pending = 0`, `ledger.Verify` không lỗi.
  8. Đơn thứ hai (Vios 12/11 08:00 → 13/11 08:00) được chấp nhận và thanh toán nhưng không giao xe; clock 13/11 08:00: `FinishRentals` → `no_show`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Nối dây** và cập nhật tài liệu.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...` và `go tool sqlc diff` (không có diff).
- [ ] **Step 5: Kiểm tra tay** trên **DB riêng của nhánh** (`make db-branch`, migrate từ đầu), dùng `PAYMENT_PROVIDER=fake`:
  1. Tạo listing thuê xe đầy đủ, duyệt.
  2. Gửi hai yêu cầu cho mẫu còn 1 xe (cái thứ hai bị 409), chấp nhận cái đầu, để quá 2h05 cho job hết hạn, gửi lại.
  3. Rút một yêu cầu đã chấp nhận, `simulate-paid` vào link đã hủy và xem refund `duplicate_payment`.
  4. Giao xe, nhận lại sớm, xem số dư. Một đơn khác để job `FinishRentals` tự hoàn thành (đổi clock qua `return_at + 24h`). Ghi kết quả vào báo cáo task.
- [ ] **Step 6: Commit** `feat(rental): wire self-drive rentals; e2e request-to-return flow`.

---

## Không làm trong plan này

- Giao xe tới địa chỉ bất kỳ của khách (`delivery_address`), trả xe ở điểm khác điểm nhận (quyết định 1).
- Thời gian đệm vệ sinh/bàn giao giữa hai lượt thuê (mặc định B3).
- Hoàn tiền khi trả xe sớm, thu phí trả trễ trên sàn (quyết định 3).
- Quản lý từng chiếc xe, giá theo ngày/mùa, ảnh nhiều tấm cho mẫu xe.
- Private Charter (plan 8). Chuông thông báo trong web (plan 9). Báo cáo §13 cho thuê xe (plan 10).
- Chụp lại Rental Terms vào đơn: đối tác sửa điều khoản không cần duyệt lại, `listing_audit_log` là căn cứ khi khiếu nại.

## Các quyết định đã chốt

1. **Điểm nhận xe:** bắt buộc chọn từ danh sách điểm của đối tác; mỗi điểm có phí giao riêng, tính một lần; trả xe tại chính điểm đó; không có địa chỉ giao tự do.
2. **Thời lượng:** 1..30 ngày (khối 24 giờ, làm tròn lên, tối thiểu 1); giờ nhận trong vòng 180 ngày tới.
3. **Trả sớm/trả muộn:** trả sớm không hoàn tiền, xe trống lại ngay; trả muộn do cửa hàng xử lý, LunaIn không can thiệp.
4. **Tự kết thúc để tiền đối tác không bị kẹt:** đã giao xe (`in_service`) mà chưa bấm nhận lại tới `service_end_at + 24h` → tự `completed`; đã xác nhận mà chưa giao xe tới `service_end_at` → tự `no_show`. Thao tác tay của đối tác vẫn có và thắng nếu xảy ra trước. `service_start_at = pickup_at`, `service_end_at = return_at` theo lịch.
5. **(Controller) Đếm xe trống theo đỉnh sử dụng cùng lúc** trong khoảng yêu cầu, nhận khi `peak + 1 ≤ quantity`; chặn giảm `quantity` dùng cùng hàm đỉnh trên [now, ∞).

## Mặc định của builder (chưa có phản hồi, đang dùng)

- B1. `response_due_at = min(now + 12h, pickup_at − 2h)`; chỉ nhận yêu cầu khi `pickup_at ≥ now + 2h + 1h`; quá hạn thì `expired`, xe được nhả.
- B2. Giờ nhận/trả tròn phút, trong giờ giao nhận của cửa hàng (không có khung qua đêm); quét QR giao xe trong `[pickup_at − 2h, return_at)`.
- B3. Không có thời gian đệm giữa hai lượt thuê (nửa mở, nối tiếp được).
- B4. Khách chọn tỉnh đến (`destination_province_code`, bắt buộc), chỉ hiển thị cho đối tác.
- B5. Tìm kiếm lọc theo tỉnh của cửa hàng, loại xe, số chỗ tối thiểu; hiện `units_left`.
- B6. Email owner đối tác khi có yêu cầu mới, khi khách rút, khi đơn được thanh toán (kể cả xác nhận lại).

## Mâu thuẫn và chỗ spec chưa nói đã gặp

- **DB §6** cho `pickup_point_id NULL` và `delivery_address NULL`. Plan bắt `pickup_point_id NOT NULL` và bỏ `delivery_address` (quyết định 1); cập nhật DB doc ở Task 9.
- **Spec §6.2** cho đối tác 12 giờ phản hồi "không quá giờ bắt đầu dịch vụ" và cho khách 2 giờ trả tiền sau khi chấp nhận. Nếu chỉ chặn ở giờ nhận thì một đơn được chấp nhận sát giờ nhận sẽ không còn đủ 2 giờ. Plan chặn hạn phản hồi ở `pickup_at − 2h` (mặc định B1).
- **Spec §6.2** sơ đồ không có `no_show` cho thuê xe, nhưng đoạn chữ nói đối tác đánh dấu `no_show`. **DB §6** bảng mốc thời gian chỉ ghi "đối tác bấm đã nhận lại xe". Plan thêm `confirmed → no_show` do đối tác bấm sau `pickup_at`.
- **DB §6** `bookings_instant_only_status_check` cấm `checked_in` cho Request Booking, nên giao xe chuyển thẳng `confirmed → in_service`. Plan ghi cả `checked_in_at` (= giờ giao) để quét lại trả "đã check-in lúc …" như spec §10.
- **Spec §7** (và DB §6) nói đếm số Booking có khoảng giao nhau rồi so với `quantity`. Cách này từ chối oan (qty 2, A T2–T3, B T4–T5, yêu cầu T2–T5 bị đếm 2). Theo quyết định 5 của controller, plan đếm **đỉnh sử dụng cùng lúc** trong khoảng yêu cầu (`peak + 1 ≤ quantity`) và dùng cùng hàm đó cho việc chặn giảm `quantity`; vẫn khóa `rental_vehicle_models FOR UPDATE` và dùng index GiST. Task 9 sửa DB doc.
- **DB §6** nói checkout `closed` khi mọi booking "đã hết hạn hoặc bị từ chối mà chưa thanh toán". Plan đóng cả khi khách rút yêu cầu (`customer_withdraw`). Tiền về sau khi rút được hoàn đủ với lý do `duplicate_payment` (đường `canTakeMoney` hiện có), không phải `late_payment`.
- **GLOSSARY** định nghĩa No-show chỉ cho vé tham quan; plan 6 đã mở rộng cho xe tuyến mà chưa sửa glossary. Task 9 sửa định nghĩa.
- `rental_stores` trong DB §5.7 không có vị trí; plan dùng vị trí của `listings` làm địa chỉ cửa hàng và vẫn bắt ba mục vị trí khi gửi duyệt (khác xe tuyến).
