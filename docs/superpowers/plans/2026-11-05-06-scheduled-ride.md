# Plan 6 — Scheduled Ride (xe tuyến cố định) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Đối tác đăng được tuyến xe một chiều, khai báo lịch chạy lặp và để hệ thống sinh sẵn các chuyến. Khách tìm chuyến theo điểm đi, điểm đến và ngày, rồi đặt một chiều hoặc khứ hồi (hai Booking chung một checkout, trả tiền một lần). Ghế được giữ thật và không bao giờ bán vượt. Hủy, tiền về trễ, check-in QR theo cửa sổ giờ, No-show và hoàn thành đều đi qua đúng các đường tiền và sổ cái đã có của plan 4–5.

**Architecture:**

- `listing` sở hữu ba bảng mới `ride_routes`, `ride_schedules`, `ride_trips`. Nó có thêm:
  - API đối tác cho tuyến, lịch và chuyến.
  - Job sinh chuyến (River periodic, `listing.PeriodicJobs()`).
  - Tìm chuyến public.
  - Các hàm giữ ghế nhận `tx` của người gọi: `LockTrips`, `ReserveSeats`, `ReleaseSeats`, `CancelTripByAdmin`. `booking` không bao giờ tự ghi bảng `ride_trips`.
- `booking` sở hữu `booking_ride_details`. Nó mở rộng các đường plan 4 đã có, **không** viết lại đường riêng:
  - `CreateRide` dùng chung idempotency, checkout và `ReservePayment` với `CreateAttraction`.
  - `HandlePaid` thêm bước khóa chuyến và quy tắc "cả hai chiều hoặc hoàn hết".
  - `cancel.go` tách `cancelLocked` ra để dùng cho hủy một đơn, hủy khứ hồi và admin hủy chuyến.
  - `checkIn`, `MarkNoShows`, `ExpireHolds` rẽ nhánh theo `service_type`. Thêm job mới `CompleteRides`.
- Bất biến khóa mới: **mọi transaction khóa checkout có booking xe tuyến đều khóa các chuyến của nó trước** (DB §1.1: sức chứa đứng trước checkouts).

**Tech Stack:** như plan 1–5 (Go 1.27, PostgreSQL 17, pgx/v5, sqlc, goose, River, `net/http` ServeMux). PayOS vẫn chỉ có Fake.

**Spec:**
- [spec hệ thống](../specs/2026-11-04-lunain-travel-backend-design.md) §5.6, §6.1, §6.3, §6.4, §7, §8, §10
- [thiết kế DB](../specs/2026-11-04-database-schema.md) §1.1, §1.2, §4 (`platform_settings`), §5.5, §6 (mốc thời gian theo loại, `booking_ride_details`, "Vé khứ hồi hủy cùng nhau"), §10
- [ADR 0003](../../adr/0003-listing-base-plus-type-tables.md), [ADR 0004](../../adr/0004-partner-ledger.md), [GLOSSARY](../../../GLOSSARY.md) (Route, Trip, Scheduled Ride)
- Các header plan 4 và plan 5. Rulings 1–26 của plan 4 (`.claude/worktrees/booking-payment/.superpowers/sdd/2026-11-05-04-booking-payment-ledger/progress.md`) vẫn ràng buộc. Đặc biệt là Ruling 9–17 (định tuyến tiền), Ruling 21 (hủy tới `service_end_at`), Ruling 25 (xác nhận lại phải đúng giờ đã bán) và Ruling 26 (`service_end_at` của Attraction chỉ áp cho Attraction).

## Global Constraints

- Mọi Global Constraints của plan 1–5 vẫn áp dụng:
  - uuid v7, clock được inject (`clock.VN`), lỗi JSON tiếng Việt.
  - Path id sai định dạng hoặc không tồn tại thì 404. Body sai thì 422. Body có trường lạ thì 400.
  - Tiền là `bigint` VND, nhân tỷ lệ bằng `money.MulBps`.
  - Test chạy trên DB riêng clone từ template đã migrate, assert đúng số tiền và gọi `ledger.Verify`.
  - Migration trên `main` bất biến: file mới bắt đầu từ `00017`, rồi chạy `make migrations-sum`. Mỗi worktree dùng DB riêng tạo bằng `make db-branch`.
  - Mỗi route `/v1/admin/*` và `/v1/partner/*` mới phải có trong `adminRoutes`/`partnerRoutes` của `cmd/api/routes_test.go`.
  - Thao tác admin gọi `audit.Record` trong cùng transaction.
- **Thứ tự khóa (DB §1.1):**
  ```
  users → partners → listings → ride_trips (id tăng dần) → checkouts (id tăng dần)
        → bookings (id tăng dần) → dòng con của booking (settlements, refunds)
        → payments (FOR NO KEY UPDATE) → payouts → partner_balances
  ```
  - Mọi transaction khóa một checkout có booking xe tuyến **phải** khóa trước các chuyến của checkout đó: tạo, hết hạn, `HandlePaid`, hủy, admin hủy chuyến.
  - Nếu chỉ biết id booking: đọc `trip_id` mà không khóa, khóa chuyến, rồi mới khóa checkout và booking và kiểm tra lại trạng thái.
  - Không bao giờ gọi provider khi đang mở transaction.
- **Giữ ghế (spec §7, DB §5.5), nguyên văn:**
  - `UPDATE ride_trips SET seats_reserved = seats_reserved + n WHERE id = $1 AND status = 'scheduled' AND departure_at > $now AND seats_reserved + n <= seat_capacity`, rồi kiểm tra số dòng bị ảnh hưởng.
  - `CHECK (seats_reserved BETWEEN 0 AND seat_capacity)` là lưới an toàn cuối.
  - Booking `expired` hoặc `cancelled` thì trừ lại ghế **trong cùng transaction** chuyển trạng thái.
  - Trước khi giữ ghế, transaction đã khóa chuyến. Nó nhả luôn ghế của các booking trên chuyến này đã quá `hold_release_at` mà job chưa quét tới.
  - Ghế chỉ trả lại khi `expired`/`cancelled`. `checked_in`, `completed`, `no_show` vẫn giữ ghế.
- **Hold:** `hold_minutes` 15 và `hold_grace_minutes` 5 (`platform_settings`). `hold_expires_at = now + 15'` là mốc khách thấy. `hold_release_at = hold_expires_at + 5'` là mốc nhả ghế thật.
- **Settings dùng (đã có từ `00005`):** `ride_checkin_window_minutes` int > 0 mặc định **60**, `ride_no_show_after_minutes` int ≥ 0 mặc định **60**, `release_delay_days` 1. **Thêm mới (đã chốt với người dùng):** `ride_booking_cutoff_minutes` int ≥ 0 mặc định **60**: chỉ đặt được chuyến có `departure_at > now + max(ride_booking_cutoff_minutes, hold_minutes + hold_grace_minutes)`.
- **Số ghế (đã chốt):** khách nhập số khách; không có trần cố định mỗi đơn, chỉ bị giới hạn bởi số ghế còn trống của chuyến. Khứ hồi hai chiều cùng số ghế. Tìm kiếm chỉ hiện chuyến còn đủ ghế, kèm loại xe (`vehicle_description`).
- **Khứ hồi (đã chốt):** chiều về là tuyến ngược của **cùng nhà xe**. **Sinh chuyến (đã chốt):** 60 ngày tới.
- **Ràng buộc DB (DB §5.5):**
  - `ride_trips`: `CHECK (seats_reserved BETWEEN 0 AND seat_capacity)`, `UNIQUE(listing_id, departure_at)`, `UNIQUE(id, listing_id)`.
  - Trigger chặn sửa `departure_at` và `price_per_seat` khi `seats_reserved > 0`.
  - `booking_ride_details (trip_id, listing_id) → ride_trips(id, listing_id)`.
- **Mốc thời gian (DB §6):**
  - `service_start_at = departure_at`, `service_end_at = departure_at + duration_minutes` (chụp lại `duration_minutes` lúc đặt).
  - Qua `departure_at + ride_no_show_after_minutes` mà chưa check-in thì `no_show`. Đã check-in thì job chuyển `completed` khi qua `service_end_at`.
  - Index No-show của xe tuyến dùng `service_start_at` (đã có: `bookings_ride_no_show_idx`).
  - Quy tắc `service_end_at` = 00:00 hôm sau (Ruling 26) **chỉ** áp cho Attraction.
- **Khứ hồi (spec §5.6, §8, DB §6), nguyên văn:**
  - "Khứ hồi là **hai Booking riêng** (chiều đi, chiều về) thuộc cùng một `checkout`, thanh toán chung một lần. Mỗi chiều có QR, trạng thái, check-in và No-show riêng."
  - "Vé khứ hồi được **hủy cùng nhau**: khách hủy hay Super Admin hủy (kể cả hủy chuyến của một chiều) thì cả hai chiều đều bị hủy."
  - "Khách chỉ hủy được khi chưa đi chiều nào. % hoàn của từng chiều tính theo chính sách hủy với giờ khởi hành của chính chiều đó. Super Admin hủy thì hoàn 100% cả hai chiều."
  - "Khứ hồi: cả hai chiều trong cùng `checkout` phải xác nhận lại được thì mới xác nhận. Nếu chỉ một chiều được thì hoàn toàn bộ, để khách không bị kẹt nửa chặng."
  - Chiều về là listing tuyến ngược của **cùng nhà xe** (DB §5.5).
