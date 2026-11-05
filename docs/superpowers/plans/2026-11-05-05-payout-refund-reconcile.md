# Plan 5 — Payout, Refund admin, Đối soát — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Đối tác rút được tiền khả dụng kèm sao kê. Super Admin duyệt, đánh dấu đã chuyển hoặc từ chối Payout, điều chỉnh số dư bằng tay, đánh dấu Refund đã chuyển khoản và xử lý các payment `needs_review`. Một job đối soát hằng ngày bắt được tiền đã vào mà hệ thống chưa ghi nhận, và bắt được số dư cache lệch sổ cái, rồi báo Super Admin.

**Architecture:**

- Module mới `internal/payout` sở hữu bảng `payouts`. Nó import `ledger`, `partner`, `settings`, `files`.
- `ledger` có thêm ba bút toán `payout_hold`, `payout_reversal`, `adjustment`, cùng sao kê theo `seq` và `VerifyAll`.
- `payment` có thêm admin cho Refund và cho payment `needs_review`, bảng `payment_reconciliations`, và `ReconcilePayments`. `ReconcilePayments` đưa tiền đi qua **đúng** đường webhook: `ProcessWebhook`, hoặc `applyTransfer` tách ra từ bước 5 của nó, với cùng `DedupeKey` và `HandlePaid`.
- Module mới `internal/reconcile` không sở hữu bảng nào. Nó điều phối job hằng ngày: gọi `payment` và `ledger`, lưu kết quả qua `payment`, và gửi email cảnh báo cho Super Admin qua outbox.

**Tech Stack:** như plan 1–4 (Go 1.27, PostgreSQL 17, pgx/v5, sqlc, goose, River, `net/http` ServeMux). PayOS vẫn chỉ có Fake. Không dùng API chuyển tiền của PayOS: admin chuyển khoản tay rồi ghi nhận lại.

**Spec:**
- [spec hệ thống](../specs/2026-11-04-lunain-travel-backend-design.md) §5.2, §6.3, §8, §9, §12, §14
- [thiết kế DB](../specs/2026-11-04-database-schema.md) §1.1, §4, §7, §8, §10
- [ADR 0004](../../adr/0004-partner-ledger.md), [GLOSSARY](../../../GLOSSARY.md)
- Rulings 1–26 của plan 4 (`.claude/worktrees/booking-payment/.superpowers/sdd/2026-11-05-04-booking-payment-ledger/progress.md`) vẫn ràng buộc.

## Global Constraints

- Mọi Global Constraints của plan 1–4 vẫn áp dụng:
  - uuid v7, clock được inject, lỗi JSON tiếng Việt, test chạy trên DB riêng clone từ template đã migrate.
  - Path id sai định dạng hoặc không tồn tại thì 404. Body sai thì 422. Body có trường lạ thì 400.
  - Tiền là `bigint` VND.
  - Migration đã có trên `main` là bất biến: tạo file mới từ `00015`, rồi chạy `make migrations-sum`.
  - Mỗi worktree dùng DB riêng tạo bằng `make db-branch`.
  - Mỗi route `/v1/admin/*` và `/v1/partner/*` mới phải có trong `adminRoutes`/`partnerRoutes` của `cmd/api/routes_test.go`.
- **Thứ tự khóa (DB §1.1):**
  ```
  users → partners → listings → (sức chứa) → checkouts → bookings (id tăng dần)
        → dòng con của booking (settlements, refunds) → payments (FOR NO KEY UPDATE)
        → payouts → partner_balances
  ```
  - Không bao giờ gọi provider khi đang mở transaction.
- **Payout (spec §9):**
  - `min_payout_amount` mặc định 100000 (`platform_settings`, đọc bằng `settings.GetTx`).
  - Chỉ rút được khi `available > 0` và `amount ≤ available`, kèm `amount ≥ min_payout_amount`.
  - Tạo yêu cầu thì ghi ngay `payout_hold` = −amount vào `available`, trong cùng transaction.
  - Mỗi đối tác chỉ có **một** payout đang mở (`requested`/`approved`): unique index partial.
  - Luồng trạng thái: `requested → approved → paid`. `requested|approved → rejected` thì ghi `payout_reversal` = +amount vào `available`.
  - `available` được phép âm.
- **Sao kê:** đi theo `seq`, không theo `created_at`.
  - `statement_seq_from` = `statement_seq_to` của payout trước (bất kể trạng thái) + 1, hoặc 1 nếu chưa có payout nào.
  - `statement_seq_to` = `seq` của bút toán `payout_hold` của chính payout đó.
  - `opening_available` = `available_after` tại `seq_from − 1` (0 nếu là `seq` 0). `closing_available` = `available_after` của `payout_hold`.
  - Các dòng sao kê là mọi bút toán ngăn `available` trong khoảng đó. Bất biến: `opening + Σ dòng = closing`.
- **Bút toán (chốt dấu và ngăn cho plan 5):**
  - `payout_hold`: `amount < 0`, ngăn `available`, có `payout_id`.
  - `payout_reversal`: `amount > 0`, ngăn `available`, có `payout_id`.
  - `adjustment`: ngăn `available`, dấu tùy ý (≠ 0), bắt buộc `reason` và `created_by`, `payout_id` NULL, `booking_id` không bắt buộc.
- **Ngân hàng:**
  - Sau khi chuẩn hóa, `account_holder` phải khớp `^[A-Z]+( [A-Z]+)*$`. Áp cho tài khoản đối tác, snapshot trong payout và tài khoản hoàn tiền của khách.
  - Payout chụp lại tài khoản tại thời điểm yêu cầu (`partner.CurrentBankAccount`). Đổi tài khoản khi payout đang mở **không** ảnh hưởng payout đó: snapshot là căn cứ.
- **Audit:** mọi thao tác admin lên tiền hoặc trạng thái đều gọi `audit.Record` trong cùng transaction. Các action là:
  - `payout.approve`, `payout.mark_paid`, `payout.reject`
  - `ledger.adjustment`
  - `refund.complete`
  - `payment.review_seen`, `payment.review_resolve`
  - `reconciliation.run`

  Đổi tài khoản ngân hàng đối tác ghi `partner.bank_account.set`, với actor là user đối tác.
- **Không flow nào của plan 5 đưa `checkouts` ra khỏi `paid`.** Nếu có thì phải xóa `paid_payment_id` (CHECK hai chiều). Các flow review và đối soát không ghi bảng `checkouts`.
- **Tiền vào chỉ đi một đường:** `HandlePaid` trong transaction đã insert dòng `payment_events` với `DedupeKey(orderCode, reference)`. Đối soát không bao giờ ghi bút toán hoặc đổi booking trực tiếp.
- **Thông báo:** chỉ gửi email qua outbox (`mailer.EnqueueTx`). Chuông trong web để plan 9.
- **Commit:** không thêm trailer `Co-Authored-By` hay bất kỳ dòng ghi công AI nào.

## Review Focus

1. **Hai yêu cầu rút song song của cùng một đối tác:** chỉ tạo đúng một payout và một `payout_hold`. Yêu cầu kia nhận 409 `payout_already_open`. `available` chỉ giảm một lần, `Verify` không lỗi. Test ở Task 3.
2. **Release ghi đúng lúc đang tạo payout:** bút toán release commit sau `payout_hold` (có `seq` lớn hơn) phải nằm trong sao kê của payout **kế tiếp**. Không bút toán `available` nào nằm ở hai sao kê, cũng không bút toán nào bị lọt. `opening + Σ = closing` đúng cho cả hai sao kê. Test ở Task 3.
3. **Từ chối payout sau khi số dư đã âm:** giữ 500.000 (available về 0), sau đó release −100.000 (available −100.000), rồi từ chối. Kết quả `payout_reversal` +500.000, available 400.000, `Verify` không lỗi. Test ở Task 4.
4. **Job đối soát chạy cùng lúc với webhook thật của cùng một giao dịch:** chỉ có một dòng `payment_events` chính, một lần xác nhận và một bút toán. Test ở Task 6.
5. **Đối tác đổi tài khoản ngân hàng khi payout đang mở:** payout vẫn giữ snapshot. Màn admin trả `current_bank_differs: true` và email `paid` dùng snapshot. Lần đổi được ghi audit. Test ở Task 4.

---

## Cấu trúc file

```
internal/platform/db/migrations/00015_payout.sql          # payouts, FK ledger→payouts, CHECK dấu/ngăn, CHECK tên chủ TK
internal/platform/db/migrations/00016_payment_ops.sql     # payment_reconciliations, payment_events.amount, payments.review_*, refunds reminder
internal/platform/validate/bank.go                        # luật [A-Z ]
internal/ledger/{ledger.go,payout.go,statement.go,verify_all.go,http.go,queries.sql}
internal/payout/{service.go,admin.go,http_partner.go,http_admin.go,queries.sql,store/,*_test.go}
internal/payment/{refund_admin.go,review.go,reissue.go,reconcile.go,http_admin.go,webhook.go,payos.go,fake.go}
internal/reconcile/{service.go,schedule.go,jobs.go,http.go,*_test.go}
internal/booking/{alerts.go,jobs.go,backoff.go,create.go}
internal/identity/admins.go                               # SuperAdminEmails, EmailOf
internal/partner/{bank.go,owner.go}
cmd/api/{main.go,routes.go,routes_test.go,e2e_payout_test.go}
```

