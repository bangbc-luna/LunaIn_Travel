# Plan 4 — Booking + PayOS + Sổ cái cho vé tham quan — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Khách đặt được vé tham quan và thanh toán qua PayOS. Đối tác quét QR để check-in. Khách hủy được vé (có hoàn tiền theo chính sách) hoặc đổi lịch một lần. Hệ thống tự xử lý hết hạn giữ chỗ, bỏ vé (no-show) và giải phóng tiền. Mọi đồng tiền đều được ghi vào sổ cái của đối tác và sổ hoa hồng của sàn.

**Architecture:** Thêm 3 module.

- `internal/ledger`: bút toán và số dư của đối tác, sổ hoa hồng của sàn.
- `internal/payment`: bảng payments, payment_events, refunds, cùng client PayOS và webhook.
- `internal/booking`: checkout, booking, quy trình đặt, hủy, đổi lịch, check-in và các job định kỳ.

Để tránh import vòng:

- `booking` import `ledger`, `payment`, `listing`, `catalog`, `settings`, `identity`.
- `payment` **không** import `booking`. Khi webhook báo tiền đã về, `payment` gọi interface `PaymentHandler`, và `booking` là bên cài đặt interface này.
- `listing` cần biết có booking hay chưa trước khi đóng ngày hoặc đổi khung giờ. Việc này đi qua interface `BookingGuard`, khai báo trong `listing` và do `booking` cài đặt. `main` nối hai bên lại với nhau.

Job định kỳ dùng River periodic jobs.

**Tech Stack:** như plan 1–3. PayOS gọi qua REST (`net/http`), không dùng SDK.

**Spec:**
- [spec hệ thống](../specs/2026-11-04-lunain-travel-backend-design.md) §5.9, §6.1, §6.3, §6.4, §7, §8, §9, §10
- [thiết kế DB](../specs/2026-11-04-database-schema.md) §1, §1.1, §1.2, §6, §7, §8

## Global Constraints

- Mọi Global Constraints của plan 1–3 vẫn áp dụng: uuid v7, `created_at`/`updated_at`, thời gian lấy từ clock được inject, lỗi JSON tiếng Việt, test chạy Postgres thật, mỗi module có mục sqlc riêng, path id không trỏ tới tài nguyên nào thì trả 404, body sai thì trả 422, body có trường lạ thì trả 400.
- **Tiền:**
  - Kiểu `bigint` VND, tuyệt đối không dùng float.
  - Mọi phép nhân với tỷ lệ dùng `money.MulBps(amount int64, bps int) int64`, làm tròn nửa lên. Kết quả phải giống `round()` của Postgres với số dương.
  - `commission_amount = MulBps(gross, rate_bps)`
  - `refund_amount = MulBps(gross, refund_bps)`
  - `partner_earning = gross − commission − refund`, có thể âm.
- **Thứ tự khóa (DB §1.1):**
  ```
  users → partners → listings → (dòng sức chứa) → checkouts → bookings (id tăng dần)
        → dòng con của booking → payments → partner_balances (partner_id tăng dần)
  ```
  - Không bao giờ nâng khóa `FOR SHARE` của listing lên mức cao hơn trong transaction booking.
  - Nếu chỉ biết id: đọc id mà không khóa, sau đó khóa theo đúng thứ tự trên rồi kiểm tra lại trạng thái.
- **Gọi PayOS (HTTP ra ngoài) không bao giờ diễn ra trong một DB transaction đang mở.**
- **Sổ cái:**
  - `ledger_entries` chỉ được insert. Có trigger chặn UPDATE và DELETE.
  - Mỗi bút toán có `seq` liên tục theo đối tác, cấp khi đang giữ khóa `partner_balances`, kèm `available_after`.
  - Bút toán bằng 0 thì không ghi.
- **Lời gọi plan 3 (bắt buộc, trong cùng transaction tạo booking, theo đúng thứ tự):**
  1. `listing.BookableListing`
  2. `SlotsForDate` (kiểm tra khung giờ thuộc ngày đó và đang active)
  3. `TimeSlot.StartAt`/`EndAt` để tính `service_start_at`/`service_end_at` theo giờ VN
  4. `TicketTypesForBooking`

  Ngoài ra:
  - Ngày tham quan trong quá khứ, hoặc khung giờ đã bắt đầu, thì trả 422.
  - Chụp lại tên vé và đối tượng vé (`audience`) vào `booking_items.description`.
  - `cancellation_policy_snapshot = json(CancellationPolicy())`.
  - Loại dịch vụ chưa có % hoa hồng thì trả 422 `no_commission_rate`.
- **Quy trình làm việc (mới, bắt buộc từ plan này):**
  - Mỗi worktree dùng **database riêng** `lunain_<tên nhánh>` trong container dev, tạo bằng `make db-branch`. DB `lunain` chỉ dùng cho `main`.
  - Migration đã có trên `main` là **bất biến**. Test `TestMigrationChecksums` sẽ fail nếu có file migration cũ bị sửa.
- Mỗi route mới dưới `/v1/admin/*` hoặc `/v1/partner/*` phải được thêm vào `adminRoutes`/`partnerRoutes` trong `cmd/api/routes_test.go`.

## Review Focus

1. **Webhook gửi lặp hoặc gửi song song cho cùng một thanh toán:** booking chỉ được xác nhận **một lần** và sổ cái chỉ ghi **một lần**. Test ở Task 6.
2. **Tiền về sau khi giữ chỗ đã hết hạn:** nếu còn đặt được thì xác nhận lại. Nếu Listing đã bị ẩn, đối tác bị đình chỉ hoặc khung giờ đã qua, thì tạo hoàn 100% và **không ghi bút toán nào cho đối tác**. Test ở Task 6.
3. **Hủy vé sát giờ:** % hoàn tính theo `original_service_start_at`, kể cả khi đã đổi lịch. Hoa hồng không giảm, đối tác có thể bị âm. Super Admin hủy vì sự cố thì hoàn 100% và miễn hoa hồng. Sau khi hủy, `pending` của booking về đúng 0. Test ở Task 8.
4. **Khách A thao tác lên booking của khách B** (xem, hủy, đổi lịch, gửi tài khoản hoàn tiền) phải nhận 404. **Đối tác A quét QR của đối tác B** cũng nhận 404. Test ở Task 5, Task 7 và Task 8.
5. **Bấm "đặt" hai lần với cùng `Idempotency-Key`** chỉ tạo một booking. Dùng lại key đó với body khác thì nhận 422. Test ở Task 5.