- **Đối tác (spec §6.1):**
  - Không hủy hoặc giảm sức chứa được chuyến đã có ghế được giữ: `seat_capacity` mới không được nhỏ hơn `seats_reserved`. Chỉ sửa giờ, giá hoặc hủy chuyến khi `seats_reserved = 0`.
  - Đối tác bị đình chỉ thì không có đơn mới (`BookableListing` qua `visible_listings`).
- **Đổi lịch chỉ dành cho Attraction (spec §6.4):** xe tuyến luôn có `can_reschedule = false`, và `POST /reschedule` trả 409 `reschedule_not_allowed`.
- **`visible_listings`:** plan này **không** thêm cột vào `listings`, nên view giữ nguyên. Nếu task nào buộc phải thêm cột thì migration phải `CREATE OR REPLACE VIEW visible_listings` (view dùng `SELECT l.*`).
- **Thông báo:** chỉ email qua outbox (`mailer.EnqueueTx`).
- **Commit:** không thêm trailer `Co-Authored-By` hay bất kỳ dòng ghi công AI nào.

## Review Focus

1. **Hai khách cùng giành ghế cuối:** chuyến còn 1 ghế, hai `CreateRide` (mỗi đơn 1 ghế) chạy song song. Kỳ vọng đúng một 201 và một 409 `not_enough_seats`, `seats_reserved = seat_capacity`, chỉ có một checkout và một payment. Test `TestCreateRide_LastSeatRace` ở Task 5.
2. **Giữ chỗ quá hạn mà job chưa quét, trên cùng chuyến:** chuyến 2 ghế đã bị một đơn `pending_payment` giữ hết, `hold_release_at` đã qua. Khách mới đặt 2 ghế thì thành công. Trong cùng transaction đó, đơn cũ chuyển `expired` (có history), payment `pending` của nó chuyển `expired`, checkout `closed`, và link bị hủy sau commit. Kết quả `seats_reserved = 2`. Nếu đơn cũ là khứ hồi và chiều kia nằm trên chuyến khác (chưa khóa) thì chiều kia vẫn để job xử lý. Test ở Task 5.
3. **Chiều về khởi hành trước khi chiều đi tới nơi, kể cả qua nửa đêm:** chiều đi 22:00, chạy 180 phút, tới nơi 01:00 hôm sau. Chiều về 00:30 hôm sau thì 422 `invalid_return_trip`, chiều về 01:00 thì được. Ngày tìm kiếm và ngày sinh chuyến luôn tính theo giờ VN của `departure_at`. Test ở Task 2, Task 4 và Task 5.
4. **Admin hủy chuyến của một chiều trong vé khứ hồi đã thanh toán:** cả hai chiều bị hủy, mỗi chiều hoàn 100% và miễn hoa hồng. Ghế trên cả hai chuyến được trả lại, pending của đối tác về 0, `Verify` không lỗi. Nếu chiều đi đã `checked_in` thì chỉ chiều về bị hủy. Test ở Task 7.
5. **Tiền về sau khi ghế đã bán cho người khác:** khứ hồi đã `expired`, chuyến đi bị khách khác mua hết, rồi tiền về. Kỳ vọng hoàn `late_payment` 100% **cho cả hai chiều**, không có bút toán nào, `seats_reserved` của cả hai chuyến không đổi. Ngược lại, nếu cả hai chuyến còn ghế thì cả hai được xác nhận lại và ghế được giữ lại. Test ở Task 6.

---

## Cấu trúc file

```
internal/platform/db/migrations/00017_ride.sql            # ride_routes, ride_schedules, ride_trips, trigger, booking_ride_details, CHECK checked_in
internal/listing/{ride.go,ride_trips.go,ride_jobs.go,ride_search.go,http_ride.go,queries.sql,errors.go,review.go,public.go,deps.go}
internal/listing/{ride_test.go,ride_trips_test.go,ride_jobs_test.go,ride_search_test.go}
internal/booking/{create_ride.go,ride_seats.go,cancel_ride.go,create.go,confirm.go,cancel.go,checkin.go,jobs.go,views.go,http_*.go,queries.sql}
internal/booking/{ride_helpers_test.go,create_ride_test.go,confirm_ride_test.go,cancel_ride_test.go,ride_lifecycle_test.go,schema_test.go}
cmd/api/{main.go,routes.go,routes_test.go,e2e_ride_test.go}
docs/superpowers/specs/2026-11-04-database-schema.md (§5.5, §6), docs/superpowers/plans/README.md
```

---

### Task 1: Schema xe tuyến

**Files:**
- Create: `internal/platform/db/migrations/00017_ride.sql` (cập nhật `CHECKSUMS` bằng `make migrations-sum`)
- Modify: `internal/listing/queries.sql`, `internal/booking/queries.sql` (mỗi bên tối thiểu một query chạm bảng mới để sqlc sinh model), `internal/booking/schema_test.go`, `internal/settings` (struct `Settings`, API admin cập nhật settings và test của nó)
- `00017_ride.sql` thêm `ALTER TABLE platform_settings ADD COLUMN ride_booking_cutoff_minutes int NOT NULL DEFAULT 60 CHECK (ride_booking_cutoff_minutes >= 0)`. Test settings: đọc mặc định 60, admin sửa được, giá trị âm thì 422.

**Schema** (đúng DB §5.5 và §6, kèm các phần bổ sung đã chốt dưới đây):

- **`ride_routes`:**
  - `listing_id` PK, FK `listings`.
  - `origin_province_code`, `origin_ward_code`, `origin_name`, `origin_address`, cùng `dest_*` tương tự, tất cả NOT NULL. FK `(origin_ward_code, origin_province_code) → wards(code, province_code)`, tương tự cho `dest_*`.
  - `duration_minutes int CHECK BETWEEN 1 AND 4320`, `vehicle_description text NOT NULL`.
  - Index `(origin_province_code, dest_province_code)`.
- **`ride_schedules`:**
  - `id`, `listing_id` FK `ride_routes(listing_id)` (phải có tuyến trước mới tạo được lịch).
  - `departure_time time`, `weekdays smallint CHECK BETWEEN 1 AND 127` (bit 0 = thứ Hai, giống `attraction_time_slots`).
  - `seat_capacity int CHECK > 0`, `price_per_seat bigint CHECK > 0`.
  - `valid_from date`, `valid_to date NULL`, `CHECK (valid_to IS NULL OR valid_to >= valid_from)`, `active bool`.
  - **Bổ sung:** `generated_until date NULL`, là ngày cuối cùng (VN) job đã sinh chuyến cho lịch này.
  - `UNIQUE(id, listing_id)`.
- **`ride_trips`:**
  - `id`, `listing_id` FK `ride_routes(listing_id)`, `schedule_id NULL` với FK kép `(schedule_id, listing_id) → ride_schedules(id, listing_id)`.
  - `departure_at timestamptz`, `seat_capacity int CHECK > 0`, `seats_reserved int NOT NULL DEFAULT 0`, `price_per_seat bigint CHECK > 0`.
  - `status CHECK IN ('scheduled','cancelled','departed')`. Plan 6 không ghi `departed` (xem "Không làm").
  - `cancelled_reason`, `cancelled_at`, `cancelled_by uuid NULL FK users`.
  - **Bổ sung:** `cancel_source text NULL CHECK IN ('partner','admin','schedule')`.
  - `CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))`, `CHECK ((cancelled_at IS NULL) = (cancel_source IS NULL))`, `CHECK (status <> 'cancelled' OR cancelled_reason IS NOT NULL)`.
  - `CONSTRAINT ride_trips_seats_check CHECK (seats_reserved BETWEEN 0 AND seat_capacity)`.
  - `UNIQUE(listing_id, departure_at)` (đã đủ cho index `(listing_id, departure_at)`), `UNIQUE(id, listing_id)`.
  - Index tìm kiếm `(departure_at) WHERE status = 'scheduled'`, index `(schedule_id, departure_at)`.
- **Trigger `ride_trips_booked_immutable`:** `BEFORE UPDATE`. Nếu `OLD.seats_reserved > 0` và (`NEW.departure_at IS DISTINCT FROM OLD.departure_at` hoặc `NEW.price_per_seat IS DISTINCT FROM OLD.price_per_seat`) thì `RAISE EXCEPTION 'ride_trip_booked' USING ERRCODE = 'P0001'`. App luôn kiểm tra trước; trigger chỉ là lưới an toàn.
- **`booking_ride_details`:**
  - `booking_id` PK, `listing_id`, `trip_id`, `seats int CHECK > 0`, `direction CHECK IN ('outbound','return')`.
  - **Bổ sung:** `origin_name`, `dest_name` text NOT NULL (chụp lại lúc đặt).
  - FK kép `(booking_id, listing_id) → bookings(id, listing_id)` và `(trip_id, listing_id) → ride_trips(id, listing_id)`. Index `(trip_id)`.