---

### Task 1: Schema payout, ràng buộc sổ cái, vận hành payment

**Files:**
- Create: `internal/platform/db/migrations/00015_payout.sql`, `00016_payment_ops.sql` (cập nhật `CHECKSUMS` bằng `make migrations-sum`)
- Create: `internal/payout/queries.sql` (tối thiểu một query), mục `payout` trong `sqlc.yaml`, `internal/payout/schema_test.go`
- Modify: `internal/platform/validate/bank.go` (và test)

**Schema:**

- **`00015_payout.sql`:**
  - `payouts`, đúng DB §8:
    - `id`, `partner_id` FK partners, `amount bigint CHECK > 0`.
    - `status CHECK IN ('requested','approved','paid','rejected')`.
    - `bank_bin`, `bank_name`, `account_number`, `account_holder` đều NOT NULL, cùng CHECK định dạng như refunds (`^[0-9]{6}$`, `^[0-9]{6,19}$`). `account_holder ~ '^[A-Z]+( [A-Z]+)*$'`.
    - `bank_account_id` FK `partner_bank_accounts`: dòng đã được chụp lại.
    - `statement_seq_from`, `statement_seq_to`, `opening_available`, `closing_available` đều `bigint NULL`. Chúng được điền trong cùng transaction tạo payout, ngay sau `payout_hold`, vì FK từ bút toán buộc phải có dòng payout trước. `CHECK ((statement_seq_to IS NULL) = (statement_seq_from IS NULL) AND ... )` cho cả bốn cột, cùng `CHECK (statement_seq_from <= statement_seq_to)`.
    - `requested_by` FK users NOT NULL, `requested_at` NOT NULL, `approved_by/at`, `paid_by/at`, `transfer_reference`, `proof_file_id` FK files, `rejected_by/at`, `reject_reason`.
    - CHECK theo trạng thái:
      - `approved` hoặc `paid` ⇒ có `approved_at` và `approved_by`.
      - `paid` ⇔ có `paid_at`, `paid_by`, `transfer_reference` và `proof_file_id`.
      - `rejected` ⇔ có `rejected_at`, `rejected_by` và `reject_reason`.
    - Unique index `payouts_one_open_idx (partner_id) WHERE status IN ('requested','approved')`.
    - Index `(status, id DESC)`, `(partner_id, id DESC)`.
  - `ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_payout_fk FOREIGN KEY (payout_id) REFERENCES payouts (id)`.
  - Thêm CHECK có tên lên `ledger_entries`:
    - `ledger_entries_payout_hold_check`: `kind <> 'payout_hold' OR (bucket = 'available' AND amount < 0)`.
    - `ledger_entries_payout_reversal_check`: `kind <> 'payout_reversal' OR (bucket = 'available' AND amount > 0)`.
    - `ledger_entries_adjustment_check`: `kind <> 'adjustment' OR (bucket = 'available' AND payout_id IS NULL)`.
  - `partner_bank_accounts`:
    - `ADD COLUMN created_by uuid NULL REFERENCES users (id)` (dòng cũ để NULL).
    - `ADD CONSTRAINT partner_bank_accounts_holder_check CHECK (account_holder ~ '^[A-Z]+( [A-Z]+)*$')`.
- **`00016_payment_ops.sql`:**
  - `payment_reconciliations`:
    - `id`, `run_date date UNIQUE`, `status CHECK IN ('ok','mismatch')`.
    - `checked_payments int ≥ 0`, `mismatches jsonb NOT NULL DEFAULT '[]'` (CHECK là mảng JSON).
    - `started_at`, `finished_at`, `run_by uuid NULL FK users` (NULL nghĩa là job chạy).
  - `payment_events`: `ADD COLUMN amount bigint NULL CHECK (amount > 0)`, là số tiền của lần chuyển mà `HandlePaid` đã xử lý.
    - Backfill: `UPDATE ... SET amount = (payload->'data'->>'amount')::bigint WHERE processed_at IS NOT NULL AND result NOT IN ('unknown_order','no_amount','awaiting_status','already_processed') AND jsonb_typeof(payload->'data'->'amount') = 'number'`.
    - Thêm index `(result, received_at) WHERE result = 'awaiting_status'`.
  - `payments`: thêm `review_seen_at`, `review_seen_by` FK users (admin đã mở xem), `review_resolved_at`, `review_resolved_by` FK users, `review_note` NULL (ghi chú không bắt buộc).
    - `CHECK ((review_seen_at IS NULL) = (review_seen_by IS NULL))`, `CHECK ((review_resolved_at IS NULL) = (review_resolved_by IS NULL))`, `CHECK (review_resolved_at IS NULL OR review_seen_at IS NOT NULL)`.
    - `CHECK ((review_seen_at IS NULL AND review_resolved_at IS NULL) OR status = 'needs_review')`.
    - Index `(id) WHERE status = 'needs_review' AND review_resolved_at IS NULL`.
  - `refunds`:
    - `ADD COLUMN bank_info_reminder_count int NOT NULL DEFAULT 0 CHECK (≥ 0)`, `ADD COLUMN bank_info_reminded_at timestamptz NULL`.
    - `ADD CONSTRAINT refunds_holder_check CHECK (account_holder IS NULL OR account_holder ~ '^[A-Z]+( [A-Z]+)*$')`. Áp cùng CHECK đó cho `customer_refund_accounts.account_holder`.
    - Index `(status, created_at) WHERE status = 'awaiting_bank_info'`.
  - Trước khi viết CHECK tên chủ tài khoản, chạy `SELECT` trên DB nhánh để chắc không có dòng nào vi phạm. Dữ liệu dev đều đi qua bộ chuẩn hóa nên chỉ có thể vi phạm khi có chữ số hoặc dấu câu.
- **`validate.NormalizeBankAccount`:** sau khi chuẩn hóa, holder không khớp `^[A-Z]+( [A-Z]+)*$` thì trả message `"account_holder chỉ gồm chữ cái A–Z không dấu và khoảng trắng"`.