---

## Cấu trúc file

```
internal/platform/money/money.go                 # MulBps, round half up
internal/platform/db/migrations/CHECKSUMS        # sha256 từng migration
internal/platform/db/migrations_test.go          # TestMigrationChecksums
internal/platform/db/migrations/00012_booking.sql
internal/platform/db/migrations/00013_payment.sql
internal/platform/db/migrations/00014_ledger.sql
internal/ledger/{queries.sql,ledger.go,http.go,store/,*_test.go}
internal/payment/{queries.sql,payos.go,fake.go,service.go,webhook.go,refund.go,http.go,store/,*_test.go}
internal/booking/{queries.sql,create.go,confirm.go,checkin.go,cancel.go,reschedule.go,jobs.go,views.go,guard.go,http_*.go,store/,*_test.go}
```

---

### Task 1: Quy trình DB theo nhánh, migration bất biến, `money.MulBps`

**Files:**
- Create: `internal/platform/money/money.go` và test
- Create: `internal/platform/db/migrations/CHECKSUMS`, `internal/platform/db/migrations_test.go`
- Modify: `Makefile`, `README.md` (mục "Môi trường dev")

**Interfaces:**
- Produces:
  - `func money.MulBps(amount int64, bps int) int64`
    - `amount ≥ 0` và `0 ≤ bps ≤ 10000`, sai thì panic (lỗi lập trình).
    - Kết quả là `(amount*bps + 5000) / 10000`. Không tràn số với `amount ≤ 10^13`. Nếu vượt ngưỡng đó thì panic.
  - `CHECKSUMS`: mỗi dòng có dạng `<sha256>  <tên file>` cho mọi file `*.sql`, sắp theo tên.
  - `TestMigrationChecksums`: fail nếu
    - một file đang liệt kê bị đổi nội dung (message: "migration đã merge là bất biến — hãy tạo migration mới"), hoặc
    - có file `.sql` chưa được liệt kê (message hướng dẫn chạy `make migrations-sum`).
  - `make migrations-sum`: ghi lại `CHECKSUMS` (chỉ dùng khi **thêm** migration mới).
  - `make db-branch`: tạo database `lunain_<tên nhánh hiện tại, ký tự không phải chữ hoặc số đổi thành _>` trong container compose `lunain` (`docker exec lunain-postgres-1 createdb -U lunain ...`, đã có thì bỏ qua), rồi in ra `DATABASE_URL` tương ứng.
  - `make db-branch-drop`: xóa database đó. Chỉ chạy được khi tên DB khác `lunain`.

- [ ] **Step 1: Viết test fail.**
  - `money_test.go`: `MulBps(1_000_000, 1000) == 100_000`, `MulBps(12_345, 1000) == 1_235` (1.234,5 làm tròn lên), `MulBps(5, 1000) == 1` (0,5 thành 1), `MulBps(4, 1000) == 0`, `MulBps(0, 10000) == 0`, `MulBps(999, 10000) == 999`. Panic khi bps bằng −1 hoặc 10001.
  - `TestMigrationChecksums` (fail vì chưa có file `CHECKSUMS`).
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt** `money`, sinh `CHECKSUMS` cho các migration `00001`–`00011` đang có, viết test và các target Makefile, cập nhật README.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Chạy thử `make db-branch` trên container dev, kỳ vọng in ra một URL dùng được. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `chore: per-branch dev databases, immutable migration checksums, money.MulBps`.

---

### Task 2: Schema booking, payment, ledger

**Files:**
- Create: `00012_booking.sql`, `00013_payment.sql`, `00014_ledger.sql` (cập nhật `CHECKSUMS` bằng `make migrations-sum`)
- Create: `internal/{booking,payment,ledger}/queries.sql` (mỗi module tối thiểu một query để sqlc sinh code), các mục trong `sqlc.yaml`, `internal/booking/schema_test.go`

**Schema** (đúng DB §6–§8, những chỗ sau là bắt buộc):

- **`00012_booking.sql`:**
  - `checkouts`: `id`, `code text UNIQUE` (ví dụ `CO` + 8 ký tự Crockford base32), `customer_id` FK users, `total_amount bigint CHECK ≥ 0`, `status CHECK IN ('open','paid','closed')`.
  - `bookings`: các cột ở DB §6.
    - `code text UNIQUE` (`LT` + 8 ký tự Crockford base32).
    - `status CHECK` theo danh sách ở DB §6, kèm CHECK ràng buộc với `booking_mode`.
    - FK kép `(listing_id, partner_id) → listings(id, partner_id)`, `UNIQUE (id, partner_id)`.
    - `CHECK (commission_amount BETWEEN 0 AND gross_amount)`, `CHECK (service_end_at >= service_start_at)`.
    - `cancel_kind CHECK IN ('customer','customer_withdraw','admin_force_majeure','admin_policy')`.
    - `qr_token text UNIQUE NOT NULL`.
    - Đủ các partial index ở DB §6.
  - `booking_items`: `quantity numeric(10,1) CHECK > 0`, `CHECK (line_total = round(unit_price * quantity))`.
  - `booking_ticket_lines`: `booking_item_id` PK, `listing_id`, `ticket_type_id`, FK kép `(ticket_type_id, listing_id) → attraction_ticket_types(id, listing_id)`.
  - `booking_attraction_details`: `booking_id` PK, `listing_id`, `visit_date date`, `time_slot_id`, `slot_start time`, `slot_end time`, FK kép `(time_slot_id, listing_id) → attraction_time_slots(id, listing_id)`, FK kép `(booking_id, listing_id) → bookings(id, listing_id)`. Muốn có FK này thì `bookings` cần thêm `UNIQUE(id, listing_id)`.
  - `booking_status_history`, `booking_reschedules` (`booking_id UNIQUE`), `idempotency_keys` (PK `(user_id, key)`).