- **`bookings`:** `ADD CONSTRAINT bookings_checked_in_at_check CHECK (status <> 'checked_in' OR checked_in_at IS NOT NULL)`.
- Down: drop theo thứ tự ngược lại.

- [ ] **Step 1: Viết test fail** trong `schema_test.go`, chèn trực tiếp bằng SQL:
  - `TestRideTripSeatsCheck`: `seats_reserved = seat_capacity + 1` thì 23514, `-1` thì 23514.
  - `TestRideTripUniqueDeparture`: hai chuyến cùng `listing_id` và `departure_at` thì 23505.
  - `TestRideTripTriggerBlocksBookedChanges`: khi `seats_reserved = 1`, UPDATE `departure_at` hoặc `price_per_seat` thì lỗi `ride_trip_booked`. UPDATE `seat_capacity` vẫn được. Khi `seats_reserved = 0` thì sửa giờ và giá đều được.
  - `TestRideDetailTripMustMatchListing`: `booking_ride_details` có `trip_id` thuộc listing khác thì 23503.
  - `TestRideScheduleOtherListing`: chuyến có `schedule_id` của listing khác thì 23503.
  - `TestBookingCheckedInNeedsTimestamp`: 23514.
- [ ] **Step 2: Chạy** `go test ./internal/booking -run 'TestRide|TestBookingCheckedIn'`. Kỳ vọng FAIL.
- [ ] **Step 3: Viết migration**, rồi chạy `make migrations-sum` và `make sqlc`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...` (gồm `TestMigrationChecksums`).
- [ ] **Step 5: Commit** `feat(db): scheduled ride schema`.

---

### Task 2: Tuyến, lịch chạy, sinh chuyến, điều kiện gửi duyệt

**Files:**
- Create: `internal/listing/{ride.go,ride_jobs.go,ride_test.go,ride_jobs_test.go}`, `internal/listing/http_ride.go` (phần tuyến và lịch)
- Modify: `internal/listing/{errors.go,review.go,public.go,http_partner.go,queries.sql}`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: `LocationValidator.ValidateLocation`, `lockOwned`, `requireActivePartner`, `logChange`, `atVN`, `weekdayBit`, `vnDate` (đã có trong `listing`).
- Produces:
  - `const rideServiceType = "scheduled_ride"`. `supportedServiceType` trả `true` cho loại này.
  - `var ErrNotRideListing = httpx.NewError(422, "wrong_service_type", "Thao tác này chỉ áp dụng cho Listing xe tuyến")`.
  - `ErrInvalidRide` (422 `invalid_ride`), `ErrRouteRequired` (422 `route_required`), `ErrOverlappingSchedule` (422 `overlapping_schedule`).
  - `type RidePoint struct { ProvinceCode, WardCode, Name, Address string }` (json `province_code`, `ward_code`, `name`, `address`).
  - `type RideRouteInput struct { Origin, Dest RidePoint; DurationMinutes int; VehicleDescription string }`.
  - `type RideRoute struct { ListingID uuid.UUID; RideRouteInput }`.
  - `type RideSchedule struct { ID, ListingID uuid.UUID; DepartureTime string /*"HH:MM"*/; Weekdays, SeatCapacity int; PricePerSeat int64; ValidFrom string; ValidTo *string; Active bool }`.
  - `type RideScheduleInput struct { DepartureTime string; Weekdays, SeatCapacity int; PricePerSeat int64; ValidFrom string; ValidTo *string }`.
  - `type RideSchedulePatch struct { SeatCapacity *int; PricePerSeat *int64; ValidTo *string; ClearValidTo bool; Active *bool }`.
    - JSON: `valid_to: null` nghĩa là bỏ hạn, thiếu trường nghĩa là giữ nguyên. `departure_time`/`weekdays`/`valid_from` có trong body thì 422: muốn đổi thì tạo lịch mới.
  - `type RideDetail struct { Route *RideRoute; Schedules []RideSchedule }`.
  - `func (s *Service) SetRideRoute(ctx, partnerID, actor, listingID uuid.UUID, in RideRouteInput) (RideRoute, error)`:
    - Upsert, ghi `listing_audit_log` action `ride_route`.
    - Ràng buộc: tên 1..200 rune, địa chỉ 1..500, `vehicle_description` 1..500, `duration_minutes` 1..4320. Cả hai điểm qua `ValidateLocation`. Điểm đi và điểm đến không được trùng cả tỉnh, phường lẫn tên.
    - Đổi tỉnh hoặc phường của điểm đi hay điểm đến khi listing còn chuyến tương lai có `seats_reserved > 0` thì 409 `has_active_bookings` (`ErrHasActiveBookings`).
  - `func (s *Service) RideDetail(ctx, tx pgx.Tx, listingID uuid.UUID, includeInactive bool) (RideDetail, error)`, cùng `GetRide(ctx, partnerID, listingID)` cho đối tác.
  - `func (s *Service) CreateRideSchedule(ctx, partnerID, actor, listingID uuid.UUID, in RideScheduleInput) (RideSchedule, error)`:
    - `seat_capacity` 1..80, giá 1.000..100.000.000, `valid_from ≥` hôm nay (VN).
    - Chưa có tuyến thì `ErrRouteRequired`.
    - Một lịch active khác có cùng `departure_time`, có ngày trong tuần giao nhau và khoảng hiệu lực giao nhau thì `ErrOverlappingSchedule`.
    - Sinh chuyến ngay trong cùng transaction (`generateForSchedule`).
  - `func (s *Service) UpdateRideSchedule(ctx, partnerID, actor, listingID, scheduleID uuid.UUID, in RideSchedulePatch) (RideSchedule, error)`. Áp cho các chuyến **tương lai** (`departure_at > now`), `status = 'scheduled'`, `seats_reserved = 0` của lịch đó:
    - Đổi giá hoặc sức chứa: cập nhật tại chỗ.
    - Tắt lịch, hoặc `valid_to` lùi lại: chuyến nằm ngoài khoảng mới thì chuyển `cancelled`, với `cancel_source = 'schedule'` và lý do "Lịch chạy đã thay đổi".
    - Bật lại lịch, hoặc nới `valid_to`: đặt `generated_until = NULL` rồi sinh lại ngay.
    - Chuyến đã có ghế được giữ thì **không đổi**.
  - `func (s *Service) GenerateTrips(ctx context.Context) (int, error)` và `generateForSchedule(ctx, tx, sched, now) (int, error)`:
    - Mỗi lịch active chạy trong một transaction riêng, khóa dòng `ride_schedules FOR UPDATE`. Lịch lỗi thì log rồi chạy tiếp.
    - Khoảng sinh: `from = max(valid_from, hôm nay VN, generated_until + 1)`, `to = min(valid_to, hôm nay VN + 59)`, tức **60 ngày**, hằng `tripHorizonDays = 60`.
    - Với mỗi ngày có bit `weekdayBit`: `departure_at = atVN(date, departure_time)`. Bỏ qua nếu `departure_at ≤ now`.
    - `INSERT ... ON CONFLICT (listing_id, departure_at) DO UPDATE SET status='scheduled', schedule_id, seat_capacity, price_per_seat = EXCLUDED.*, cancelled_* = NULL, cancel_source = NULL WHERE ride_trips.status = 'cancelled' AND ride_trips.cancel_source = 'schedule'`. Chuyến đang chạy, hoặc chuyến do đối tác hay admin hủy, thì giữ nguyên.
    - Cuối cùng đặt `generated_until = to`. Hàm trả về số chuyến đã chèn hoặc mở lại.
  - `GenerateTripsArgs{}` (Kind `listing_generate_trips`), `GenerateTripsWorker{Service *Service}`, và `func PeriodicJobs() []*river.PeriodicJob` chạy mỗi giờ, `MaxAttempts 1`, `RunOnStart true`.
  - `requireComplete` cho `scheduled_ride` thay ba mục "tỉnh/thành, phường/xã, địa chỉ" của listing bằng các mục sau:
    - "tuyến (điểm đi, điểm đến)"
    - "ít nhất 1 lịch chạy đang hoạt động"

    Vẫn cần tiêu đề, mô tả và ít nhất 1 ảnh.
  - Routes đối tác (`withID`, listing phải là `scheduled_ride`, nếu không thì `ErrNotRideListing`):
    - `GET /v1/partner/listings/{id}/ride`
    - `PUT /v1/partner/listings/{id}/ride-route`
    - `POST /v1/partner/listings/{id}/ride-schedules`
    - `PATCH /v1/partner/listings/{id}/ride-schedules/{scheduleID}`

- [ ] **Step 1: Viết test fail** `ride_test.go`. Clock cố định `2026-11-05T09:00:00+07:00` (thứ Năm). Tuyến TP.HCM (79) → Lâm Đồng (68), lấy phường bằng helper `location`.
  - `TestSetRideRoute_UpsertAndAudit`.
  - `TestSetRideRoute_WardNotInProvince422`.
  - `TestSetRideRoute_OnAttraction422`: lỗi `wrong_service_type`.
  - `TestSetRideRoute_ChangeOriginWithBookedTrip409`.
  - `TestCreateSchedule_NeedsRoute422`.
  - `TestCreateSchedule_Overlap422`.
  - `TestCreateSchedule_GeneratesSixtyDays`: lịch 07:00 mỗi ngày từ 2026-11-05. Kỳ vọng chuyến đầu tiên là 2026-11-06 07:00+07 (07:00 hôm nay đã qua), chuyến cuối là 2027-01-03 07:00+07, tức hôm nay + 59. Tổng 59 chuyến.
  - `TestUpdateSchedule_PriceAppliesToUnbookedOnly`: chuyến có `seats_reserved = 2` giữ giá cũ, các chuyến khác lấy giá mới.
  - `TestUpdateSchedule_DeactivateThenReactivateReopens`: tắt lịch thì chuyến chưa ai đặt thành `cancelled` (`schedule`). Bật lại thì các chuyến đó về `scheduled`. Chuyến do đối tác tự hủy vẫn `cancelled`.
  - `TestUpdateSchedule_ImmutableFields422`.
  - `TestSubmit_RideCompleteness`: thiếu tuyến và lịch thì message liệt kê đúng hai mục đó, không đòi địa chỉ của listing.
  - HTTP: `TestHTTP_RideRoutes_MalformedID404`, `TestHTTP_RideSchedule_UnknownField400`.
- [ ] **Step 2: Viết test fail** `ride_jobs_test.go`:
  - `TestGenerateTrips_Idempotent`: chạy hai lần ra cùng số dòng, lần hai trả 0.
  - `TestGenerateTrips_AcrossMidnightVN` (Review Focus 3): lịch 23:45 chỉ chạy thứ Hai và lịch 00:15 chỉ chạy thứ Ba.
    - Chuyến của thứ Hai 2026-11-09 có `departure_at = 2026-11-09T16:45Z`.
    - Chuyến của thứ Ba 2026-11-10 lúc 00:15 có `departure_at = 2026-11-09T17:15Z`, và không chuyến nào bị sinh vào thứ Hai lúc 00:15.
  - `TestGenerateTrips_RespectsValidTo`.
  - `TestGenerateTrips_SkipsAdHocConflict`: một chuyến lẻ trùng giờ vẫn giữ nguyên `schedule_id NULL`.
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt.** Thêm 4 route vào `partnerRoutes`.
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(listing): ride routes, recurring schedules and trip generation`.