- [ ] **Step 1: Viết test fail.**
  - `internal/platform/validate/bank_test.go`:
    - `TestNormalizeBankAccount_HolderLettersOnly`: `"Nguyễn Văn Đức"` cho ra `"NGUYEN VAN DUC"` và hợp lệ. `"NGUYEN VAN A2"`, `"CTY TNHH. ABC"` và `"O'NEIL"` đều trả message trên.
  - `internal/payout/schema_test.go`, chèn trực tiếp bằng SQL:
    - `TestPayoutsOneOpenPerPartner`: payout `requested` thứ hai cho cùng đối tác thì lỗi 23505 `payouts_one_open_idx`. Sau khi payout đầu chuyển `rejected` thì tạo được.
    - `TestPayoutPaidNeedsReferenceAndProof`: `paid` thiếu `proof_file_id` thì 23514.
    - `TestLedgerPayoutHoldSign`: `payout_hold` +1 thì 23514 `ledger_entries_payout_hold_check`. `payout_hold` ở ngăn `pending` thì 23514.
    - `TestLedgerPayoutReversalSign`.
    - `TestLedgerAdjustmentAvailableOnly`: `adjustment` ở ngăn `pending` thì 23514. Thiếu `reason` thì 23514.
    - `TestLedgerPayoutFK`: `payout_id` không tồn tại thì 23503.
    - `TestBankHolderCheck`: `partner_bank_accounts.account_holder = 'NGUYEN VAN A2'` thì 23514.
    - `TestReviewResolvedOnlyWhenNeedsReview`.
    - `TestReconciliationRunDateUnique`.
    - `TestPaymentEventsAmountBackfilled`: chèn event trước migration `00016`, bằng cách migrate tới `00015`, insert rồi migrate tiếp, nếu helper hỗ trợ. Nếu không, kiểm tra biểu thức backfill bằng một câu `SELECT` trên payload mẫu.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Viết migration và code validate,** rồi chạy `make migrations-sum` và `make sqlc`. Các test của partner và refund có tên chủ tài khoản kiểu `"NGUYEN VAN A"` vẫn phải xanh.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(db): payouts, payout ledger constraints, payment ops schema`.

---

### Task 2: Ledger: bút toán payout và adjustment, sao kê, `VerifyAll`, điều chỉnh của admin

**Files:**
- Create: `internal/ledger/{payout.go,statement.go,verify_all.go,payout_test.go,statement_test.go,verify_all_test.go,http_admin_test.go}`
- Modify: `internal/ledger/{ledger.go,queries.sql,http.go}`, `cmd/api/routes.go`, `cmd/api/routes_test.go`, `GLOSSARY.md`

**Interfaces:**
- Consumes: schema của Task 1.
- Produces:
  - `post` được tổng quát hóa: `entry{kind, bucket string; amount int64; bookingID, payoutID, createdBy *uuid.UUID; reason *string}`. `post(ctx, q, partnerID, now, entries...) ([]Posted, error)`. Các hàm booking hiện có giữ nguyên chữ ký và hành vi, mọi test cũ vẫn xanh.
  - `type Posted struct{ Seq, AvailableAfter int64 }`.
  - `func (s *Service) PostPayoutHold(ctx context.Context, tx pgx.Tx, partnerID, payoutID uuid.UUID, amount int64) (Posted, error)`:
    - Gọi `EnsurePartnerBalance` rồi khóa `partner_balances`.
    - Lỗi khi `amount ≤ 0` (`ErrInvalidAmount`), khi `available ≤ 0` (`ErrNothingAvailable`), hoặc khi `amount > available` (`ErrExceedsAvailable`).
    - Hợp lệ thì ghi −amount vào `available`.
    - Ghi trùng (unique `ledger_entries_payout_kind_idx`) thì trả `ErrAlreadyPosted`.
  - `func (s *Service) PostPayoutReversal(ctx, tx, partnerID, payoutID uuid.UUID, amount int64) (Posted, error)`: ghi +amount vào `available`, ghi trùng thì `ErrAlreadyPosted`.
  - `type Adjustment struct{ PartnerID uuid.UUID; BookingID *uuid.UUID; Amount int64; Reason string; CreatedBy uuid.UUID }`.
  - `func (s *Service) PostAdjustment(ctx, tx, a Adjustment) (Posted, error)`:
    - Lỗi `ErrInvalidAmount` khi `Amount == 0` hoặc `|Amount| > 1_000_000_000_000`.
    - `Reason` sau trim phải có 1..500 rune.
    - `BookingID` thuộc đối tác khác thì `ErrBookingNotOfPartner`, map từ 23503 của FK `(booking_id, partner_id)`.
  - `func (s *Service) AvailableAfter(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, seq int64) (int64, error)`: `seq` 0 trả 0. `seq` không tồn tại là lỗi.
  - `type StatementLine struct{ Seq int64; Kind string; Amount, AvailableAfter int64; BookingID, PayoutID *uuid.UUID; Reason *string; CreatedAt time.Time }` (JSON snake_case).
  - `func (s *Service) Statement(ctx context.Context, partnerID uuid.UUID, fromSeq, toSeq int64) ([]StatementLine, error)`: chỉ lấy ngăn `available`, sắp theo `seq` tăng dần.
  - `func (s *Service) LastSeq(ctx context.Context, partnerID uuid.UUID) (int64, error)`: trả 0 nếu chưa có dòng nào.
  - `type Drift struct{ PartnerID uuid.UUID; BookingID *uuid.UUID; Problem string }`.
  - `func (s *Service) VerifyAll(ctx context.Context) ([]Drift, error)`, gồm ba kiểm tra:
    1. `Verify` cho từng `partner_balances.partner_id`.
    2. Mỗi `booking_settlements` chưa release có `net_pending` bằng tổng bút toán `pending` của booking đó.
    3. Mỗi settlement đã release có tổng bút toán `pending` bằng 0 (spec §9).

    Lỗi truy vấn trả `error`. Lệch dữ liệu thì trả về trong danh sách `Drift`.
  - `func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer, partners *partner.Service)`. Routes admin (`RequireRole(super_admin)`):
    - `GET /v1/admin/partners/{id}/ledger?after_seq=&limit=` (mặc định 50, tối đa 200) trả `{balance, entries: [StatementLine kèm bucket], next_after_seq}`. Đối tác không tồn tại thì 404.
    - `POST /v1/admin/partners/{id}/adjustments` với body `{amount, reason, booking_id?}`:
      - Transaction: `partner.Status(tx, id, false)` (không có thì 404), `PostAdjustment`, rồi `audit.Record("ledger.adjustment", target_kind "partner", after {amount, booking_id, seq})`.
      - Trả 201 `{seq, available_after}`.
- GLOSSARY thêm ba thuật ngữ:
  - **Statement** (Sao kê): danh sách bút toán `available` giữa hai Payout, kèm số dư đầu kỳ và cuối kỳ.
  - **Adjustment** (Điều chỉnh): bút toán tay của Super Admin, bắt buộc có lý do.
  - **Reconciliation** (Đối soát): job hằng ngày so payments với PayOS và so cache với sổ cái.

- [ ] **Step 1: Viết test fail.**
  - `payout_test.go`. Dùng `TestConfirmThenRelease` của plan 4 để có `available` 900.000. Payout chỉ cần một dòng `payouts` chèn bằng SQL trong test.
    - `TestPayoutHold_TakesAvailable`: hold 300.000, kỳ vọng available 600.000, `Posted.AvailableAfter == 600_000`, `Seq` bằng `last_seq` trước đó + 1, `Verify` không lỗi.
    - `TestPayoutHold_ExceedsAvailable`: hold 900.001 thì `ErrExceedsAvailable`.
    - `TestPayoutHold_NothingAvailable`: available −100.000 (hoàn 100%, release) thì `ErrNothingAvailable`. Available 0 cũng trả `ErrNothingAvailable`.
    - `TestPayoutHold_Twice`: lần hai trả `ErrAlreadyPosted`.
    - `TestPayoutReversal_RestoresAvailable`.
    - `TestAdjustment_PlusMinus`: +50.000 rồi −70.000, kỳ vọng available 880.000, `reason` và `created_by` được lưu, `Verify` không lỗi.
    - `TestAdjustment_OtherPartnersBooking`: trả `ErrBookingNotOfPartner`.
    - `TestAdjustment_ZeroOrNoReason`.
  - `statement_test.go`:
    - `TestStatement_OnlyAvailableInRange`: confirm (pending), release (2 dòng), adjustment, kỳ vọng `Statement(1, last)` trả 2 dòng (release available và adjustment), đúng thứ tự `seq`.
    - `TestAvailableAfter_SeqZeroIsZero`.
  - `verify_all_test.go`:
    - `TestVerifyAll_Clean`: hai đối tác có bút toán, kết quả rỗng.
    - `TestVerifyAll_DetectsCacheDrift`: `UPDATE partner_balances SET available = available + 1` bằng SQL, kỳ vọng một `Drift` đúng `PartnerID`.
    - `TestVerifyAll_DetectsSettlementDrift`: sửa `booking_settlements.net_pending` bằng SQL, kỳ vọng `Drift` có `BookingID`.
  - `http_admin_test.go`:
    - `TestHTTP_AdminAdjustment_AuditedAndPosted`: kỳ vọng 201, có một dòng audit `ledger.adjustment` với `actor_user_id` của admin.
    - `TestHTTP_AdminAdjustment_UnknownPartner404`, `TestHTTP_AdminAdjustment_BadBody422`, `TestHTTP_AdminLedgerPage`.
    - `TestHTTP_AdminLedger_PartnerForbidden`: role partner nhận 403.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật `adminRoutes` và GLOSSARY.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(ledger): payout and adjustment entries, statements, VerifyAll`.

---

### Task 3: Audit tài khoản ngân hàng, đối tác tạo và xem Payout