- **`00013_payment.sql`:**
  - `payments`: các cột ở DB §7.
    - `provider_order_code bigint UNIQUE NOT NULL`, lấy từ sequence `payment_order_code_seq` bắt đầu ở 100000.
    - Unique `(checkout_id) WHERE status = 'pending'`.
  - `payment_events`: `dedupe_key text UNIQUE`.
  - `refunds`: đúng DB §7 sau khi đã sửa.
    - `payment_id NOT NULL`, `booking_id NULL`.
    - Unique `(booking_id) WHERE booking_id IS NOT NULL`, unique `(payment_id) WHERE booking_id IS NULL`.
    - `CHECK (status <> 'completed' OR transfer_reference IS NOT NULL)`.
    - `CHECK (status = 'awaiting_bank_info' OR (bank_bin IS NOT NULL AND account_number IS NOT NULL AND account_holder IS NOT NULL))`.
  - `customer_refund_accounts`.
- **`00014_ledger.sql`:**
  - `partner_balances (partner_id PK, pending bigint, available bigint, last_seq bigint DEFAULT 0)`.
  - `ledger_entries`: đúng DB §8.
    - `seq`, `available_after`, `UNIQUE(partner_id, seq)`.
    - CHECK theo `kind`, gồm `booking_id`/`payout_id` và yêu cầu `reason`/`created_by` cho `adjustment`.
    - Unique `(booking_id, kind, bucket) WHERE kind IN (...)` và unique `(payout_id, kind) WHERE ...`.
    - FK kép `(booking_id, partner_id) → bookings(id, partner_id)`.
    - Trigger `ledger_entries_immutable` chặn `BEFORE UPDATE OR DELETE` (`RAISE EXCEPTION`).
    - `payout_id uuid NULL` tạm **chưa** có FK, vì bảng `payouts` thuộc plan 5. Ghi chú TODO trong migration.
  - `booking_settlements`: `booking_id` PK, `partner_id`, `net_pending bigint`, `release_due_at NULL`, `released_at NULL`, FK kép tới bookings, index `(release_due_at) WHERE released_at IS NULL`.
  - `platform_commission_entries`: `UNIQUE(booking_id, kind)`.

- [ ] **Step 1: Viết test fail** `schema_test.go`, chèn trực tiếp bằng SQL:
  - `TestLedgerEntriesImmutable`: UPDATE hoặc DELETE trên `ledger_entries` đều lỗi.
  - `TestLedgerUniqueBookingKind`: hai `booking_earning` cho cùng một booking thì lỗi 23505.
  - `TestBookingPartnerMustMatchListing`: `partner_id` khác chủ của Listing thì lỗi 23503.
  - `TestBookingItemsLineTotalCheck`.
  - `TestRefundsOnePerBooking`.
  - `TestOnePendingPaymentPerCheckout`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Viết migration, chạy `make migrations-sum` và `make sqlc`.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(db): booking, payment and ledger schema`.

---

### Task 3: Module ledger

**Files:**
- Create: `internal/ledger/{ledger.go,http.go,ledger_test.go}`, mở rộng `queries.sql`
- Modify: `cmd/api/routes.go`, `cmd/api/routes_test.go`

**Interfaces** (mọi hàm nhận `tx` của người gọi, khóa `partner_balances` bằng `FOR UPDATE` sau khi `INSERT ... ON CONFLICT DO NOTHING`, cấp `seq`, cập nhật cache và `available_after`):
- `func (s *Service) PostBookingConfirmed(ctx, tx pgx.Tx, b BookingMoney) error`
  - `BookingMoney{BookingID, PartnerID uuid.UUID; ServiceType string; Gross, Commission int64}`.
  - Ghi `booking_earning` = +(Gross − Commission) vào `pending`.
  - Ghi `platform_commission_entries` (`commission`, +Commission).
  - Tạo `booking_settlements` với `net_pending = Gross − Commission`.
- `func (s *Service) PostRefund(ctx, tx, bookingID, partnerID uuid.UUID, refund int64) error`: ghi `refund_debit` = −refund vào `pending` và trừ vào `net_pending`.
- `func (s *Service) PostCommissionWaiver(ctx, tx, bookingID, partnerID uuid.UUID, serviceType string, commission int64) error`: ghi `commission_waiver` = +commission vào `pending` và ghi `platform_commission_entries` (`waiver`, −commission).
- `func (s *Service) ScheduleRelease(ctx, tx, bookingID uuid.UUID, dueAt time.Time) error`: đặt `release_due_at` nếu chưa có.
- `func (s *Service) Release(ctx, tx, bookingID uuid.UUID) error`
  - Nếu đã release rồi thì không làm gì.
  - Nếu chưa: ghi `release` hai dòng (−net ở `pending`, +net ở `available`), trừ khi net = 0 thì không ghi dòng nào. Đặt `released_at`, `net_pending = 0`.
- `func (s *Service) Balance(ctx context.Context, partnerID uuid.UUID) (Balance, error)` với `Balance{Pending, Available int64}`. Chưa có dòng nào thì trả 0 và 0.
- `func (s *Service) Verify(ctx context.Context, partnerID uuid.UUID) error`: so cache với tổng bút toán theo từng ngăn, lệch thì lỗi. Dùng trong test và cho job đối soát sau này.
- Route đối tác: `GET /v1/partner/balance` trả `{pending, available}`.

- [ ] **Step 1: Viết test fail** `ledger_test.go`:
  - `TestConfirmThenRelease`: gross 1.000.000, hoa hồng 100.000. Sau khi xác nhận: pending 900.000, available 0. Sau khi release: pending 0, available 900.000. `seq` là 1, 2, 3. `available_after` đúng. `Verify` không lỗi.
  - `TestCancelHalfRefund`: +900.000, −500.000, release, kết quả available 400.000. Khớp ví dụ trong spec §6.3.
  - `TestFullRefundGoesNegative`: available −100.000.
  - `TestForceMajeureWaiver`: +900.000, −1.000.000, +100.000, release 0 (không ghi dòng release nào), available 0, sổ sàn có +100.000 và −100.000.
  - `TestReleaseIdempotent`.
  - `TestConcurrentPostsSerializeSeq`: 20 goroutine ghi cho cùng một đối tác, kỳ vọng `seq` chạy liền 1..N không trùng và `Verify` không lỗi.
  - HTTP `TestHTTP_PartnerBalance`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(ledger): partner ledger with sequenced entries and settlements`.