---

### Task 3: Chuyến: đối tác quản lý, các hàm giữ ghế cho booking

**Files:**
- Create: `internal/listing/{ride_trips.go,ride_trips_test.go}`
- Modify: `internal/listing/{http_ride.go,http_admin.go,queries.sql}`, `cmd/api/routes_test.go`

**Interfaces:**
- Produces:
  - `type Trip struct { ID, ListingID uuid.UUID; ScheduleID *uuid.UUID; DepartureAt time.Time; SeatCapacity, SeatsReserved int; PricePerSeat int64; Status string; CancelledReason *string }` và `func (t Trip) SeatsLeft() int`.
  - `ErrTripNotFound` (404 `not_found`), `ErrTripHasBookings` (409 `trip_has_bookings`), `ErrCapacityBelowReserved` (409 `capacity_below_reserved`), `ErrTripTimeTaken` (409 `trip_time_taken`), `ErrTripNotScheduled` (409 `trip_not_scheduled`).
  - Đối tác (khóa partner `FOR SHARE` → listing `FOR NO KEY UPDATE` → trip `FOR UPDATE`):
    - `func (s *Service) ListTrips(ctx, partnerID, listingID uuid.UUID, from, to time.Time) ([]Trip, error)`. Khoảng tối đa 62 ngày, sắp theo `departure_at`.
    - `func (s *Service) CreateTrip(ctx, partnerID, actor, listingID uuid.UUID, in TripInput) (Trip, error)`. Tạo chuyến lẻ (`schedule_id NULL`), với `TripInput{DepartureAt time.Time; SeatCapacity int; PricePerSeat int64}`. Giờ khởi hành phải ở tương lai và trong vòng 365 ngày. Trùng giờ thì `ErrTripTimeTaken`.
    - `func (s *Service) UpdateTrip(ctx, partnerID, actor, listingID, tripID uuid.UUID, in TripPatch) (Trip, error)` với `TripPatch{DepartureAt *time.Time; SeatCapacity *int; PricePerSeat *int64}`:
      - Chuyến phải `scheduled`, nếu không thì `ErrTripNotScheduled`.
      - Đổi giờ hoặc giá khi `seats_reserved > 0` thì `ErrTripHasBookings` (app kiểm tra trước trigger).
      - Sức chứa mới nhỏ hơn `seats_reserved` thì `ErrCapacityBelowReserved`.
      - Ghi audit `ride_trip`.
    - `func (s *Service) CancelTripByPartner(ctx, partnerID, actor, listingID, tripID uuid.UUID, reason string) (Trip, error)`. Chỉ được khi `seats_reserved = 0`, ghi `cancel_source = 'partner'`, `reason` 1..500.
  - Cho `booking` (nhận `tx` của người gọi, không tự mở transaction):
    - `func (s *Service) RideRoute(ctx, tx pgx.Tx, listingID uuid.UUID) (RideRoute, error)`.
    - `func (s *Service) TripListingIDs(ctx, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)`. Đọc không khóa. Thiếu id nào thì `ErrTripNotFound`.
    - `func (s *Service) LockTrips(ctx, tx pgx.Tx, ids []uuid.UUID) ([]Trip, error)`. `SELECT ... FOR UPDATE ORDER BY id`, tự loại id trùng. Thiếu id thì `ErrTripNotFound`.
    - `func (s *Service) ReserveSeats(ctx, tx pgx.Tx, tripID uuid.UUID, n int, now time.Time) (bool, error)`. Đúng lệnh UPDATE có điều kiện ở Global Constraints. Trả `false` khi 0 dòng bị ảnh hưởng.
    - `func (s *Service) ReleaseSeats(ctx, tx pgx.Tx, tripID uuid.UUID, n int) error`. `seats_reserved = seats_reserved - n`, áp cả cho chuyến đã hủy. CHECK bắt trường hợp âm.
    - `func (s *Service) CancelTripByAdmin(ctx, tx pgx.Tx, tripID, actor uuid.UUID, reason string, now time.Time) error`. Chuyển `cancelled` với `cancel_source = 'admin'` bất kể ghế. Người gọi đã giữ khóa. Chuyến không `scheduled` thì `ErrTripNotScheduled`.
  - Admin: `func (s *Service) AdminListTrips(ctx, listingID uuid.UUID, from, to time.Time) ([]Trip, error)`. `AdminGet` của listing có thêm `Ride *RideDetail`.
  - Routes:
    - Đối tác:
      - `GET /v1/partner/listings/{id}/ride-trips?from&to` (RFC 3339 hoặc `YYYY-MM-DD`)
      - `POST /v1/partner/listings/{id}/ride-trips`
      - `PATCH /v1/partner/listings/{id}/ride-trips/{tripID}`
      - `POST /v1/partner/listings/{id}/ride-trips/{tripID}/cancel`
    - Admin: `GET /v1/admin/listings/{id}/ride-trips?from&to`.

