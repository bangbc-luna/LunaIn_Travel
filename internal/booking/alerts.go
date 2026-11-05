package booking

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
)

// alertAdmins queues, in tx, one email per active super admin (M-3).
// Without a job client (some tests) the emails are only logged.
func (s *Service) alertAdmins(ctx context.Context, tx pgx.Tx, subject, text string) error {
	to, err := s.identity.SuperAdminEmails(ctx, tx)
	if err != nil {
		return err
	}
	for _, addr := range to {
		if err := s.enqueueEmail(ctx, tx, mailer.Message{To: addr, Subject: subject, Text: text}); err != nil {
			return err
		}
	}
	return nil
}

// mismatchResult: the HandlePaid result needs a human look (underpaid,
// overpaid or a refunded duplicate).
func mismatchResult(result string) bool {
	for _, r := range strings.Split(result, "+") {
		switch r {
		case ResultUnderpaid, ResultOverpaid, ResultDuplicateRefunded:
			return true
		}
	}
	return false
}

// alertMismatch alerts the super admins about a processed transfer whose
// result is a mismatch, or that the provider's status did not confirm.
func (c *confirmation) alertMismatch(ctx context.Context) error {
	if !mismatchResult(c.out.Result) && !c.ev.NeedsReview {
		return nil
	}
	oc := strconv.FormatInt(c.ev.OrderCode, 10)
	text := "Một giao dịch thanh toán cần được kiểm tra.\n\n" +
		"Mã checkout: " + c.co.Code + "\n" +
		"Mã thanh toán (orderCode): " + oc + "\n" +
		"Mã tham chiếu giao dịch: " + c.ev.Reference + "\n" +
		"Số tiền chuyển (webhook): " + formatVND(c.ev.AmountPaid) + "\n" +
		"Số tiền phải trả: " + formatVND(c.pay.Amount) + "\n" +
		"Kết quả xử lý: " + c.out.Result + "\n"
	if c.ev.NeedsReview {
		text += "Trạng thái tại cổng thanh toán chưa xác nhận khoản tiền này.\n"
	}
	text += "\nVui lòng mở giao dịch trong trang quản trị để xem xét và bấm Hoàn thành khi đã xử lý xong.\n"
	return c.s.alertAdmins(ctx, c.tx, "[LunaIn] Cảnh báo lệch tiền thanh toán "+oc+" (checkout "+c.co.Code+")", text)
}