---

### Task 4: Client PayOS, provider giả lập, cấu hình

**Files:**
- Create: `internal/payment/{payos.go,fake.go,payos_test.go}`
- Modify: `internal/platform/config/config.go`, `.env.example`

**Implementer phải đọc tài liệu PayOS chính thức** (`https://payos.vn/docs/api/` và `https://payos.vn/docs/du-lieu-tra-ve/webhook/`) để xác nhận tên trường và cách ký. Plan ghi những gì đã biết. Nếu tài liệu khác plan thì làm theo tài liệu và ghi lại sự khác biệt trong báo cáo.

- Tạo link: `POST {base}/v2/payment-requests`, header `x-client-id`, `x-api-key`.
  - Body: `orderCode`, `amount`, `description` (tối đa 25 ký tự), `cancelUrl`, `returnUrl`, `expiredAt` (unix giây), `signature`.
  - `signature` = HMAC-SHA256 bằng checksum key, ký lên chuỗi `amount=..&cancelUrl=..&description=..&orderCode=..&returnUrl=..` (sắp theo key).
  - Response `data` có `checkoutUrl`, `qrCode`, `paymentLinkId`.
- Tra cứu: `GET {base}/v2/payment-requests/{orderCode}` trả `status` (`PAID`/`PENDING`/`CANCELLED`/`EXPIRED`...) và `amountPaid`.
- Hủy link: `POST {base}/v2/payment-requests/{orderCode}/cancel`.
- Webhook: body `{code, desc, success, data:{orderCode, amount, description, reference, transactionDateTime, paymentLinkId, ...}, signature}`. `signature` = HMAC-SHA256 lên các trường của `data` sắp theo key, nối thành `k=v&...`.

**Interfaces:**
- `payment.Provider interface { CreateLink(ctx, LinkRequest) (Link, error); GetStatus(ctx, orderCode int64) (Status, error); CancelLink(ctx, orderCode int64) error; VerifyWebhook(body []byte) (WebhookData, error) }`
  - `LinkRequest{OrderCode int64; Amount int64; Description string; ExpiresAt time.Time}` (return/cancel URL lấy từ config).
  - `Link{CheckoutURL, QRCode, ProviderLinkID string}`, `Status{State string; AmountPaid int64}`.
  - `WebhookData{OrderCode int64; Amount int64; Reference string; TransactionAt time.Time; Raw json.RawMessage}`.
- `payment.NewPayOS(cfg PayOSConfig, hc *http.Client) *PayOS`: mọi request có timeout 10 giây. Lỗi HTTP hoặc `code ≠ "00"` thì trả lỗi đã bọc lại. Sai chữ ký webhook thì `ErrInvalidSignature`.
- `payment.Fake`: provider trong bộ nhớ dùng cho test và dev.
  - `CreateLink` trả `http://localhost/fake-pay/{orderCode}`.
  - `MarkPaid(orderCode, amount)` đặt trạng thái và sinh body webhook đã ký bằng một checksum key cố định.
  - `VerifyWebhook` kiểm tra chữ ký thật, theo đúng cách của PayOS.
- **Config:** `PAYOS_CLIENT_ID`, `PAYOS_API_KEY`, `PAYOS_CHECKSUM_KEY`, `PAYOS_BASE_URL` (mặc định `https://api-merchant.payos.vn`), `PAYOS_RETURN_URL`, `PAYOS_CANCEL_URL`, `PAYMENT_PROVIDER` (`payos` | `fake`).
  - `APP_ENV=prod` thì bắt buộc dùng `payos` và có đủ khóa.
  - `APP_ENV=dev` mà không có khóa thì mặc định là `fake`.