- [ ] **Step 1: Viết test fail** `ride_trips_test.go`:
  - `TestUpdateTrip_ShrinkBelowReserved409`: sức chứa 10, đã giữ 4. Đổi sức chứa thành 3 thì 409, thành 4 thì được.
  - `TestUpdateTrip_TimeOrPriceWithBookings409`.
  - `TestUpdateTrip_NoBookingsOK`.
  - `TestCancelTripByPartner_WithSeats409` và `TestCancelTripByPartner_EmptyOK`.
  - `TestTripOtherPartner404`: đối tác B sửa chuyến của A thì 404.
  - `TestCreateTrip_TimeTaken409`.
  - `TestReserveSeats_Conditional`: sức chứa 3. Giữ 2 thì true, giữ thêm 2 thì false, giữ 1 thì true. Chuyến đã hủy thì false, `departure_at ≤ now` thì false.
  - `TestReleaseSeats_BelowZeroFails`.
  - `TestLockTrips_OrderedAndDeduped`.
  - `TestSuspendedPartnerCannotEditTrips`: 403 `partner_suspended`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Thêm 4 route đối tác và 1 route admin vào danh sách route.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(listing): ride trip management and seat reservation primitives`.

---

### Task 4: Tìm chuyến public và trang chi tiết xe tuyến

**Files:**
- Create: `internal/listing/{ride_search.go,ride_search_test.go}`
- Modify: `internal/listing/{public.go,http_public.go,queries.sql}`

**Interfaces:**
- Produces:
  - `type TripSearchFilter struct { OriginProvince, DestProvince string; OriginWard, DestWard *string; Date string /*YYYY-MM-DD, VN*/; Seats, Limit, Offset int }`.
  - `type TripCard struct { TripID uuid.UUID; Listing ListingRef{ID, Slug, Title}; Origin, Dest RidePoint; DepartureAt, ArriveAt time.Time; DurationMinutes int; VehicleDescription string; PricePerSeat int64; SeatsLeft int }`.
  - `func (s *Service) SearchTrips(ctx context.Context, f TripSearchFilter) ([]TripCard, error)`:
    - Hai mã tỉnh là bắt buộc. Phường không bắt buộc.
    - `Date` không được trước hôm nay (VN) và không quá hôm nay + 365.
    - `Seats` là **số khách** (query `passengers`), mặc định 1, phải ≥ 1 và ≤ 1000 (chỉ chặn đầu vào vô lý). Limit mặc định 20, tối đa 50, offset tối đa 1000.
    - Điều kiện:
      - `ride_trips.status = 'scheduled'`
      - `departure_at ∈ [00:00 VN của Date, 00:00 VN hôm sau)`
      - `departure_at > now + max(ride_booking_cutoff_minutes, hold_minutes + hold_grace_minutes)`, tức đúng điều kiện đặt được ở Task 5 (đọc `settings`)
      - `seat_capacity − seats_reserved ≥ Seats`
      - listing nằm trong `visible_listings`
    - Sắp theo `departure_at`, rồi `price_per_seat`, rồi `id`. Sai tham số thì `httpx.ErrInvalidRequest`.
  - `SearchPublicListings`/`GetPublicListingBySlug`: `price_from` của xe tuyến là giá thấp nhất trong các lịch active, dạng `COALESCE((min vé), (min lịch), 0)`.
  - `PublicDetail` có thêm `Ride *RideDetail` (chỉ lịch active). Nhánh `scheduled_ride` trong `switch` không còn trả `ErrServiceTypeNotSupported`.
  - Route public: `GET /v1/ride-trips?origin_province=&dest_province=&date=&origin_ward=&dest_ward=&passengers=&limit=&offset=`.

- [ ] **Step 1: Viết test fail** `ride_search_test.go`:
  - `TestSearchTrips_MatchesProvincesAndSeatsLeft`: chuyến đã đầy không hiện. Chuyến còn 1 ghế hiện với `seats=1` và không hiện với `seats=2`.
  - `TestSearchTrips_DateIsVNDepartureDate` (Review Focus 3): chuyến 23:45 thứ Hai hiện khi tìm thứ Hai. Chuyến 00:15 thứ Ba chỉ hiện khi tìm thứ Ba.
  - `TestSearchTrips_HidesSoonDeparting`: now 09:00, chuyến 09:15 không hiện, chuyến 09:25 hiện.
  - `TestSearchTrips_HidesInvisibleListings`: đối tác bị đình chỉ, listing bị ẩn, hoặc loại dịch vụ bị tắt thì không hiện.
  - `TestSearchTrips_WardFilter`.
  - `TestSearchTrips_BadParams400`: thiếu tỉnh, ngày sai định dạng, `seats=11`.
  - `TestPublicDetail_Ride` và `TestSearch_RidePriceFrom`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(listing): public trip search and ride listing detail`.

---

### Task 5: Đặt vé xe tuyến (một chiều và khứ hồi), nhả giữ chỗ quá hạn, hết hạn, các màn xem

**Files:**
- Create: `internal/booking/{create_ride.go,ride_seats.go,ride_helpers_test.go,create_ride_test.go}`
- Modify: `internal/booking/{create.go,jobs.go,views.go,http_customer.go,http_partner.go,queries.sql}`