**Files:**
- Create: `internal/payout/{service.go,http_partner.go,helpers_test.go,service_test.go,http_partner_test.go}`, `internal/partner/owner.go`
- Modify: `internal/partner/bank.go` (và `bank_test.go`), `internal/partner/queries.sql`, `internal/payout/queries.sql`, `cmd/api/routes.go`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: Task 2 (`PostPayoutHold`, `AvailableAfter`, `Statement`, `Balance`). Ngoài ra dùng `partner.Status(ctx, tx, id, lock bool) (string, error)`, `partner.CurrentBankAccount(ctx, tx, partnerID) (*BankAccount, error)`, `settings.GetTx(ctx, tx) (Settings, error)`.
- Produces:
  - `partner.SetBankAccount`:
    - Lưu `created_by = actorUserID`.
    - Trong cùng transaction gọi `audit.Record` với `Action "partner.bank_account.set"`, `TargetKind "partner"`, `TargetID` là partner id, `Before` là tài khoản current cũ (hoặc nil), `After` là tài khoản mới, `ActorUserID` là user đối tác.
  - `func (s *partner.Service) OwnerEmail(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID) (string, error)`: email tài khoản `owner` (join `partner_members` với `users`), dùng để gửi email Payout.
  - `payout.Deps{Pool; Clock; Ledger *ledger.Service; Partners *partner.Service; Settings *settings.Service; Files *files.Service; Jobs *river.Client[pgx.Tx]; Log *slog.Logger}`, `payout.NewService(Deps) *Service`.
  - `type Payout struct{ ID, PartnerID uuid.UUID; Amount int64; Status string; Bank BankSnapshot; StatementSeqFrom, StatementSeqTo, OpeningAvailable, ClosingAvailable int64; RequestedAt time.Time; ApprovedAt, PaidAt, RejectedAt *time.Time; TransferReference, RejectReason *string }`. `BankSnapshot{BankBIN, BankName, AccountNumber, AccountHolder string}`.
  - `type PayoutView struct{ Payout; Statement []ledger.StatementLine }`.
  - `func (s *Service) Request(ctx context.Context, partnerID, actorUserID uuid.UUID, amount int64) (PayoutView, error)`, chạy trong một transaction theo thứ tự khóa:
    1. `partner.Status(tx, id, true)` (FOR SHARE). Đối tác `suspended` thì 409 `partner_suspended` (spec §5.2, xem Câu hỏi 1).
    2. `settings.GetTx`: `amount < min_payout_amount` thì 422 `payout_below_minimum`, message nêu mức tối thiểu.
    3. `CurrentBankAccount`: nil thì 422 `bank_account_required`.
    4. Insert `payouts` (`requested`, snapshot, statement NULL). Gặp 23505 `payouts_one_open_idx` thì 409 `payout_already_open`.
    5. Tính `seqFrom` theo quy tắc ở Global Constraints. Query là `MAX(statement_seq_to)` theo đối tác, không tính payout vừa tạo.
    6. `ledger.PostPayoutHold`: `ErrNothingAvailable` và `ErrExceedsAvailable` thì 422 `insufficient_available`.
    7. Gọi `opening = ledger.AvailableAfter(tx, partnerID, seqFrom−1)`, rồi `UPDATE` bốn cột statement của payout.

    Sau commit thì đọc `Statement(seqFrom, seqTo)`. Lỗi `amount ≤ 0` thì 422 `invalid_amount`.
  - `func (s *Service) ListMine(ctx, partnerID uuid.UUID, f ListFilter) ([]Payout, *uuid.UUID, error)` (`ListFilter{Status *string; Limit int; Cursor *uuid.UUID}`, giống `booking.ListFilter`, sắp theo id giảm dần).
  - `func (s *Service) GetMine(ctx, partnerID, id uuid.UUID) (PayoutView, error)`: payout của đối tác khác trả 404.
  - `type Preview struct{ Available, MinPayoutAmount int64; OpenPayoutID *uuid.UUID; CanRequest bool; StatementFromSeq, OpeningAvailable int64; Statement []ledger.StatementLine }`.
  - `func (s *Service) Preview(ctx, partnerID uuid.UUID) (Preview, error)`: sao kê tạm tính từ `seqFrom` tới `LastSeq`. `CanRequest` đúng khi đối tác active, có tài khoản ngân hàng, không có payout mở, và `available ≥ max(min_payout_amount, 1)`.
  - `func (s *Service) PartnerRoutes(mux httpx.Router, iss *auth.Issuer, requireMember func(http.Handler) http.Handler)`. Routes đối tác (`Authenticate → RequireRole(partner) → RequireMember`, `partner_id` lấy từ `partner.IDFrom`):
    - `POST /v1/partner/payouts` với body `{amount}`, trả 201.
    - `GET /v1/partner/payouts`
    - `GET /v1/partner/payouts/preview`
    - `GET /v1/partner/payouts/{id}`

- [ ] **Step 1: Viết test fail.**
  - `internal/partner/bank_test.go`:
    - `TestSetBankAccount_Audited`: hai lần đổi tạo hai dòng audit. Dòng thứ hai có `before.account_number` là tài khoản cũ. `created_by` được lưu.
    - `TestSetBankAccount_HolderWithDigits422`.
  - `internal/payout/helpers_test.go`: dựng đối tác active có tài khoản ngân hàng, rồi dùng ledger thật đưa `available` lên 900.000 (`PostBookingConfirmed` + `Release` trên một booking dựng bằng SQL như helper của ledger). Clock cố định `2026-11-05T09:00:00+07:00`.
  - `service_test.go`:
    - `TestRequest_HoldsAndSnapshots`: rút 300.000, kỳ vọng `requested`, available 600.000, bank snapshot bằng tài khoản current, `statement_seq_from == 1`, `statement_seq_to` = `seq` của hold, `opening_available == 0`, `closing_available == 600_000`, `opening + Σ Statement.Amount == closing`, `Verify` không lỗi.
    - `TestRequest_SecondStatementStartsAfterPrevious`: payout 1 bị từ chối bằng SQL cùng `PostPayoutReversal`, release thêm 100.000, rồi tạo payout 2. Kỳ vọng `seq_from(2) == seq_to(1) + 1`, sao kê 2 có dòng `payout_reversal` +300.000 và release +100.000, `opening(2) == closing(1)`.
    - `TestRequest_BelowMinimum422`: 99.999 thì `payout_below_minimum`.
    - `TestRequest_ExceedsAvailable422`: 900.001.
    - `TestRequest_NegativeAvailable422`.
    - `TestRequest_NoBankAccount422`.
    - `TestRequest_OneOpen409`.
    - `TestRequest_Suspended409`.
    - `TestRequest_Concurrent` (Review Focus 1): hai goroutine cùng rút 500.000. Kỳ vọng đúng một thành công, một lỗi `payout_already_open` (hoặc `insufficient_available` nếu transaction thứ hai đọc sau commit), đúng một dòng `payout_hold`, available 400.000, `Verify` không lỗi.
    - `TestRequest_ReleaseRacesStatementBoundary` (Review Focus 2):
      - Transaction A gọi `Request` và dừng ở hook test, ngay trước commit và sau `PostPayoutHold`. Transaction B release một booking khác (+200.000) và phải chờ khóa `partner_balances`. Commit A, rồi B.
      - Kỳ vọng `seq` của release lớn hơn `statement_seq_to`, release không nằm trong `Statement` của payout 1, nhưng nằm trong `Preview().Statement`.
      - Từ chối payout 1, tạo payout 2. Kỳ vọng sao kê 2 có release đúng một lần và cả hai sao kê đều thỏa `opening + Σ = closing`.
      - Hook là trường không export `beforeCommit func()` trên `Service`, chỉ set trong test.
    - `TestPreview_CanRequestRules`.
  - `http_partner_test.go`:
    - `TestHTTP_RequestPayout201`.
    - `TestHTTP_GetOtherPartnersPayout404`: đối tác A xem payout của đối tác B nhận 404.
    - `TestHTTP_MalformedID404`, `TestHTTP_UnknownField400`, `TestHTTP_ListMine`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật `partnerRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(payout): partner payout requests with statements; audit bank account changes`.

---

### Task 4: Admin duyệt, chuyển tiền, từ chối Payout

**Files:**
- Create: `internal/payout/{admin.go,http_admin.go,admin_test.go,http_admin_test.go}`
- Modify: `internal/payout/queries.sql`, `cmd/api/routes.go`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: Task 3. Ngoài ra dùng `files.Owned(ctx, tx, id, uploadedBy uuid.UUID, purpose string) error` với purpose `payout_proof` (đã có trong `files.rules`), `ledger.PostPayoutReversal`, `partner.OwnerEmail`, `partner.CurrentBankAccount`.
- Produces:
  - `AdminFilter{Status *string; PartnerID *uuid.UUID; Limit int; Cursor *uuid.UUID}`, `func (s *Service) AdminList(ctx, f AdminFilter) ([]Payout, *uuid.UUID, error)`.
  - `type AdminPayoutView struct{ PayoutView; Balance ledger.Balance; CurrentBank *partner.BankAccount; CurrentBankDiffers bool; ProofFileID *uuid.UUID }`.
  - `func (s *Service) AdminGet(ctx, id uuid.UUID) (AdminPayoutView, error)`: `CurrentBankDiffers` đúng khi BIN hoặc số tài khoản hiện tại khác snapshot.
  - `func (s *Service) Approve(ctx, actor, id uuid.UUID) (Payout, error)`:
    - Khóa `payouts FOR UPDATE`. Không ở `requested` thì 409 `invalid_payout_state`.
    - Chuyển `approved`, ghi audit `payout.approve`.
    - Email cho owner: "Yêu cầu rút tiền đã được duyệt".
  - `type MarkPaidInput struct{ TransferReference string; ProofFileID uuid.UUID }`.
  - `func (s *Service) MarkPaid(ctx, actor, id uuid.UUID, in MarkPaidInput) (Payout, error)`:
    - `TransferReference` sau trim phải có 1..100 rune. Thiếu `proof_file_id` thì 422.
    - Transaction: `files.Owned(tx, proof, actor, "payout_proof")` (sai thì 422 `invalid_file`) → khóa payout → không ở `approved` thì 409.
    - Chuyển `paid`, ghi audit `payout.mark_paid` với `after` gồm reference, file và snapshot ngân hàng.
    - Email nêu số tiền, mã giao dịch và tài khoản **snapshot**.
    - **Không** ghi bút toán nào, vì tiền đã rời `available` lúc hold.
  - `func (s *Service) Reject(ctx, actor, id uuid.UUID, reason string) (Payout, error)`:
    - `reason` sau trim phải có 1..500 rune.
    - Khóa payout trước, sau đó mới gọi `ledger.PostPayoutReversal` (theo thứ tự `payouts → partner_balances`).
    - Đang `requested` hoặc `approved` thì chuyển `rejected`, nếu không thì 409.
    - Ghi audit `payout.reject` kèm lý do. Email cho owner kèm lý do.
  - Đối tác bị đình chỉ: admin vẫn approve, mark paid hoặc reject được payout đang mở (spec §5.2 "tự xử lý").
  - `func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer)`. Routes admin:
    - `GET /v1/admin/payouts?status=&partner_id=&limit=&cursor=`
    - `GET /v1/admin/payouts/{id}`
    - `POST /v1/admin/payouts/{id}/approve` (body rỗng hoặc `{}`)
    - `POST /v1/admin/payouts/{id}/mark-paid` với body `{transfer_reference, proof_file_id}`
    - `POST /v1/admin/payouts/{id}/reject` với body `{reason}`

