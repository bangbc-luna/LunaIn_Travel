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
)

// invalidPartnerError carries a field-specific Vietnamese message while still
// matching ErrInvalidPartner with errors.Is.
type invalidPartnerError struct{ app *httpx.AppError }

func (e *invalidPartnerError) Error() string        { return e.app.Error() }
func (e *invalidPartnerError) Unwrap() error        { return e.app }
func (e *invalidPartnerError) Is(target error) bool { return target == ErrInvalidPartner }

func invalidPartner(msg string) error {
	return &invalidPartnerError{app: httpx.NewError(ErrInvalidPartner.Status, ErrInvalidPartner.Code, msg)}
}