**Interfaces:**
- Consumes: `listing.BookableListing`, `RideRoute`, `TripListingIDs`, `LockTrips`, `ReserveSeats`, `ReleaseSeats`, `CancellationPolicy`; `catalog.CurrentCommissionRate`; `settings.GetTx`; `payment.ReservePayment`/`ActivateLink`.
- Produces:
  - `create.go` được tổng quát hóa (giữ nguyên hành vi Attraction): `requestHash(endpoint string, in any)`, `replayed(ctx, q, customerID, key, endpoint string, hash []byte) (json.RawMessage, error)`. Thêm hằng `endpointRide = "POST /v1/bookings/ride"`.
  - `type CreateRideInput struct { OutboundTripID uuid.UUID; ReturnTripID *uuid.UUID; Seats int; ContactName, ContactPhone, ContactEmail string; Note *string }`.
    - `Seats` ≥ 1, không có trần cố định (đã chốt): giới hạn thực tế là số ghế còn trống, `ReserveSeats` trả `false` thì `ErrNotEnoughSeats`. Hai chiều có cùng số ghế.
    - Thông tin liên hệ chuẩn hóa giống Attraction.
  - `type CheckoutView struct { ID uuid.UUID; Code, Status string; TotalAmount int64; Bookings []BookingView }`. Thứ tự là chiều đi rồi chiều về.
  - `ErrTripUnavailable` (422 `trip_unavailable`), `ErrNotEnoughSeats` (409 `not_enough_seats`), `ErrInvalidReturnTrip` (422 `invalid_return_trip`).
  - `func (s *Service) CreateRide(ctx, customerID uuid.UUID, idemKey string, in CreateRideInput) (CheckoutView, error)`. Trong **một** transaction, theo đúng thứ tự:
    1. Giữ Idempotency-Key, rồi `FillProfileIfEmpty`.
    2. `TripListingIDs`, sau đó `BookableListing` cho từng listing theo id tăng dần. Listing phải là `scheduled_ride`, nếu không thì `ErrWrongServiceType`.
    3. Khứ hồi:
       - Hai listing khác nhau, **cùng `partner_id`**.
       - Tuyến ngược theo tỉnh: `return.origin_province = outbound.dest_province` và `return.dest_province = outbound.origin_province`.
       - Sai thì `ErrInvalidReturnTrip`.
    4. `LockTrips(tất cả trip id)`, rồi `releaseOverdueOnTrips(tx, tripIDs, uuid.Nil, now)`.
    5. Mỗi chuyến phải `scheduled` và có `departure_at > now + max(ride_booking_cutoff_minutes, hold_minutes + hold_grace_minutes)` (mặc định now + 60'), nếu không thì `ErrTripUnavailable`. Chiều về phải có `departure_at ≥ outbound.departure_at + outbound duration`, nếu không thì `ErrInvalidReturnTrip`.
    6. `ReserveSeats` cho từng chuyến. `false` thì `ErrNotEnoughSeats`.
    7. Hoa hồng, settings, chính sách hủy của **từng listing**. Insert checkout (`total = Σ gross`), hai booking `pending_payment` có cùng hold.
       - `service_start_at = departure_at`, `service_end_at = departure_at + duration`.
       - `booking_items`: `line_kind = 'seat'`, `description = "Ghế {origin_name} → {dest_name}, {15:04 02/01/2006}"`, `unit_price = price_per_seat`, `quantity = seats`.
       - `booking_ride_details` (`direction` là `outbound`/`return`, snapshot tên điểm), history.
    8. `ReservePayment(checkout, total, holdExpires)`.

    Sau commit thì `ActivateLink`. Response lưu với key là `{"checkout_id","booking_ids"}`, và replay trả lại `CheckoutView` mới đọc.
  - `func (s *Service) releaseOverdueOnTrips(ctx, tx pgx.Tx, tripIDs []uuid.UUID, exceptCheckout uuid.UUID, now time.Time) (cancelCodes []int64, err error)`:
    - Người gọi đã khóa các chuyến.
    - Liệt kê booking `pending_payment` có `hold_release_at ≤ now` trên các chuyến này (query `ListOverdueRideHolds`, join `booking_ride_details`), bỏ qua `exceptCheckout`.
    - Khóa các checkout liên quan theo id tăng dần, rồi các booking theo id tăng dần (`LockBookingsByIDs`). Kiểm tra lại trạng thái.
    - Với từng booking đó: `expired` cùng history "hết thời gian giữ chỗ", và `ReleaseSeats`.
    - Checkout không còn booking nào chưa hết hạn thì: payment `pending` thành `expired`, checkout `closed`, trả về order code. Người gọi hủy link **sau commit**.
    - Chiều còn lại của các checkout đó, nếu nằm trên chuyến chưa khóa, thì **không** đụng tới.
  - `func (s *Service) releaseRideSeats(ctx, tx pgx.Tx, b store.Booking) error`: đọc `booking_ride_details` rồi `ReleaseSeats`. Không phải xe tuyến thì không làm gì.
  - `ExpireHolds`/`expireCheckout`: trước `LockCheckout`, đọc trip id của các booking xe tuyến trong checkout (`ListCheckoutRideTrips`, không khóa) rồi `LockTrips`. Mỗi booking chuyển `expired` thì gọi `releaseRideSeats`.
  - Views:
    - `BookingView` có thêm `Ride *RideView`, với `RideView{TripID uuid.UUID; Direction string; Seats int; OriginName, DestName string; DepartureAt, ArriveAt time.Time; PairedBookingID *uuid.UUID}`.
    - `qr_token` hiện khi trạng thái là `confirmed`, `checked_in` hoặc `completed`.
    - Xe tuyến có `can_reschedule = false`. `can_cancel` theo Task 7. Tạm thời giữ `cancelBlock` hiện có.
  - `ListFilter` có thêm `TripID *uuid.UUID`, đọc từ query `trip_id` của `GET /v1/partner/bookings` (danh sách khách của một chuyến).
  - Route khách: `POST /v1/bookings/ride` (header `Idempotency-Key`, trả 201 với `CheckoutView`).

- [ ] **Step 1: Viết helper** `ride_helpers_test.go`:
  - Đối tác có hai listing đã duyệt: A (79 → 68, chạy 180 phút) và B (68 → 79, chạy 180 phút), hoa hồng `scheduled_ride` 1000 bps, chính sách hủy `[{48,10000},{24,5000}]`.
  - Chuyến A1: 2026-11-07 07:00, 250.000 đ/ghế, 10 ghế. Chuyến B1: 2026-11-09 13:00, 220.000 đ/ghế, 10 ghế.
  - Helper `rideInput(outbound, ret, seats)`.
- [ ] **Step 2: Viết test fail** `create_ride_test.go`:
  - `TestCreateRide_OneWayHoldsSeats`: 2 ghế A1. Kỳ vọng `gross 500.000`, `commission 50.000`, `seats_reserved = 2`, `service_end_at = 2026-11-07 10:00+07`, item `seat` có `quantity 2`, hold 15 và 20 phút.
  - `TestCreateRide_RoundTripOneCheckout`: A1 cộng B1 với 2 ghế. Kỳ vọng hai booking chung checkout, `total 940.000` (500.000 + 440.000), một payment 940.000, mỗi chiều có QR token riêng, `paired_booking_id` trỏ chéo nhau.
  - `TestCreateRide_Idempotent`: cùng key thì cùng checkout. Cùng key khác body thì 422.
  - `TestCreateRide_LastSeatRace` (Review Focus 1).
  - `TestCreateRide_ReleasesOverdueHoldOnTrip` (Review Focus 2), cùng biến thể khứ hồi có chiều kia nằm trên chuyến chưa khóa.
  - `TestCreateRide_ReturnBeforeArrival422` (Review Focus 3): chuyến đi 22:00 cộng 180 phút. Chiều về 00:30 hôm sau thì 422, 01:00 thì 201.
  - `TestCreateRide_ReturnNotReverseOrOtherPartner422`.
  - `TestCreateRide_BookingCutoff`: now 09:00, chuyến 09:59 thì 422 `trip_unavailable`, chuyến 10:01 thì 201. Đặt `ride_booking_cutoff_minutes = 0` thì chuyến 09:21 đặt được, 09:19 thì 422 (vẫn tôn trọng hold + grace).
  - `TestCreateRide_CancelledTrip422`.
  - `TestCreateRide_PartnerSuspended422`: `listing_not_bookable`, và không ghế nào bị giữ.
  - `TestCreateRide_SeatsBounds`: 0 ghế thì 422. Đặt đúng 10 ghế (cả chuyến) thì 201; 11 ghế trên chuyến 10 ghế thì 409 `not_enough_seats`, không ghế nào bị giữ.
  - `TestExpireHolds_RideReleasesSeats`: tại hold + 6 phút, cả hai chiều `expired`, ghế về 0 trên cả hai chuyến, payment `expired`.
  - `TestRideView_NoRescheduleAndQR`: `can_reschedule = false`, và QR chỉ hiện sau khi xác nhận (dùng helper `confirm`).
- [ ] **Step 3: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 4: Cài đặt.** Test Attraction cũ (`create_test.go`, `jobs_test.go`) phải vẫn PASS sau khi tổng quát hóa.
- [ ] **Step 5: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 6: Commit** `feat(booking): scheduled ride booking with seat holds and round trips`.

---

### Task 6: Xác nhận thanh toán và tiền về trễ cho xe tuyến

**Files:**
- Create: `internal/booking/confirm_ride_test.go`
- Modify: `internal/booking/{confirm.go,ride_seats.go,queries.sql}`

**Interfaces:**
- Consumes: Task 5 (`releaseOverdueOnTrips`, `releaseRideSeats`, `ListCheckoutRideTrips`), `listing.LockTrips`/`ReserveSeats`.
- Produces (sửa `HandlePaid`, không tạo đường mới):
  - `ListCheckoutBookings` có thêm `LEFT JOIN booking_ride_details`, trả thêm `trip_id`, `seats`.
  - `lateBooking(b, now)` có thêm điều kiện: booking xe tuyến `pending_payment` mà `service_start_at ≤ now` cũng là đơn trễ.
  - Thứ tự mới trong `HandlePaid`:
    1. `ListCheckoutBookings` (không khóa).
    2. `reconfirmChecks` gọi `BookableListing` cho các listing có booking trễ, như cũ.
    3. Nếu checkout có booking xe tuyến: `LockTrips(mọi trip của checkout)`, rồi `releaseOverdueOnTrips(..., except = ev.CheckoutID)`. Gom `cancelCodes` vào `PaidOutcome.CancelOrderCodes` để hủy link sau commit.
    4. Đọc lại `ListCheckoutBookings`.
    5. `rideChecks`.
    6. Các bước khóa checkout, booking, payment và quyết định trạng thái giữ nguyên như plan 4.
  - `func (s *Service) rideChecks(ctx, tx pgx.Tx, rows []store.ListCheckoutBookingsRow, trips map[uuid.UUID]listing.Trip, rebookable map[uuid.UUID]bool, now time.Time) error` ghi vào `rebookable` một mục cho **mọi** booking xe tuyến:
    - `pending_payment` chưa trễ: được nếu chuyến `scheduled`, `trip.departure_at == service_start_at` và `departure_at > now`.
    - Trễ (`expired`, hoặc `pending_payment` đã qua `hold_release_at`): được nếu listing bookable (bước 2), các điều kiện chuyến ở trên đúng, và ghế còn đủ.
      - `pending_payment` thì ghế vẫn đang được giữ, không cần giữ lại.
      - `expired` thì cần `seat_capacity − seats_reserved ≥ seats` khi đang giữ khóa chuyến.
      - Giờ khởi hành phải khớp giờ đã bán, tương tự Ruling 25.
    - Booking trễ mà ở bước 2 chưa được kiểm tra thì `errStateChanged`.
    - **Cả hai chiều hoặc không chiều nào:** nếu có một booking xe tuyến của checkout không được thì mọi booking xe tuyến của checkout đều không được.
  - `confirm()` và `confirmation`:
    - `expired` mà được thì gọi `ReserveSeats` trước `confirmBooking`. `false` là lỗi nội bộ (không thể xảy ra vì đang giữ khóa).
    - `pending_payment` mà không được thì `expireBooking` kèm `releaseRideSeats`.
    - `refundExplanation(late_payment)` vẫn dùng câu "chỗ không còn đặt được".
  - Xe tuyến luôn có `ledger.PostBookingConfirmed` riêng cho từng chiều (`postLedger` đã sắp theo partner rồi id).

- [ ] **Step 1: Viết test fail** `confirm_ride_test.go`:
  - `TestRidePaid_ConfirmsBothLegs`: khứ hồi 940.000. Kỳ vọng hai booking `confirmed`, pending của đối tác `846.000` (450.000 + 396.000), sổ sàn +94.000, `checkouts.paid_payment_id` được đặt, `Verify` không lỗi.
  - `TestRideLatePaid_ReconfirmsAndReReserves` (Review Focus 5): hết hạn, ghế về 0, tiền về. Kỳ vọng `reconfirmed`, ghế lại là 2 và 2, có bút toán cho cả hai chiều.
  - `TestRideLatePaid_SeatsResoldRefundsBoth` (Review Focus 5): A1 bị người khác mua hết 10 ghế. Kỳ vọng hai refund `late_payment` (500.000 và 440.000) gắn từng booking, không có dòng nào trong `ledger_entries`, ghế của A1 vẫn 10 và của B1 vẫn 0, payment `paid`.
  - `TestRideLatePaid_TripCancelledRefunds`.
  - `TestRideLatePaid_TripTimeChangedRefunds`: sau khi hết hạn, đối tác đổi giờ chuyến (đã được vì ghế = 0).
  - `TestRideOnTimePaid_TripCancelledByAdminRefunds`: đơn còn `pending_payment`, chuyến bị admin hủy. Kỳ vọng đơn `expired`, ghế được trả, refund `late_payment`.
  - `TestRideLatePaid_PendingPastReleaseKeepsSeats`: job chưa chạy. Kỳ vọng xác nhận mà **không** giữ thêm ghế (`seats_reserved` vẫn là 2).
  - `TestRidePaid_ReleasesOtherOverdueHolds`: một đơn khác đã quá hạn trên A1 bị `expired` trong transaction của webhook, và link của nó có trong `CancelOrderCodes`.
  - `TestRideWebhook_DuplicateDelivery`: chỉ một lần xác nhận, một cặp bút toán.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Toàn bộ `confirm*_test.go` của Attraction phải vẫn PASS.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): ride payment confirmation and late re-confirm with seat re-reservation`.

---

### Task 7: Hủy khứ hồi, admin hủy đơn xe tuyến, admin hủy chuyến

**Files:**
- Create: `internal/booking/{cancel_ride.go,cancel_ride_test.go}`
- Modify: `internal/booking/{cancel.go,views.go,http_admin.go,queries.sql}`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: `listing.LockTrips`, `ReleaseSeats`, `CancelTripByAdmin`; `ledger.PostRefund`/`PostCommissionWaiver`/`Release`; `payment.CreateRefund`/`SaveRefundAccountTx`.
- Produces:
  - Refactor `cancel.go`: tách phần chạy **sau khi đã khóa** thành `func (s *Service) cancelLocked(ctx, tx pgx.Tx, co store.Checkout, b store.Booking, c cancellation, now time.Time) (int64, error)`.
    - Hàm này làm: tính refund, `CancelBooking`, history, `CreateRefund`, `PostRefund`, waiver, `Release`, `releaseRideSeats`, audit `booking.cancel` (admin) và email. Trả về số tiền hoàn.
    - Attraction vẫn gọi qua `cancel()` cũ. Test cũ phải PASS nguyên vẹn.
  - `func rideCancelBlock(legs []store.Booking, now time.Time, byCustomer bool) error`:
    - Khách: mọi chiều **chưa hủy** của checkout đều `confirmed` và `now < min(service_start_at)`. Nếu không thì 409 `invalid_booking_state` với message "Chỉ hủy được khi chưa đi chiều nào".
    - Admin: chiều được chọn phải `confirmed` và `now < service_end_at` (Ruling 21).
    - `BookingView.can_cancel` của xe tuyến dùng đúng hàm này (với khách).
  - `CancelByCustomer` và `CancelByAdmin` khi booking là xe tuyến thì rẽ sang `cancelRideCheckout(ctx, bookingID, owner *uuid.UUID, c cancellation) error`:
    - Thứ tự khóa: đọc không khóa, rồi `SaveRefundAccountTx` (khách), `LockTrips(mọi trip của checkout)`, `LockCheckout`, `LockCheckoutBookings`, kiểm tra lại `rideCancelBlock`.
    - Mỗi chiều hủy được gọi `cancelLocked`. % hoàn tính theo `original_service_start_at` của **chính chiều đó**.
    - Admin: các chiều đã `checked_in`, `completed` hoặc `no_show` thì giữ nguyên.
    - Một tài khoản ngân hàng dùng cho mọi refund của lần hủy đó.
  - `type TripCancelResult struct { TripID uuid.UUID; CancelledBookingIDs []uuid.UUID; RefundTotal int64 }`.
  - `func (s *Service) CancelTrip(ctx, actor, tripID uuid.UUID, reason string) (TripCancelResult, error)`. `reason` 1..500, luôn ở chế độ `force_majeure`.
    1. Đọc không khóa các checkout có booking trên chuyến (`ListTripCheckouts`) và mọi trip của các checkout đó.
    2. `LockTrips(tất cả, id tăng dần)`, rồi `CancelTripByAdmin`.
    3. Khóa các checkout theo id tăng dần, rồi `LockBookingsByIDs` (id tăng dần), rồi đọc lại.
    4. Mỗi booking xe tuyến `confirmed` của các checkout đó, nếu thỏa luật admin, thì `cancelLocked` (hoàn 100%, miễn hoa hồng, `cancel_kind = admin_force_majeure`).
    5. Booking `pending_payment` thì để job hết hạn. Tiền về sau đó bị hoàn theo Task 6.
    6. `audit.Record("ride_trip.cancel", target ride_trip)` với `before`/`after` là `{status, seats_reserved, cancelled_booking_ids, refund_total}`.
  - Route admin: `POST /v1/admin/ride-trips/{id}/cancel` với body `{reason}`, trả 200 `TripCancelResult`. Chuyến không tồn tại thì 404, chuyến đã hủy thì 409 `trip_not_scheduled`.

- [ ] **Step 1: Viết test fail** `cancel_ride_test.go`. Khứ hồi A1 (500.000) cộng B1 (440.000) đã xác nhận, hoa hồng 10%.
  - `TestCancelRide_CustomerRoundTripPerLegPolicy`: now 2026-11-05 09:00, A1 còn 46 giờ (hoàn 50% = 250.000), B1 còn hơn 48 giờ (hoàn 100% = 440.000).
    - Kỳ vọng hai refund `pending` khi có bank, ghế của cả hai chuyến về 0.
    - Available của đối tác = (450.000 − 250.000) + (396.000 − 440.000) = **156.000**, pending 0, `Verify` không lỗi.
  - `TestCancelRide_AfterOutboundDeparted409`: lúc A1 + 1 phút, kể cả khi hủy qua id của chiều về.
  - `TestCancelRide_OneWay`.
  - `TestCancelRide_OtherCustomer404`.
  - `TestCancelRideByAdmin_ForceMajeureBothLegs`: refund 500.000 và 440.000, đối tác 0, sàn net 0, có audit cho mỗi booking.
  - `TestCancelTrip_RoundTripCancelsBothLegs` (Review Focus 4): hủy B1. Kỳ vọng cả hai chiều bị hủy, ghế của A1 và B1 về 0, B1 `cancelled` (`admin`), audit `ride_trip.cancel`, `Verify` không lỗi.
  - `TestCancelTrip_OutboundCheckedInOnlyReturnCancelled` (Review Focus 4): A1 đã `checked_in`. Kỳ vọng chỉ B1 bị hủy, A1 vẫn `checked_in` và còn pending 450.000.
  - `TestCancelTrip_ManyCheckouts`: 3 checkout trên cùng chuyến (một trong đó còn `pending_payment`). Kỳ vọng 2 đơn bị hủy, đơn pending giữ nguyên.
  - `TestCancelTrip_AlreadyCancelled409` và `TestCancelTrip_MalformedID404`.
  - `TestCanCancelView_Ride`: `can_cancel` true trước giờ đi của A1, false sau đó, trên **cả hai** booking.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Thêm route vào `adminRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. `cancel_test.go` và `cancel_fix_test.go` cũ cũng phải PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): round-trip cancellation and admin trip cancellation`.

---

### Task 8: Check-in theo cửa sổ giờ, No-show, hoàn thành, chặn đổi lịch

**Files:**
- Create: `internal/booking/ride_lifecycle_test.go`
- Modify: `internal/booking/{checkin.go,jobs.go,reschedule.go,queries.sql}`

**Interfaces:**
- Produces:
  - `ErrCheckInOutsideWindow` (422 `checkin_outside_window`, "Chỉ check-in được trong khoảng ±{X} phút quanh giờ khởi hành").
  - `checkIn` rẽ nhánh theo `service_type`. Attraction giữ nguyên. `scheduled_ride`:
    - Booking phải `confirmed`, nếu không thì `ErrCheckInNotAllowed`.
    - `|now − service_start_at| ≤ ride_checkin_window_minutes` (cả hai biên đều tính là trong cửa sổ). Sai thì `ErrCheckInOutsideWindow`.
    - Query `CheckInRide`: `status = 'checked_in'`, `checked_in_at = now`. Ghi history với actor là đối tác.
    - **Không** lên lịch release ở bước này. Quét lại thì trả `Already`.
  - `MarkNoShows` thêm danh sách `ListRideNoShowCandidates(cutoff = now − ride_no_show_after_minutes, lim, skip)`:
    - Điều kiện `status = 'confirmed' AND service_type = 'scheduled_ride' AND service_start_at < cutoff` (dùng `bookings_ride_no_show_idx`).
    - Trong transaction kiểm tra lại, rồi `MarkNoShow` và `scheduleRelease(now)`. Hai danh sách dùng chung `noShowBackoff`.
  - `CompleteRidesArgs{}` (Kind `booking_complete_rides`), `CompleteRidesWorker`, `func (s *Service) CompleteRides(ctx) (int, error)`:
    - Điều kiện `status = 'checked_in' AND service_type = 'scheduled_ride' AND service_end_at <= now` (dùng `bookings_service_end_idx`).
    - Khóa booking, kiểm tra lại, rồi `CompleteRide` (`completed`, `completed_at = now`), history actor `system`, `scheduleRelease(now)`.
    - Có tracker `completeBackoff` riêng. Thêm vào `PeriodicJobs()`.
  - `rescheduleBlock` đã chặn loại khác Attraction. Chỉ cần thêm test.

- [ ] **Step 1: Viết test fail** `ride_lifecycle_test.go`. Chuyến A1 lúc 07:00, chạy 180 phút, window 60, no-show 60.
  - `TestRideCheckIn_WindowBounds`: 05:59 thì 422, 06:00 thì OK. Một booking khác: 08:00 thì OK, 08:01 thì 422.
  - `TestRideCheckIn_SetsCheckedInNotCompleted`: không có `release_due_at`.
  - `TestRideCheckIn_Idempotent`.
  - `TestRideCheckIn_OtherPartner404`.
  - `TestRideCheckIn_RoundTripLegsIndependent`: check-in chiều về không đụng tới chiều đi.
  - `TestRideNoShow_AfterDeparturePlusSetting`: 07:59 vẫn `confirmed`, 08:01 thành `no_show` với `release_due_at = no_show_at + 1 ngày`.
  - `TestRideNoShow_SettingChangeApplies`: `ride_no_show_after_minutes = 0` thì 07:01 đã `no_show`.
  - `TestCompleteRides_AtServiceEnd`: 09:59 vẫn `checked_in`, 10:00 thành `completed` và có release đúng lịch. Sau `ReleaseDue` thì available = 450.000, `Verify` không lỗi.
  - `TestRideNoShowKeepsSeats`: `seats_reserved` không đổi sau No-show hoặc hoàn thành.
  - `TestReschedule_RideRejected409` và `TestAttractionNoShowUnchanged`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): ride check-in window, no-show and completion jobs`.