- [ ] **Step 1: Viết test fail** `admin_test.go`:
  - `TestApproveThenMarkPaid`: kỳ vọng `paid`, `paid_by` là admin, có 2 dòng audit, available không đổi so với sau hold, `Verify` không lỗi, có 2 email (dùng `mailer.Recorder` qua River test client, hoặc đếm job `send_email`).
  - `TestMarkPaid_RequiresApproved409`.
  - `TestMarkPaid_ProofOfAnotherAdminOrPurpose422`.
  - `TestReject_FromRequestedAndApproved`.
  - `TestReject_AfterPaid409`.
  - `TestReject_AfterBalanceWentNegative` (Review Focus 3):
    - Có available 500.000. Rút 500.000 (available 0). Hủy hoàn 100% một booking khác (net −100.000) rồi release, available −100.000.
    - Reject. Kỳ vọng `payout_reversal` +500.000, available 400.000, `Verify` không lỗi.
    - Tạo được payout mới.
  - `TestReject_Twice409NoSecondReversal`.
  - `TestBankChangedWhileOpen` (Review Focus 5):
    - Đối tác đổi tài khoản sau khi tạo payout.
    - Kỳ vọng `AdminGet.CurrentBankDiffers == true`, snapshot không đổi. `MarkPaid` thành công và email chứa số tài khoản cũ.
    - Có dòng audit `partner.bank_account.set`.
  - `TestSuspendedPartnerPayoutStillProcessable`.
  - `http_admin_test.go`:
    - `TestHTTP_AdminPayoutList_FilterStatus`.
    - `TestHTTP_AdminPayout_MalformedID404`.
    - `TestHTTP_MarkPaid_BadBody422`.
    - `TestHTTP_AdminPayout_PartnerRole403`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật `adminRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(payout): admin approve, mark paid with proof, reject with reversal`.

---

### Task 5: Refund admin, xử lý `needs_review`, link mới sau khi chuyển thiếu (M-4), cảnh báo lệch tiền (M-3)

**Files:**
- Create: `internal/payment/{refund_admin.go,review.go,reissue.go,http_admin.go,refund_admin_test.go,review_test.go}`, `internal/identity/admins.go` (và test), `internal/booking/{alerts.go,reissue_test.go,alerts_test.go}`
- Modify: `internal/payment/{service.go,queries.sql,refund.go}`, `internal/booking/{confirm.go,create.go,jobs.go}`, `internal/identity/queries.sql`, `cmd/api/routes.go`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: Task 1 (các cột review và reminder). Ngoài ra dùng `files.Owned` (purpose `refund_proof`), `audit.Record`, `mailer.EnqueueTx`.
- Produces:
  - Trong `identity`:
    - `func (s *Service) SuperAdminEmails(ctx context.Context, tx pgx.Tx) ([]string, error)`: user có `role = 'super_admin'` và `status = 'active'`, sắp theo email.
    - `func (s *Service) EmailOf(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (string, error)`.
  - `payment.OpsDeps{Jobs *river.Client[pgx.Tx]; Users interface{ EmailOf(ctx, pgx.Tx, uuid.UUID) (string, error) }; Files interface{ Owned(ctx, pgx.Tx, uuid.UUID, uuid.UUID, string) error }}`, `func (s *Service) SetOps(d OpsDeps)`. Hàm này được gọi trong `main` sau khi tạo job client. Test cũ không gọi `SetOps` vẫn chạy được: email chỉ được log.
  - **Refund admin:**
    - `RefundFilter{Status, Reason *string; BookingID, PaymentID *uuid.UUID; Limit int; Cursor *uuid.UUID}`, `func (s *Service) AdminListRefunds(ctx, f RefundFilter) ([]Refund, *uuid.UUID, error)`, `func (s *Service) AdminGetRefund(ctx, id uuid.UUID) (Refund, error)`. JSON admin có thêm `checkout_id`, `provider_reference`, `proof_file_id`, `completed_by`.
    - `type CompleteRefundInput struct{ TransferReference string; ProofFileID *uuid.UUID }`.
    - `func (s *Service) CompleteRefund(ctx, actor, id uuid.UUID, in CompleteRefundInput) (Refund, error)`:
      - Reference sau trim phải có 1..100 rune.
      - Transaction: khóa `refunds FOR UPDATE` (chỉ dòng này).
      - `awaiting_bank_info` thì 409 `refund_awaiting_bank_info`. `completed` thì 409 `refund_already_completed`.
      - Có proof thì kiểm tra `Files.Owned(..., "refund_proof")`.
      - Chuyển `completed` với `completed_by/at`, ghi audit `refund.complete`.
      - Email khách (`EmailOf` theo `checkouts.customer_id`): "Khoản hoàn {amount}đ đã được chuyển, mã giao dịch {ref}".
    - `func (s *Service) RemindAwaitingBankInfo(ctx context.Context) (int, error)`:
      - Chọn refund `awaiting_bank_info` có `created_at ≤ now − 3 ngày`, `(bank_info_reminded_at IS NULL OR ≤ now − 3 ngày)` và `bank_info_reminder_count < 5`, tối đa 200 dòng.
      - Mỗi refund một transaction: khóa, kiểm tra lại, enqueue email nhắc nhập tài khoản, tăng `count` và set `reminded_at`.
      - Xem Câu hỏi 3.
  - **Xử lý `needs_review`:**
    - `PaymentFilter{Status *string; OpenReview bool; CheckoutID *uuid.UUID; Limit int; Cursor *uuid.UUID}`, `func (s *Service) AdminListPayments(ctx, f PaymentFilter) ([]Payment, *uuid.UUID, error)`.
    - `type PaymentDetail struct{ Payment; Events []EventSummary; Refunds []Refund; ReviewNote *string; ReviewResolvedAt *time.Time }` với `EventSummary{DedupeKey, Result string; Amount *int64; ReceivedAt time.Time}`. `func (s *Service) AdminGetPayment(ctx, id uuid.UUID) (PaymentDetail, error)`.
    - Hai bước (đã chốt với người dùng, Câu hỏi 5): **Đã xem** và **Hoàn thành**.
    - `func (s *Service) MarkReviewSeen(ctx, actor, id uuid.UUID) (Payment, error)`: FE gọi khi admin mở chi tiết giao dịch.
      - Transaction: `LockCheckoutRow` → `LockPayment`. Không ở `needs_review` thì 409 `payment_not_in_review`.
      - Đã có `review_seen_at` thì trả nguyên, không ghi gì (idempotent). Chưa có thì set `review_seen_at/by`, ghi audit `payment.review_seen`.
    - `func (s *Service) ResolveReview(ctx, actor, id uuid.UUID, note *string) (Payment, error)`: nút **Hoàn thành**.
      - `note` không bắt buộc; nếu có thì sau trim tối đa 1000 rune.
      - Transaction: `LockCheckoutRow` → `LockPayment` (FOR NO KEY UPDATE). Không ở `needs_review`, hoặc đã hoàn thành rồi, thì 409 `payment_not_in_review`.
      - Nếu chưa có `review_seen_at` thì set luôn (hoàn thành ngụ ý đã xem). Set `review_resolved_at/by`, `review_note`, ghi audit `payment.review_resolve`.
      - **Không** đổi `payments.status` và không ghi `checkouts`. Mọi khoản hoàn cho tiền thừa, thiếu hoặc trùng đã được `HandlePaid` tạo tự động, nên Hoàn thành chỉ là admin xác nhận đã xử lý xong.
    - Query `SetPaymentStatus` và `RecordPaymentReceived` xóa toàn bộ `review_*` (cả seen lẫn resolved) khi status mới là `needs_review`. Tiền mới về sẽ mở lại review từ đầu.
    - Trong danh sách admin, mỗi payment có `review_state` ∈ {`new`, `seen`, `resolved`} để FE hiện trạng thái.
  - **M-4:**
    - `func (s *Service) ReissueAfterMismatch(ctx context.Context, checkoutID uuid.UUID) (Payment, error)`, chạy trong transaction `LockCheckoutRow` → `LockCheckoutBookingRows` → `LockCheckoutPayments`:
      - Đã có payment `pending` thì trả lại nó.
      - Ngược lại chỉ reissue khi checkout `open`, payment mới nhất (id lớn nhất) là `needs_review` có `provider_reference` (tức đã nhận tiền chuyển thiếu, đã hoàn), và `expires_at > now`. Khi đó gọi `ReservePayment(tx, checkoutID, latest.Amount, latest.ExpiresAt)`, **không** gia hạn giữ chỗ (Câu hỏi 4).
      - Không đủ điều kiện thì `ErrPaymentNotFound`.
    - `booking.Pay`: khi `StartPayment` trả `ErrPaymentNotFound` thì gọi `ReissueAfterMismatch` rồi `StartPayment` lại. Vẫn lỗi thì giữ `booking_not_payable` như cũ.
  - **M-3:** `booking.alertAdmins(ctx, tx, subject, text string) error` enqueue một email cho mỗi địa chỉ trong `identity.SuperAdminEmails`. `HandlePaid` gọi nó trong cùng transaction khi kết quả là `underpaid`, `overpaid` hoặc `duplicate_refunded`, hoặc khi `ev.NeedsReview`. Email nêu mã checkout, orderCode, số tiền webhook và số tiền phải trả.
  - Worker gọi `RemindAwaitingBankInfo` (kind `payment_remind_refunds`) nằm ở `internal/reconcile` và lịch 09:00 VN được đăng ký ở Task 6 cùng `DailyAt`. Task này chỉ cần hàm và test của nó.
  - `func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer)` trên `payment.Service`.
  - Routes admin:
    - `GET /v1/admin/refunds?status=&reason=&booking_id=&payment_id=&limit=&cursor=`
    - `GET /v1/admin/refunds/{id}`
    - `POST /v1/admin/refunds/{id}/complete` với body `{transfer_reference, proof_file_id?}`
    - `GET /v1/admin/payments?status=&open_review=true&limit=&cursor=`
    - `GET /v1/admin/payments/{id}`
    - `POST /v1/admin/payments/{id}/seen` (body rỗng hoặc `{}`)
    - `POST /v1/admin/payments/{id}/resolve` với body `{note?}` (nút Hoàn thành)

