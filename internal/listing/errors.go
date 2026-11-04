package listing

import (
	"net/http"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

var (
	ErrInvalidListing        = httpx.NewError(http.StatusUnprocessableEntity, "invalid_listing", "Thông tin Listing không hợp lệ")
	ErrListingNotFound       = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy Listing")
	ErrPartnerSuspended      = httpx.NewError(http.StatusForbidden, "partner_suspended", "Đối tác đang bị đình chỉ, không thể tạo hoặc sửa Listing")
	ErrInvalidListingState   = httpx.NewError(http.StatusConflict, "invalid_listing_state", "Trạng thái Listing không cho phép thao tác này")
	ErrListingIncomplete     = httpx.NewError(http.StatusUnprocessableEntity, "listing_incomplete", "Listing chưa đủ thông tin để gửi duyệt")
	ErrServiceTypeNotEnabled = httpx.NewError(http.StatusUnprocessableEntity, "service_type_not_enabled", "Loại dịch vụ này chưa được bật cho đối tác")
	// ErrServiceTypeNotSupported: the catalog knows the type but listing has
	// no detail model for it yet (only attraction_ticket today).
	ErrServiceTypeNotSupported = httpx.NewError(http.StatusUnprocessableEntity, "service_type_not_supported", "Loại dịch vụ này chưa hỗ trợ đăng bán")
)

// supportedServiceType reports whether listing can sell serviceType. Every
// per-service-type switch in this package falls back to
// ErrServiceTypeNotSupported, so a new type fails closed until it is wired.
func supportedServiceType(serviceType string) bool {
	switch serviceType {
	case attractionServiceType:
		return true
	default:
		return false
	}
}

// fieldError carries a field-specific Vietnamese message while still matching
// its sentinel (ErrInvalidListing) with errors.Is.
type fieldError struct{ app, sentinel *httpx.AppError }

func (e *fieldError) Error() string        { return e.app.Error() }
func (e *fieldError) Unwrap() error        { return e.app }
func (e *fieldError) Is(target error) bool { return target == e.sentinel }

// withMessage is sentinel with a more specific Vietnamese message.
func withMessage(sentinel *httpx.AppError, msg string) error {
	return &fieldError{app: httpx.NewError(sentinel.Status, sentinel.Code, msg), sentinel: sentinel}
}

func invalidListing(msg string) error { return withMessage(ErrInvalidListing, msg) }

func incompleteListing(msg string) error { return withMessage(ErrListingIncomplete, msg) }