---

### Task 9: Nối dây, kiểm thử trọn luồng, tài liệu

**Files:**
- Create: `cmd/api/e2e_ride_test.go`
- Modify: `cmd/api/{main.go,routes.go,routes_test.go}`, `docs/superpowers/specs/2026-11-04-database-schema.md` (§5.5, §6), `docs/superpowers/plans/README.md`

**Interfaces:**
- Consumes: mọi task trước.
- Produces:
  - `main`:
    - Đăng ký `listing.GenerateTripsWorker` và `booking.CompleteRidesWorker`, gán Service trước khi `jobClient.Start`.
    - `periodic := append(append(booking.PeriodicJobs(), reconcile.PeriodicJobs()...), listing.PeriodicJobs()...)`.
  - `buildMux` không đổi chữ ký. Các route mới đi qua `Listing.PartnerRoutes/AdminRoutes/PublicRoutes` và `Booking.CustomerRoutes/AdminRoutes`.
  - Tài liệu DB:
    - §5.5 ✅ plan 6: `generated_until`, `cancel_source`, FK tới `ride_routes`, trigger, cửa sổ 60 ngày.
    - §6: `booking_ride_details.origin_name/dest_name`, CHECK `checked_in_at`, quy tắc hủy khứ hồi khi một chiều đã đi.
    - §1.1: "khóa chuyến trước mọi checkout có xe tuyến".
  - `plans/README.md`: đánh dấu plan 6 xong.