- [ ] **Step 1: Viết test fail.**
  - `refund_admin_test.go`, dùng refund dựng qua helper booking của plan 4 (khách hủy trước 48 giờ, có tài khoản):
    - `TestCompleteRefund_Pending`: kỳ vọng `completed`, có audit, có 1 email gửi tới email tài khoản của khách.
    - `TestCompleteRefund_AwaitingBankInfo409`.
    - `TestCompleteRefund_Twice409`.
    - `TestCompleteRefund_ProofWrongPurpose422`.
    - `TestAdminListRefunds_FilterStatusReason`.
    - `TestRemindAwaitingBankInfo_Cadence`: tại +2 ngày có 0 email, +3 ngày có 1, +4 ngày có 0, +6 ngày có 1. Sau 5 lần nhắc thì không gửi thêm. Refund đã chuyển `pending` thì không được nhắc.
  - `review_test.go`:
    - `TestMarkReviewSeen_Idempotent`: lần đầu set `review_seen_at/by` và có 1 audit `payment.review_seen`; lần hai không đổi `review_seen_at`, không thêm audit. `review_state` chuyển `new → seen`.
    - `TestResolveReview_Overpaid`: dùng kịch bản `TestPaid_Overpaid`. Hoàn thành không ghi chú làm payment vẫn `needs_review` nhưng có `review_resolved_at`, `review_seen_at` cũng được set, `review_state = resolved`, có audit, `checkouts.status` vẫn `paid`.
    - `TestResolveReview_NotInReview409`, `TestResolveReview_Twice409`.
    - `TestNewTransferReopensReview`: xem và hoàn thành, rồi một giao dịch trùng có `NeedsReview` (status lag), kỳ vọng cả `review_seen_at` và `review_resolved_at` NULL, `review_state = new`.
    - `TestAdminListPayments_OpenReview`.
  - `internal/booking/reissue_test.go`:
    - `TestPayAfterUnderpaid_IssuesNewLink`: chuyển thiếu, `POST /pay` trả 200 với `payment.checkout_url` mới (orderCode khác), `expires_at` giữ nguyên. Trả đủ trên link mới thì `confirmed`, có đúng 1 `booking_earning`, `Verify` không lỗi.
    - `TestPayAfterUnderpaid_FullOnOldLinkStillAccepted`: chuyển thiếu, `/pay`, rồi trả đủ trên link **cũ**. Kỳ vọng `accepted_replaced_link`, payment mới chuyển `cancelled`, đúng 1 bút toán.
    - `TestPayAfterUnderpaid_HoldExpired409`.
  - `alerts_test.go`:
    - `TestUnderpaidAlertsSuperAdmins`: 2 super admin active và 1 bị khóa thì có đúng 2 email cảnh báo.
    - `TestConfirmedNoAlert`.
  - `internal/identity/admins_test.go`: `TestSuperAdminEmails_ActiveOnly`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật `adminRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(payment): refund admin, needs_review resolution, reissue after underpayment, mismatch alerts`.

---

### Task 6: Đối soát hằng ngày

**Implementer phải đọc tài liệu PayOS** (`https://payos.vn/docs/api/`, mục lấy thông tin link thanh toán) để xác nhận response `GET /v2/payment-requests/{id}` có mảng `transactions` (`reference`, `amount`, `transactionDateTime`, ...). Client hiện chỉ decode `status` và `amountPaid` (`payos.go:290`). PayOS **không** có API liệt kê giao dịch, nên spec §8 ("đối chiếu với danh sách giao dịch PayOS") được hiểu là kiểm tra từng payment trong một cửa sổ thời gian. Nếu tài liệu không có `transactions` thì bỏ phần khôi phục ở bước (b) bên dưới, chỉ ghi `missing_transfer` và cảnh báo, rồi ghi khác biệt này vào báo cáo.

**Điều kiện bắt buộc để khôi phục:** `transactions[].reference` phải chính là trường mà webhook dùng làm `data.reference` (cùng giá trị cho cùng một lần chuyển), để `DedupeKey(orderCode, reference)` của đối soát và của webhook trùng nhau. Nếu tài liệu không khẳng định được điều này, hoặc response mẫu cho thấy hai giá trị khác nhau, thì **không** khôi phục: chỉ ghi `missing_transfer` và cảnh báo. Khôi phục với khóa sai sẽ làm webhook đến sau bị tính là lần chuyển thứ hai (hoàn tiền trùng).

**Files:**
- Create: `internal/payment/{reconcile.go,reconcile_test.go}`, `internal/reconcile/{service.go,schedule.go,jobs.go,http.go,service_test.go,schedule_test.go,http_test.go}`
- Modify: `internal/payment/{payos.go,fake.go,webhook.go,queries.sql,payos_test.go,fake_test.go}`, `internal/booking/confirm.go` (chuyển `ResultAlreadyProcessed` sang `payment`), `cmd/api/routes.go`, `cmd/api/routes_test.go`