- [ ] **Step 1: Viết test fail** `payos_test.go`, dùng `httptest.Server` đóng vai PayOS:
  - `TestCreateLink_SignsAndSendsHeaders`: server kiểm tra header, chữ ký (tính lại bằng key đã biết) và `description` không quá 25 ký tự.
  - `TestCreateLink_ErrorCode`: `code: "01"` thì trả lỗi.
  - `TestGetStatus`, `TestCancelLink`.
  - `TestVerifyWebhook_ValidAndTampered`: dùng vector mẫu. Ưu tiên ví dụ trong tài liệu PayOS nếu có, nếu không thì tự tạo.
  - `TestFake_MarkPaidProducesVerifiableWebhook`.
  - `TestConfig_PaymentProvider`: prod mà thiếu khóa thì lỗi, dev mà không có khóa thì ra `fake`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(payment): payos client, fake provider and config`.

---

### Task 5: Tạo booking vé tham quan, link thanh toán, các màn xem

**Files:**
- Create: `internal/booking/{create.go,views.go,http_customer.go,http_partner.go,http_admin.go,helpers_test.go,create_test.go,views_test.go}`
- Create: `internal/payment/service.go` (`StartPayment`)
- Modify: `cmd/api/routes.go`, `cmd/api/routes_test.go`

**Interfaces:**
- `booking.CreateAttractionInput{ListingID uuid.UUID; VisitDate string (YYYY-MM-DD); TimeSlotID uuid.UUID; Items []TicketLine{TicketTypeID uuid.UUID; Quantity int}; ContactName, ContactPhone, ContactEmail string; Note *string}`
  - Có 1 đến 10 dòng vé. Mỗi dòng có quantity từ 1 đến 50, tổng số vé tối đa 100.
  - Liên hệ: tên 1..100 rune, số điện thoại theo `validate.NormalizePhone`, email theo `identity.NormalizeEmail`.
- `func (s *Service) CreateAttraction(ctx, customerID uuid.UUID, idemKey string, in CreateAttractionInput) (BookingView, error)`:
  1. **Idempotency:** `idempotency_keys` theo `(customer, key)` cùng hash body. Trùng key mà cùng body thì trả lại kết quả cũ. Trùng key mà khác body thì 422 `idempotency_key_reused`. Thiếu header thì 400.
  2. **Transaction:**
     - `BookableListing`
     - kiểm tra service type là `attraction_ticket`
     - `SlotsForDate` phải chứa `TimeSlotID`
     - `StartAt` phải lớn hơn now, nếu không thì 422 `slot_started`
     - `TicketTypesForBooking`
     - `CurrentCommissionRate`
     - `settings.GetTx`
     - insert `checkout`, `booking` (`pending_payment`, `hold_expires_at = now + hold_minutes`, `hold_release_at = hold_expires_at + hold_grace_minutes`, `service_start_at`/`end_at`, `original_service_start_at = service_start_at`), `booking_items` (description là `"{tên vé} ({Trẻ em|Người lớn})"`), `ticket_lines`, `attraction_details`, `status_history`
     - `identity.FillProfileIfEmpty`
     - `payment.ReservePayment(tx, checkoutID, amount, expiresAt)`: tạo dòng `payments` ở trạng thái `pending` và cấp `orderCode`
  3. **Sau commit:** `payment.ActivateLink(ctx, paymentID)` gọi `Provider.CreateLink` rồi lưu `checkout_url`/`qr_code`. Gọi lỗi thì vẫn trả 201, nhưng `payment.checkout_url = null`. Khách gọi `POST /v1/bookings/{id}/pay` để thử lại.
- `BookingView`: gồm mã, trạng thái, Listing (id, tên, slug), ngày và khung giờ, các dòng, `gross_amount`, `hold_expires_at`, `payment{checkout_url, qr_code, expires_at}` (chỉ khi `pending_payment`), `qr_token` (chỉ khi `confirmed`/`completed`, và chỉ trả cho chính khách), chính sách hủy, và `can_cancel`/`can_reschedule` (tính theo quy tắc ở Task 8).
- Các màn xem:
  - Khách: `ListMine(customerID, filter)`, `GetMine(customerID, id)` (không phải của mình thì 404).
  - Đối tác: `ListForPartner(partnerID, filter)`, `GetForPartner` (không có `qr_token`).
  - Admin: `AdminList(filter)`, `AdminGet(id)` (có lịch sử trạng thái).
- Routes:
  - Khách (`RequireRole(customer)`): `POST /v1/bookings/attraction`, `GET /v1/bookings`, `GET /v1/bookings/{id}`, `POST /v1/bookings/{id}/pay`.
  - Đối tác: `GET /v1/partner/bookings`, `GET /v1/partner/bookings/{id}`.
  - Admin: `GET /v1/admin/bookings`, `GET /v1/admin/bookings/{id}`.

- [ ] **Step 1: Viết helper test.**
  - Dựng toàn bộ service thật với `payment.Fake` và clock cố định `2026-11-05T09:00:00+07:00`.
  - Tạo sẵn một Listing attraction đã duyệt: 2 loại vé (người lớn 150.000, trẻ em 80.000), khung 08:00–17:00 mọi ngày, chính sách `[{48,10000},{24,5000}]`, hoa hồng 1000 bps.
  - Tạo khách đã xác minh.
- [ ] **Step 2: Viết test fail** `create_test.go`:
  - `TestCreate_PendingWithMoneySnapshot`: 2 vé người lớn và 1 vé trẻ em, kỳ vọng `gross = 380.000`, `commission = 38.000`, hold 15 và 20 phút, `service_start_at = 2026-11-07 08:00+07`, snapshot chính sách đúng, có dòng payment `pending` kèm `checkout_url`.
  - `TestCreate_Idempotent`: cùng key thì chỉ có 1 booking. Cùng key khác body thì 422.
  - `TestCreate_NoCommissionRate422`.
  - `TestCreate_PastDateOrStartedSlot422`.
  - `TestCreate_ClosedDate422`.
  - `TestCreate_InactiveTicket422`.
  - `TestCreate_NotBookable422`: đối tác bị đình chỉ.
  - `TestCreate_ProviderDownStillCreatesBooking`: fake trả lỗi, sau đó gọi `/pay` thì thành công.
  - `TestCreate_FillsEmptyProfile`.
- [ ] **Step 3: Viết test fail** `views_test.go`:
  - `TestGetMine_OtherCustomer404` (Review Focus 4).
  - `TestPartnerView_NoQRToken`.
  - `TestAdminGet_History`.
- [ ] **Step 4: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 5: Cài đặt.** Cập nhật danh sách route trong `routes_test.go`.
- [ ] **Step 6: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 7: Commit** `feat(booking): attraction booking creation, payment link and views`.

---

### Task 6: Webhook và xác nhận thanh toán (cả tiền về trễ, thiếu, thừa, trùng)

**Files:**
- Create: `internal/payment/webhook.go`, `internal/booking/confirm.go`, cùng test
- Modify: `cmd/api/routes.go`

**Interfaces:**
- `payment.PaymentHandler interface { HandlePaid(ctx context.Context, p PaidEvent) error }`, với `PaidEvent{PaymentID, CheckoutID uuid.UUID; OrderCode int64; AmountPaid int64; Reference string; PaidAt time.Time}`. `booking.Service` là bên cài đặt.
- Webhook `POST /v1/payments/payos/webhook` (public, rate limit 600 request/phút theo IP):
  1. `VerifyWebhook`. Sai chữ ký thì 400, không lưu gì.
  2. Insert `payment_events` với `dedupe_key = "payos:" + orderCode + ":" + reference`. Trùng thì trả 200 ngay.
  3. Tìm payment theo `orderCode`. Không có thì trả 200 và log, vì PayOS có gửi webhook thử khi đăng ký URL.
  4. Gọi `Provider.GetStatus(orderCode)` để xác nhận lại, chạy **ngoài** transaction. Nếu không phải `PAID` thì ghi `result = "not_paid"` và trả 200.
  5. Gọi `handler.HandlePaid(...)`.
  6. Lỗi tạm thời thì trả 500, để PayOS gửi lại (`dedupe_key` đã được rollback cùng transaction).

- **`booking.HandlePaid`:**
  - Đọc `checkout` và các booking mà không khóa.
  - Nếu có booking `expired`: gọi `BookableListing` cho từng Listing (theo id tăng dần) **trước** khi khóa checkout.
  - Sau đó: khóa `checkouts FOR UPDATE` → bookings (id tăng dần) → `payments FOR UPDATE` → so trạng thái.

  | Trường hợp | Xử lý |
  |---|---|
  | `payments.status` đã là `paid` mà có `reference` khác | **Trả trùng**: tạo refund `duplicate_payment` bằng số tiền vừa trả, gắn với payment, `booking_id` NULL |
  | `AmountPaid < total` | **Chuyển thiếu**: `payments.status = needs_review`, refund toàn bộ `payment_mismatch` (gắn payment), booking **không** được xác nhận |
  | Booking đang `pending_payment` | Xác nhận: `confirmed`, `confirmed_at`, `checkout.status = paid`, `payments.status = paid`, ghi `ledger.PostBookingConfirmed` cho từng booking |
  | Booking `expired` và thỏa điều kiện xác nhận lại | Đi từ `expired` sang `confirmed`, history ghi lý do "thanh toán trễ, xác nhận lại", rồi ghi ledger như trường hợp trên. **Điều kiện:** `BookableListing` ok, khung giờ vẫn có trong `SlotsForDate(visit_date)`, và `service_start_at > now` |
  | Booking `expired` và **không** thỏa điều kiện | Giữ `expired`, `payments.status = paid`, refund 100% `late_payment` cho từng booking (gắn `booking_id`), **không ghi bút toán nào** |
  | `AmountPaid > total` | **Chuyển thừa**: xác nhận như bình thường, kèm refund phần thừa `payment_mismatch` (gắn payment, `booking_id` NULL), `needs_review` |

  - Mọi refund tạo ra đều ở trạng thái `awaiting_bank_info`. Nếu khách đã có `customer_refund_accounts` thì điền sẵn và chuyển sang `pending`.
  - Ghi outbox gửi email cho khách: đã xác nhận (kèm mã đặt chỗ) hoặc sẽ được hoàn tiền.

- **Chế độ dev:** khi `PAYMENT_PROVIDER = fake` thì đăng ký thêm `POST /v1/dev/payments/{order_code}/simulate-paid` với body `{amount?}`. Route này sinh webhook đã ký rồi gọi đúng luồng webhook ở trên. **Tuyệt đối không đăng ký khi `APP_ENV = prod`**, và có test kiểm chứng điều này.

- [ ] **Step 1: Viết test fail** `confirm_test.go`:
  - `TestPaid_Confirms`: kỳ vọng `confirmed`, pending của đối tác = 342.000 (380.000 − 38.000), sổ sàn có +38.000.
  - `TestWebhook_DuplicateDelivery`: gửi cùng một body 2 lần, chỉ có 1 bút toán (Review Focus 1).
  - `TestWebhook_ConcurrentDelivery`: 2 goroutine cùng gửi, kết quả 1 xác nhận và 1 bút toán.
  - `TestWebhook_BadSignature400`.
  - `TestWebhook_UnknownOrder200`.
  - `TestWebhook_ProviderSaysNotPaid`.
  - `TestPaid_Underpaid`: không xác nhận, có refund toàn bộ, `needs_review`.
  - `TestPaid_Overpaid`: xác nhận, có refund phần thừa không gắn booking.
  - `TestPaid_DuplicatePaymentRefunded`.
  - `TestLatePaid_ReconfirmsWhenStillBookable` (Review Focus 2).
  - `TestLatePaid_RefundsWhenListingHidden`: không có dòng nào trong `ledger_entries` (Review Focus 2).
  - `TestLatePaid_RefundsWhenSlotPassed`.
  - `TestRefundPrefilledFromSavedAccount`.
  - `TestDevSimulateRoute_OnlyWhenFake` và `TestDevSimulateRoute_NeverInProd`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): payos webhook, confirmation and late/mismatched payments`.

