package listing

import (
	"net/http"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

var (
	ErrInvalidListing        = httpx.NewError(http.StatusUnprocessableEntity, "invalid_listing", "Thông tin Listing không hợp lệ")
	ErrListingNotFound       = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy Listing")
	ErrPartnerSuspended      = httpx.NewError(http.StatusForbidden, "partner_suspended", "Đối tác đang bị đình chỉ, không thể tạo hoặc sửa Listing")
	ErrServiceTypeNotEnabled = httpx.NewError(http.StatusUnprocessableEntity, "service_type_not_enabled", "Loại dịch vụ này chưa được bật cho đối tác")
)

// fieldError carries a field-specific Vietnamese message while still matching
// its sentinel (ErrInvalidListing) with errors.Is.
type fieldError struct{ app, sentinel *httpx.AppError }

func (e *fieldError) Error() string        { return e.app.Error() }
func (e *fieldError) Unwrap() error        { return e.app }
func (e *fieldError) Is(target error) bool { return target == e.sentinel }

func invalidListing(msg string) error {
	return &fieldError{
		app:      httpx.NewError(ErrInvalidListing.Status, ErrInvalidListing.Code, msg),
		sentinel: ErrInvalidListing,
	}
}