**Interfaces:**
- Consumes: Task 2 (`ledger.VerifyAll`), Task 5 (`identity.SuperAdminEmails`, `payment.OpsDeps`). Ngoài ra dùng `payment.ProcessWebhook(ctx, body []byte, h PaymentHandler) error`, `DedupeKey`, `StatusLagLimit` (30 phút).
- Produces:
  - `Status` có thêm `Transactions []StatusTransaction`, với `StatusTransaction{Reference string; Amount int64; At time.Time}`. `PayOS.GetStatus` decode mảng này. `Fake` ghi lại mỗi lần `MarkPaidRef` thành một transaction và trả về trong `GetStatus`. `LagNextStatus` vẫn trả mảng rỗng.
  - `payment.ResultAlreadyProcessed = "already_processed"` (booking dùng hằng này).
  - `ProcessWebhook` bước 5 được tách thành `func (s *Service) applyTransfer(ctx context.Context, key string, p Payment, ev PaidEvent, payload []byte, h PaymentHandler) (PaidOutcome, bool, error)`:
    - Insert `payment_events` kèm `amount = ev.AmountPaid` (dòng `already_processed` thì set `amount` NULL).
    - Nếu `n == 0` thì trả `false`.
    - `HandlePaid`, rồi `FinishPaymentEvent`.
    - `CancelLinkBestEffort` sau commit.

    Webhook và đối soát cùng gọi hàm này. Hành vi webhook không đổi, mọi test plan 4 vẫn xanh.
  - Mismatch kinds (`payment.Mismatch{Kind string; PaymentID *uuid.UUID; OrderCode *int64; PartnerID, BookingID *uuid.UUID; Reference *string; ProviderAmount, RecordedAmount *int64; Detail string}`, JSON snake_case):
    - `stale_awaiting_processed`
    - `recovered_transfer`
    - `missing_transfer`
    - `recorded_exceeds_provider`
    - `provider_error`
    - `needs_review_open`
    - `balance_drift`
  - `func (s *Service) ReconcilePayments(ctx context.Context, now time.Time, h PaymentHandler) (checked int, ms []Mismatch, err error)`. Chạy ngoài mọi transaction, gọi provider từng payment một, tối đa 500 payment mỗi lần:
    - **(a) Dòng `awaiting_status` còn treo:** `result = 'awaiting_status'`, `received_at ≤ now − StatusLagLimit`, và chưa có dòng chính (dedupe key bỏ hậu tố `:awaiting`). Đưa `payload` vào `ProcessWebhook(ctx, payload, h)`. Đây chính là luồng webhook: `awaitStatus` thấy đã quá 30 phút nên xử lý với `needs_review`. Ghi `stale_awaiting_processed` (`Detail` là result). `ProcessWebhook` lỗi thì ghi `provider_error`.
    - **(b) Cửa sổ:** payment có `created_at ≥ now − 72 giờ` (mọi trạng thái), cộng mọi payment `needs_review` chưa resolve. Với mỗi payment:
      - Gọi `GetStatus` ngoài transaction.
      - Nếu `IsOrderNotFound` và payment chưa có `checkout_url` thì bỏ qua (link chưa từng được tạo). Lỗi khác thì ghi `provider_error`.
      - `recorded = SUM(payment_events.amount)` của payment, không tính dòng `:awaiting`.
      - `provider = Status.AmountPaid`.
      - Nếu `provider > recorded`: với mỗi transaction có `Reference ≠ ""`, dedupe key chưa có, và `Amount ≤ provider − recorded` (cập nhật lại sau mỗi lần áp dụng), dựng `PaidEvent{AmountPaid: tx.Amount, Reference: tx.Reference, PaidAt: tx.At, NeedsReview: true, Raw: json(tx)}` rồi `applyTransfer` với payload `{"source":"reconcile","order_code":..,"transaction":{..}}`. Kết quả ghi `recovered_transfer`. Phần còn thiếu ghi `missing_transfer`.
      - Nếu `provider < recorded` và payment `paid_at` hoặc event cuối cũ hơn 30 phút thì ghi `recorded_exceeds_provider`.
    - **(c)** Mọi payment `needs_review` chưa resolve ghi `needs_review_open`.
  - `func (s *Service) SaveReconciliation(ctx, runDate time.Time, checked int, ms []Mismatch, runBy *uuid.UUID, started, finished time.Time) (Reconciliation, error)`: upsert theo `run_date`, `status = ok` khi `len(ms) == 0`. Cùng với `ListReconciliations(ctx, limit int, before *time.Time)` và `GetReconciliation(ctx, runDate time.Time)`.
  - `reconcile.Service` (`Deps{Pool; Clock; Payments *payment.Service; Ledger *ledger.Service; Handler payment.PaymentHandler; Identity *identity.Service; Jobs *river.Client[pgx.Tx]; Log}`):
    - `func (s *Service) Run(ctx context.Context, runBy *uuid.UUID) (payment.Reconciliation, error)`:
      1. `runDate` = ngày VN của `now`.
      2. `ReconcilePayments`.
      3. `ledger.VerifyAll`, mỗi `Drift` thành một `balance_drift`.
      4. `SaveReconciliation`.
      5. Nếu `mismatch` thì trong một transaction enqueue email `"[LunaIn] Đối soát {YYYY-MM-DD}: {n} chênh lệch"` tới mọi `SuperAdminEmails`, liệt kê theo kind.
      6. Nếu `runBy` khác nil thì trong cùng transaction đó ghi audit `reconciliation.run`.
  - `func DailyAt(hour, minute int, loc *time.Location) river.PeriodicSchedule`: `Next(t)` trả lần kế tiếp đúng giờ VN, luôn sau `t`.
  - Worker trong `internal/reconcile/jobs.go`:
    - `DailyArgs` (kind `reconcile_daily`) và `DailyWorker{Service *Service}` gọi `Run(ctx, nil)`.
    - `RemindRefundsArgs` (kind `payment_remind_refunds`) và `RemindRefundsWorker{Payments *payment.Service}` gọi `RemindAwaitingBankInfo` (Task 5).
  - `reconcile.PeriodicJobs() []*river.PeriodicJob`:
    - `reconcile_daily` lúc 02:30 VN.
    - `payment_remind_refunds` lúc 09:00 VN.

    Cả hai dùng `MaxAttempts: 1` như plan 4.
  - `func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer)`.
  - Routes admin:
    - `GET /v1/admin/reconciliations?limit=&before=YYYY-MM-DD`
    - `GET /v1/admin/reconciliations/{run_date}`: ngày sai định dạng hoặc chưa chạy thì 404.
    - `POST /v1/admin/reconciliations/run`: chạy đồng bộ, trả 200 kèm kết quả.

- [ ] **Step 1: Viết test fail.**
  - `payos_test.go`: `TestGetStatus_DecodesTransactions` (vector theo tài liệu). `fake_test.go`: `TestFake_StatusListsTransfers`.
  - `payment/reconcile_test.go`, dựng toàn bộ service với `booking` làm handler (helper của plan 4):
    - `TestReconcile_ReplaysStaleAwaiting`:
      - Webhook khi status lag trả 503 và để lại dòng `awaiting_status`.
      - Ở +31 phút, `ReconcilePayments` làm booking `confirmed`, payment `needs_review`, có 1 `booking_earning`, mismatch `stale_awaiting_processed`.
      - Chạy lại lần nữa thì không thêm gì.
    - `TestReplayAwaitingPayloadStillVerifies`: `payload` đọc lại từ jsonb vẫn qua `VerifyWebhook`, dù thứ tự key và khoảng trắng đã đổi.
    - `TestReconcile_RecoversTransferWithoutWebhook`: `Fake.MarkPaidRef` nhưng **không** gửi webhook. Kỳ vọng `confirmed`, `recovered_transfer`, dedupe key là `DedupeKey(oc, ref)`. Gửi webhook đó sau này thì 200 và không ghi thêm gì (Review Focus 4).
    - `TestReconcile_RacesLiveWebhook` (Review Focus 4): goroutine A `ProcessWebhook`, goroutine B `ReconcilePayments` trên cùng giao dịch, lặp 20 lần. Kỳ vọng mỗi lần đúng 1 dòng event chính, 1 `booking_earning`, `Verify` không lỗi.
    - `TestReconcile_EmptyReferenceNotDoubleCounted`: webhook không có reference đã xử lý (khóa `raw:`), status có transaction `Reference ""` hoặc `provider == recorded`. Kỳ vọng không recover và không có mismatch.
    - `TestReconcile_MissingTransferWhenNoTransactions`: Fake cho `AmountPaid` lớn hơn nhưng không có transaction. Kỳ vọng `missing_transfer`, không ghi bút toán nào.
    - `TestReconcile_ProviderErrorRecorded`.
    - `TestReconcile_NeedsReviewOpenListed`.
    - `TestReconcile_OutsideWindowSkipped`: payment `paid` từ 4 ngày trước thì không gọi `GetStatus` (đếm số lần gọi trên Fake).
  - `reconcile/service_test.go`:
    - `TestRun_OkNoEmail`.
    - `TestRun_BalanceDriftAlerts`: sửa cache bằng SQL. Kỳ vọng `mismatch` có `balance_drift`, có email tới super admin.
    - `TestRun_SameDayUpserts`: chạy 2 lần trong ngày cho ra 1 dòng.
    - `TestRun_ManualRunAudited`.
  - `schedule_test.go`: `TestDailyAt_Next`:
    - 02:29 VN cho ra 02:30 cùng ngày.
    - 02:30:00 VN cho ra 02:30 hôm sau.
    - 23:00 UTC cho ra 02:30 VN của ngày VN kế tiếp.
  - `http_test.go`:
    - `TestHTTP_RunAndGet`.
    - `TestHTTP_BadRunDate404`.
    - `TestHTTP_PartnerRole403`.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.** Cập nhật `adminRoutes`.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `feat(reconcile): daily payment and balance reconciliation with admin alerts`.

---

### Task 7: Vệ sinh job: TTL cho idempotency, chống dòng lỗi chặn batch

**Files:**
- Create: `internal/booking/{backoff.go,backoff_test.go,idempotency_cleanup_test.go}`
- Modify: `internal/booking/{jobs.go,queries.sql,jobs_test.go}`