---

### Task 7: Job định kỳ và check-in QR

**Files:**
- Create: `internal/booking/{jobs.go,checkin.go,jobs_test.go,checkin_test.go}`
- Modify: `cmd/api/main.go` (đăng ký periodic jobs với River), `cmd/api/routes_test.go`

**Interfaces:**
- River periodic job, chạy mỗi 1 phút (mỗi lần tối đa 200 booking, xử lý từng booking trong transaction riêng, booking nào lỗi thì log rồi chạy tiếp):
  - `ExpireHoldsWorker`:
    - Chọn booking `pending_payment` có `hold_release_at ≤ now`.
    - Khóa checkout → booking, kiểm tra lại trạng thái, chuyển `expired` và ghi history.
    - Payment `pending` chuyển `expired`, checkout chuyển `closed`.
    - Sau commit, gọi `Provider.CancelLink` (best effort).
  - `NoShowWorker`: booking attraction `confirmed` có `service_end_at < now` → `no_show`, `no_show_at`, `ledger.ScheduleRelease(no_show_at + release_delay_days)`.
  - `ReleaseWorker`: các `booking_settlements` có `release_due_at ≤ now` và `released_at IS NULL` → khóa booking → `ledger.Release`.
- Check-in:
  - `func (s *Service) CheckIn(ctx, partnerID uuid.UUID, qrToken string) (CheckInResult, error)`
    - Khóa booking theo `qr_token` **và** `partner_id`. Không có thì 404.
    - Đã check-in rồi thì 200 `{already: true, checked_in_at}`.
    - Đang `confirmed` mà `visit_date` (VN) khác hôm nay thì 422 `checkin_wrong_date`.
    - Hợp lệ thì: `checked_in_at` cộng `completed`, `completed_at`, `ScheduleRelease(completed_at + release_delay_days)`.
  - `CheckInByCode(partnerID, code)` cho trường hợp nhập tay mã booking.
  - Routes: `POST /v1/partner/check-ins` với body `{qr_token}` hoặc `{code}` (đúng một trong hai, gửi cả hai hoặc không gửi gì thì 422).