- [ ] **Step 1: Viết test fail** `e2e_ride_test.go`, qua mux thật với `payment.Fake`:
  1. Admin đặt hoa hồng `scheduled_ride` 10%, tạo đối tác, bật `scheduled_ride`.
  2. Đối tác tạo listing A (79 → 68) và B (68 → 79): tuyến, lịch (A 07:00 giá 250.000, B 13:00 giá 220.000, 10 ghế), ảnh, gửi duyệt, admin duyệt.
  3. Khách `GET /v1/ride-trips` cho cả hai chiều, rồi `POST /v1/bookings/ride` khứ hồi 2 ghế (có `Idempotency-Key`). Kỳ vọng 201, `total_amount 940.000`.
  4. `POST /v1/dev/payments/{order_code}/simulate-paid` một lần. Kỳ vọng cả hai booking `confirmed` và `GET /v1/partner/balance` có `pending = 846.000`.
  5. Đổi clock tới giờ đi của A. Đối tác check-in chiều đi bằng QR, kết quả `checked_in`. `CompleteRides` tại +180 phút, kết quả `completed`.
  6. Đổi clock tới giờ đi của B + 61 phút. `MarkNoShows` chuyển chiều về sang `no_show`.
  7. `ReleaseDue` sau 1 ngày. Kỳ vọng `available = 846.000`, `pending = 0`, `ledger.Verify` không lỗi, `seats_reserved` của hai chuyến vẫn là 2.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Nối dây** và cập nhật tài liệu.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...` và `go tool sqlc diff` (không có diff).
- [ ] **Step 5: Kiểm tra tay** trên **DB riêng của nhánh** (`make db-branch`, migrate từ đầu), dùng `PAYMENT_PROVIDER=fake`:
  1. Tạo hai listing tuyến ngược, lịch chạy, duyệt. Xem job sinh chuyến chạy lúc khởi động (`RunOnStart`).
  2. Tìm chuyến, đặt khứ hồi, `simulate-paid`.
  3. Check-in một chiều. Admin `POST /v1/admin/ride-trips/{id}/cancel` một chuyến của đơn khứ hồi khác.
  4. Xem số dư và refund. Ghi kết quả vào báo cáo task.
- [ ] **Step 6: Commit** `feat(ride): wire scheduled rides; e2e round-trip flow`.

---

## Không làm trong plan này

- Trạng thái `departed` của `ride_trips`: không job nào ghi nó, vì điều kiện `departure_at > now` đã chặn việc bán ghế. Để dành cho báo cáo (plan 10) nếu cần.
- Sơ đồ ghế hoặc chọn ghế (spec §2 [Chốt]), giá theo ngày hoặc mùa, nhiều điểm đón trên một tuyến.
- Báo cáo §13 cho xe tuyến (plan 10). Chuông thông báo trong web (plan 9).
- Self-Drive Rental, Private Charter (plan 7–8).
- Nhả giữ chỗ quá hạn khi đối tác sửa chuyến: đối tác có thể bị chặn sửa hoặc hủy chuyến tối đa khoảng một nhịp job (≤ 1 phút sau `hold_release_at`). Chấp nhận được.

## Các quyết định đã chốt

1. **Chiều về:** cùng nhà xe, tuyến ngược theo tỉnh (phường tự do).
2. **Sinh chuyến:** 60 ngày tới, hằng số `tripHorizonDays`; job mỗi giờ và sinh ngay khi tạo hoặc sửa lịch.
3. **Số ghế:** khách nhập số khách, hệ thống lọc chuyến còn đủ ghế và hiện loại xe. Không có trần cố định mỗi đơn. Khứ hồi hai chiều cùng số ghế.
4. **Ngày chiều về:** ngày bất kỳ, miễn `return.departure_at ≥ outbound.departure_at + duration`.
5. **Tìm kiếm:** bắt buộc tỉnh ở hai đầu, phường là bộ lọc tùy chọn; ngày là ngày VN của `departure_at`.
6. **Hạn chót đặt chỗ:** trước giờ khởi hành **60 phút**, setting `ride_booking_cutoff_minutes` (admin chỉnh được), không bao giờ ngắn hơn hold + grace.
7. **Sửa lịch lặp:** không đổi `departure_time`/`weekdays`/`valid_from`; giá, sức chứa, `valid_to`, `active` chỉ áp cho chuyến tương lai chưa có ghế được giữ.
8. **Admin hủy:** hủy đơn cho chọn `policy` hoặc `force_majeure`; hủy chuyến luôn `force_majeure`. Chỉ hủy chiều còn `confirmed`.

## Mâu thuẫn trong spec đã gặp

- **§5.6** nói Super Admin hủy (kể cả hủy chuyến của một chiều) thì "cả hai chiều đều bị hủy". **DB §6** nói đơn `checked_in`/`completed`/`no_show` không hủy được. Plan chọn: chỉ hủy những chiều còn `confirmed`. Chiều đã đi thì giữ nguyên (Review Focus 4).
- **§5.6** nói "Super Admin hủy thì hoàn 100% cả hai chiều". **§6.3** cho Super Admin chọn áp chính sách thông thường. Plan chọn: hủy chuyến luôn hoàn 100%, hủy đơn cho chọn cả hai chế độ (câu hỏi 8).
- Plan 5, mục "Không làm", ghi "cửa sổ giờ check-in (plan 9)". Spec §10 và phạm vi plan 6 đưa cửa sổ ±X của xe tuyến vào plan này. Check-in của Attraction (trong ngày tham quan) giữ nguyên.
- DB §5.5 không có `generated_until`, `cancel_source` và snapshot tên điểm trong `booking_ride_details`. Plan thêm các cột này và cập nhật DB doc ở Task 9.
- Listing lớp chung đòi tỉnh, phường và địa chỉ khi gửi duyệt. Với xe tuyến, vị trí nằm ở `ride_routes`, nên plan thay điều kiện này bằng "có tuyến" (Task 2).