**Interfaces:**
- Consumes: các job của plan 4 (`ExpireHolds`, `MarkNoShows`, `ReleaseDue`).
- Produces:
  - `type jobBackoff struct` (trong bộ nhớ, an toàn cho goroutine):
    - `skip(now) []uuid.UUID`
    - `fail(id uuid.UUID, now time.Time) (failures int)`: chờ `1m × 2^(failures−1)`, tối đa 1 giờ.
    - `ok(id uuid.UUID)`

    Mỗi job có một instance trên `Service`.
  - Ba query `ListHoldsToRelease` (skip theo `checkout_id`), `ListNoShowCandidates` và `ListDueReleases` nhận thêm `@skip::uuid[]` và lọc `AND NOT (x = ANY(@skip))`. `ListDueReleases` thuộc ledger, nên `ledger.DueReleases` có thêm tham số `skip []uuid.UUID`.
  - Dòng lỗi thì log `Error` kèm `failures`. Khi `failures ≥ 5` thì log thêm `"poison row"` đúng một lần.
  - Hook test không export `beforeItem func(id uuid.UUID) error` trên `Service`.
  - `const idempotencyTTL = 24 * time.Hour`. `func (s *Service) PurgeIdempotencyKeys(ctx context.Context) (int64, error)` xóa các dòng có `created_at < now − idempotencyTTL`, mỗi lần tối đa 5000 dòng (`DELETE ... WHERE ctid IN (SELECT ... LIMIT 5000)`).
  - `PurgeIdempotencyArgs` (kind `booking_purge_idempotency`) chạy mỗi giờ, thêm vào `booking.PeriodicJobs()`.

- [ ] **Step 1: Viết test fail.**
  - `backoff_test.go`: `TestJobBackoff_Doubling`: kỳ vọng thời gian chờ 1, 2, 4, ..., tối đa 60 phút, và `ok` xóa trạng thái.
  - `jobs_test.go`:
    - `TestRelease_PoisonRowDoesNotStarve`: batch size test = 2 (trường không export `batchSize`). Hai booking đầu luôn lỗi qua hook. Lần chạy 1 xử lý 0. Lần 2 (cùng `now`) bỏ qua hai dòng lỗi và release booking thứ 3. Ở `now + 2 phút` thì thử lại dòng lỗi.
    - `TestExpireHolds_PoisonCheckoutSkipped`.
  - `idempotency_cleanup_test.go`: `TestPurgeIdempotencyKeys_TTL`: key 25 giờ tuổi bị xóa, 23 giờ tuổi còn. Sau khi xóa, dùng lại key cũ thì tạo booking mới.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt.**
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...`.
- [ ] **Step 5: Commit** `fix(booking): back off failing job rows; purge idempotency keys after 24h`.

---

### Task 8: Nối dây, kiểm thử trọn luồng, tài liệu

**Files:**
- Create: `cmd/api/e2e_payout_test.go`
- Modify: `cmd/api/main.go`, `cmd/api/routes.go`, `cmd/api/routes_test.go`, `docs/superpowers/specs/2026-11-04-database-schema.md` (§4 audit, §7, §8, §10), `docs/superpowers/plans/README.md`

**Interfaces:**
- Consumes: mọi task trước.
- Produces:
  - `main`:
    - Tạo `payout.NewService`, `reconcile.NewService`.
    - `paymentSvc.SetOps(payment.OpsDeps{Jobs, Users: idSvc, Files: fileSvc})`.
    - Thêm worker `reconcile.DailyWorker` và `reconcile.RemindRefundsWorker`, gán Service trước khi `jobClient.Start`.
    - `jobs.NewClient(..., append(booking.PeriodicJobs(), reconcile.PeriodicJobs()...)...)`.
  - `muxDeps` có thêm `Payout`, `Reconcile`. `buildMux` đăng ký:
    - `Payout.PartnerRoutes(rr, iss, RequireMember)`, `Payout.AdminRoutes`
    - `Ledger.AdminRoutes(rr, iss, partnerSvc)`
    - `Payment.AdminRoutes`
    - `Reconcile.AdminRoutes`
  - Tài liệu DB:
    - §7: `payment_reconciliations` ✅ plan 5 (thêm các cột), `payment_events.amount`, `payments.review_*`, `refunds` reminder.
    - §8: `payouts` ✅ (thêm `bank_account_id`, CHECK trạng thái, statement nullable chỉ trong lúc insert), dấu và ngăn của các kind payout và `adjustment`, FK `payout_id`.
    - §4: `admin_audit_log` cũng ghi thao tác đổi tài khoản ngân hàng của đối tác, với actor là user đối tác.
    - §10: thêm "sao kê `opening + Σ = closing`".
  - `plans/README.md`: đánh dấu plan 5 xong.

- [ ] **Step 1: Viết test fail** `e2e_payout_test.go`, qua mux thật với `payment.Fake`:
  1. admin đặt hoa hồng 10%, tạo đối tác, đối tác `PUT /v1/partner/bank-account`
  2. Listing attraction được duyệt, khách đặt 2 đơn (mỗi đơn gross 380.000), webhook fake báo đã trả, check-in cả hai, rồi `ReleaseDue` sau thời gian chờ. Kỳ vọng `GET /v1/partner/balance` có `available = 684.000`.
  3. `GET /v1/partner/payouts/preview`: `can_request` true, `opening_available` 0, các dòng sao kê cộng lại bằng 684.000.
  4. `POST /v1/partner/payouts {amount: 500000}` trả 201 với `closing_available` 184.000.
  5. Gửi lần hai thì 409.
  6. Admin `approve`, upload `payout_proof` qua `POST /v1/files` (purpose `payout_proof`), `mark-paid`.
  7. Đối tác `GET /v1/partner/payouts/{id}`: `paid`, `opening + Σ statement = closing`, available 184.000.
  8. Admin `POST /v1/admin/reconciliations/run`: `status = ok`.
  9. Admin adjustment −200.000 với lý do. Kỳ vọng available −16.000, và preview `can_request` false.

  Kiểm tra `ledger.Verify` cho đối tác ở cuối.
- [ ] **Step 2: Chạy.** Kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt nối dây** và cập nhật tài liệu.
- [ ] **Step 4: Chạy lại.** Kỳ vọng PASS. Sau đó chạy `go vet ./... && go test -race ./...` và `go tool sqlc diff` (không có diff).
- [ ] **Step 5: Kiểm tra tay** trên **DB riêng của nhánh** (`make db-branch`, migrate từ đầu), dùng `PAYMENT_PROVIDER=fake`:
  1. Đặt vé, `POST /v1/dev/payments/{order_code}/simulate-paid`, check-in.
  2. Tạm đặt `release_delay_days = 0` để chạy release, sau đó trả lại 1.
  3. Đối tác xem preview và rút tiền.
  4. Admin duyệt và mark-paid với file proof trên MinIO dev.
  5. `POST /v1/admin/reconciliations/run` trả `ok`.
  6. Ghi kết quả vào báo cáo task.
- [ ] **Step 6: Commit** `feat(payout): wire payouts, admin money ops and reconciliation; e2e payout flow`.

---

## Không làm trong plan này

- Các loại dịch vụ xe (plan 6–8), nên `charter_late_confirm_min_hours` và giữ ghế/xe khi xác nhận lại chưa dùng tới.
- Chuông thông báo trong web, review, complaint, cửa sổ giờ check-in (plan 9). Báo cáo tổng hợp (plan 10).
- Tự động chuyển tiền qua API chi hộ của PayOS. Hoàn tiền ngoài chính sách hủy (spec §2: không có).
- Key PayOS thật: phụ lục ở plan 4 vẫn là hướng dẫn.

## Các quyết định đã chốt

1. **Đối tác bị đình chỉ có được tạo Payout không?** Đã chốt theo spec §5.2: không tạo được Payout mới (409 `partner_suspended`). Admin vẫn duyệt, chuyển hoặc từ chối được payout đang mở.
2. **Tiền có trên PayOS mà webhook không bao giờ tới:** **đã chốt: có**, tự xác nhận và báo admin. Khoản tiền đi qua `HandlePaid` với `needs_review` và gửi cảnh báo, nhưng chỉ khi giao dịch có reference và nằm trong phần chênh lệch số tiền. Nếu tài liệu PayOS không có `transactions` thì chỉ cảnh báo.
3. **Nhắc khách nhập tài khoản hoàn tiền:** **đã chốt: 3 ngày một lần, tối đa 5 lần.** Sau đó refund vẫn nằm trong danh sách admin `awaiting_bank_info`.
4. **Link mới sau khi chuyển thiếu (M-4):** **đã chốt: không gia hạn giữ chỗ.** Link mới hết hạn đúng `hold_expires_at` cũ.
5. **Xử lý `needs_review`:** **đã chốt:** hai bước. Admin mở chi tiết thì ghi **Đã xem**; nút **Hoàn thành** (ghi chú không bắt buộc) chốt giao dịch đã xử lý xong. Hoàn tiền thừa, thiếu hoặc trùng đã được tạo tự động. Admin không có thao tác "chấp nhận số tiền thiếu để xác nhận đơn", vì spec §8 nói không xác nhận khi lệch tiền.
6. **Chứng từ khi đánh dấu Payout `paid`:** **theo spec: bắt buộc** (spec §9 "kèm mã giao dịch và chứng từ"). Với Refund thì không bắt buộc (spec §6.3 chỉ yêu cầu mã giao dịch).