- [ ] **Step 1: Viết test fail.**
  - `jobs_test.go`, gọi trực tiếp hàm xử lý của worker với clock cố định:
    - `TestExpireHolds_AfterGraceOnly`: tại hold + 4 phút vẫn `pending_payment`, tại hold + 6 phút thì `expired`.
    - `TestExpireHolds_SkipsAlreadyConfirmed`.
    - `TestNoShow_ThenReleaseAfterDelay`.
    - `TestRelease_MovesToAvailable`: `Verify` không lỗi.
  - `checkin_test.go`:
    - `TestCheckIn_CompletesAndSchedulesRelease`.
    - `TestCheckIn_Idempotent`.
    - `TestCheckIn_OtherPartner404` (Review Focus 4).
    - `TestCheckIn_WrongDate422`.
    - `TestCheckIn_ByCode`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): hold expiry, no-show, release jobs and qr check-in`.

---

### Task 8: Hủy, hoàn tiền và đổi lịch

**Files:**
- Create: `internal/booking/{cancel.go,reschedule.go,cancel_test.go,reschedule_test.go}`, `internal/payment/refund.go` (cùng test)
- Modify: các file HTTP, `cmd/api/routes_test.go`

**Interfaces:**
- `func (s *Service) CancelByCustomer(ctx, customerID, bookingID uuid.UUID, bank *BankAccount) (BookingView, error)`:
  - Booking không phải của khách thì 404. Không ở trạng thái `confirmed` thì 409 `invalid_booking_state`.
  - `bps = RefundBps(snapshot, original_service_start_at − now)`, `refund = MulBps(gross, bps)`.
  - Trong một transaction:
    - booking chuyển `cancelled` (`cancel_kind = customer`)
    - history
    - nếu `refund > 0`: `ledger.PostRefund` và `refunds.Create(booking, refund)`. Có `bank` thì refund ở `pending` (và lưu làm tài khoản mặc định), không có thì ở `awaiting_bank_info`.
    - `ledger.Release` ngay lập tức
  - Ghi outbox email.
- `func (s *Service) CancelByAdmin(ctx, actor, bookingID uuid.UUID, mode string, reason string) (BookingView, error)`:
  - `mode` ∈ {`force_majeure`, `policy`}. `reason` 1..500 rune.
  - Chỉ hủy được khi booking `confirmed`.
  - `force_majeure`: refund 100% kèm `PostCommissionWaiver`.
  - `policy`: tính giống khách hủy.
  - Release ngay, `audit.Record("booking.cancel")`.
- `func (s *Service) Reschedule(ctx, customerID, bookingID uuid.UUID, visitDate string, slotID uuid.UUID) (BookingView, error)` (spec §6.4):
  - Booking phải `confirmed` và chưa có dòng `booking_reschedules`.
  - `now ≤ confirmed_at + reschedule_window_days`.
  - `original_service_start_at > now`, tức ngày tham quan cũ chưa tới.
  - Khung giờ mới phải có trong `SlotsForDate(newDate)` và `StartAt > now`. **Không** cần gọi `BookableListing`, vì đơn đã có vẫn được phục vụ.
  - Cập nhật `attraction_details` và `service_start_at`/`end_at` (giữ nguyên `original_service_start_at`), thêm dòng `booking_reschedules`, ghi history.
  - Sai điều kiện thì 409 `reschedule_not_allowed`, kèm lý do trong message.
- `payment.SetRefundBankAccount(ctx, customerID, refundID uuid.UUID, bank BankAccount) error`:
  - Chỉ áp dụng cho refund của chính khách (qua booking hoặc payment → checkout → customer), nếu không thì 404.
  - Refund phải đang `awaiting_bank_info`, chuyển sang `pending`.
  - Kiểm tra theo quy tắc của tài khoản ngân hàng đối tác: BIN 6 số, số tài khoản 6–19 số, tên chủ tài khoản viết HOA không dấu. Hàm chuẩn hóa chuyển sang `internal/platform/validate`.
- Routes:
  - Khách: `POST /v1/bookings/{id}/cancel` với body `{bank_account?}`, `POST /v1/bookings/{id}/reschedule` với body `{visit_date, time_slot_id}`, `GET /v1/refunds` (refund của tôi), `PUT /v1/refunds/{id}/bank-account`, `GET /v1/customer/refund-account`, `PUT /v1/customer/refund-account`.
  - Admin: `POST /v1/admin/bookings/{id}/cancel` với body `{mode, reason}`.
- `can_cancel`/`can_reschedule` trong `BookingView` dùng **đúng** các hàm điều kiện ở trên.

- [ ] **Step 1: Viết test fail.**
  - `cancel_test.go`, với booking gross 1.000.000 dựng trực tiếp:
    - `TestCancel_Over48hFullRefund`: refund 1.000.000, đối tác −100.000 available, `pending` của booking = 0.
    - `TestCancel_Between24and48HalfRefund`: refund 500.000, đối tác +400.000.
    - `TestCancel_Under24hNoRefund`: không có refund, đối tác +900.000.
    - `TestCancel_AfterRescheduleUsesOriginalStart` (Review Focus 3).
    - `TestCancelByAdmin_ForceMajeureWaivesCommission`: đối tác 0, sàn net 0.
    - `TestCancelByAdmin_PolicyMode`.
    - `TestCancel_OtherCustomer404`.
    - `TestCancel_NotConfirmed409`.
    - `TestCancel_WithBankAccountPendingAndSaved`.
    - `TestSetRefundBankAccount_OtherCustomer404`.
  - `reschedule_test.go`:
    - `TestReschedule_OnceOnly`.
    - `TestReschedule_AfterWindow409`: confirmed + 15 ngày.
    - `TestReschedule_AfterOriginalDatePassed409`.
    - `TestReschedule_ToClosedDate422`.
    - `TestReschedule_KeepsOriginalStartForPolicy`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật danh sách route.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(booking): cancellation with refunds, admin cancel and reschedule`.

