package partner

import (
	"net/http"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

var (
	ErrInvalidPartner        = httpx.NewError(http.StatusUnprocessableEntity, "invalid_partner", "Thông tin đối tác không hợp lệ")
	ErrPartnerNotFound       = httpx.NewError(http.StatusNotFound, "partner_not_found", "Không tìm thấy đối tác")
	ErrServiceTypeNotEnabled = httpx.NewError(http.StatusNotFound, "partner_service_type_not_found", "Đối tác chưa từng được bật loại dịch vụ này")
	ErrInvalidSuspendReason  = httpx.NewError(http.StatusUnprocessableEntity, "invalid_suspend_reason", "Vui lòng nhập lý do đình chỉ (tối đa 500 ký tự)")
	ErrAlreadySuspended      = httpx.NewError(http.StatusConflict, "partner_already_suspended", "Đối tác đang bị đình chỉ")
	ErrNotSuspended          = httpx.NewError(http.StatusConflict, "partner_not_suspended", "Đối tác không bị đình chỉ")
	ErrTaxCodeTaken          = httpx.NewError(http.StatusConflict, "tax_code_taken", "Mã số thuế đã được đối tác khác sử dụng")
	ErrInvalidBankAccount    = httpx.NewError(http.StatusUnprocessableEntity, "invalid_bank_account", "Thông tin tài khoản ngân hàng không hợp lệ")
)

// fieldError carries a field-specific Vietnamese message while still matching
// its sentinel (ErrInvalidPartner, ErrInvalidBankAccount) with errors.Is.
type fieldError struct{ app, sentinel *httpx.AppError }

func (e *fieldError) Error() string        { return e.app.Error() }
func (e *fieldError) Unwrap() error        { return e.app }
func (e *fieldError) Is(target error) bool { return target == e.sentinel }

func fieldErr(sentinel *httpx.AppError, msg string) error {
	return &fieldError{app: httpx.NewError(sentinel.Status, sentinel.Code, msg), sentinel: sentinel}
}

func invalidPartner(msg string) error { return fieldErr(ErrInvalidPartner, msg) }

func invalidBankAccount(msg string) error { return fieldErr(ErrInvalidBankAccount, msg) }