---

### Task 9: Chặn đổi lịch phía đối tác khi đã có booking, nối dây, kiểm thử trọn luồng

**Files:**
- Create: `internal/booking/guard.go` và test, `cmd/api/e2e_booking_test.go`
- Modify: `internal/listing/deps.go`, `internal/listing/attraction.go`, `cmd/api/routes.go`, `cmd/api/main.go`

**Interfaces:**
- Trong `listing`:
  - Thêm `BookingGuard interface { ActiveBookingsOnDate(ctx, tx pgx.Tx, listingID uuid.UUID, date time.Time) (int, error); ActiveBookingsForSlot(ctx, tx pgx.Tx, slotID uuid.UUID, from time.Time) (int, error) }` vào `Deps`. Có thể nil trong các test cũ, nil thì bỏ qua kiểm tra.
  - "Active" nghĩa là `pending_payment` hoặc `confirmed`.
  - `AddClosedDate`: có booking active vào ngày đó thì 409 `has_active_bookings`.
  - `UpdateTimeSlot`: nếu đổi giờ, đổi ngày trong tuần hoặc tắt khung mà còn booking active từ hôm nay trở đi thì 409 `has_active_bookings`.
- `booking.Guard` là bên cài đặt (truy vấn `booking_attraction_details` join `bookings`).
- `main`: nối `payment.Provider` theo config, `PaymentHandler = booking`, `BookingGuard = booking`, đăng ký periodic jobs.

- [ ] **Step 1: Viết test fail.**
  - `guard_test.go`:
    - `TestCloseDateWithConfirmedBooking409`.
    - `TestDeactivateSlotWithFutureBooking409`.
    - `TestChangeSlotTimeWithoutBookingsOK`.
  - `e2e_booking_test.go`, qua mux thật với `payment.Fake`, chạy trọn luồng:
    1. admin đặt commission, tạo đối tác
    2. đối tác tạo Listing attraction, gửi duyệt, admin duyệt
    3. khách đăng ký, xác minh và đặt vé (có Idempotency-Key)
    4. webhook fake báo đã trả tiền, đơn chuyển `confirmed`
    5. đối tác check-in bằng QR, đơn chuyển `completed`
    6. chạy `ReleaseWorker` sau thời gian chờ, `GET /v1/partner/balance` có `available = gross − commission`

    Thêm một nhánh: khách hủy một đơn khác trước 48 giờ, kỳ vọng có refund và số dư đúng.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Kiểm tra tay** trên **DB riêng của nhánh** (`make db-branch`), dùng `PAYMENT_PROVIDER = fake`:
  - Đặt vé.
  - `POST /v1/dev/payments/{order_code}/simulate-paid`.
  - Đối tác check-in.
  - Xem số dư.
- [ ] **Step 6: Commit** `feat(booking): partner schedule guards and end-to-end booking flow`.

---

## Không làm trong plan này

- Admin duyệt payout, đánh dấu đã chuyển khoản refund, đối soát PayOS hằng ngày, sao kê (plan 5).
- Xe tuyến, xe bao chuyến, thuê xe (plan 6–8).
- Review, khiếu nại, thông báo trong web (plan 9). Plan này chỉ gửi email qua outbox.

---

## Phụ lục: Hướng dẫn tạo key PayOS

PayOS **không có sandbox riêng**: mọi giao dịch thử là giao dịch thật, nên hãy thử với số tiền nhỏ (vài nghìn đồng). Khi chưa có key, hệ thống chạy với `PAYMENT_PROVIDER=fake` và route chỉ dùng khi dev `POST /v1/dev/payments/{order_code}/simulate-paid`. Route này chạy đúng luồng webhook thật và bị chặn hoàn toàn khi `APP_ENV=prod`.

**Các bước lấy key:**
1. Đăng ký tại https://my.payos.vn/login. Bấm **Đăng ký**, điền thông tin, bấm **Tạo tài khoản**, rồi bấm **Xác thực** trong email PayOS gửi về.
2. Xác thực tổ chức hoặc cá nhân (cần CCCD) theo hướng dẫn trên my.payos.vn.
3. Liên kết tài khoản ngân hàng nhận tiền.
4. Ở màn hình chính, chọn **Tạo kênh thanh toán** và điền thông tin kênh.
5. Mở kênh vừa tạo để lấy **Client ID**, **API Key**, **Checksum Key**.
6. Trong cài đặt kênh, khai báo **Webhook URL** là `https://<domain-backend>/v1/payments/payos/webhook`. URL này phải truy cập được từ internet. Khi chạy ở máy thì dùng ngrok hoặc Cloudflare Tunnel. PayOS sẽ gửi một webhook thử, và backend trả 200 cho `orderCode` không tồn tại.

**Điền vào `.env`** (không commit, `.env` đã được git bỏ qua):
```
PAYMENT_PROVIDER=payos
PAYOS_CLIENT_ID=...
PAYOS_API_KEY=...
PAYOS_CHECKSUM_KEY=...
PAYOS_BASE_URL=https://api-merchant.payos.vn
PAYOS_RETURN_URL=https://<frontend>/thanh-toan/thanh-cong
PAYOS_CANCEL_URL=https://<frontend>/thanh-toan/huy
```

**Kiểm tra sau khi có key:**
1. Đặt một vé giá nhỏ.
2. Mở `checkout_url` và quét QR chuyển khoản.
3. Đợi webhook về, booking phải chuyển sang `confirmed`.
4. `GET /v1/partner/balance` phải thấy tiền ở mục `pending`.

**Nguồn:** [payOS docs](https://payos.vn/docs/), [Môi trường test](https://payos.vn/docs/moi-truong-test/), [Tạo tài khoản](https://payos.vn/docs/huong-dan-su-dung/tao-tai-khoan-payos/), [Webhook](https://payos.vn/docs/du-lieu-tra-ve/webhook/).
