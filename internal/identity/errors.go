package identity

import (
	"net/http"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Business errors of the identity module. Compare with errors.Is.
var (
	ErrEmailTaken          = httpx.NewError(http.StatusConflict, "email_taken", "Email này đã được đăng ký")
	ErrWeakPassword        = httpx.NewError(http.StatusUnprocessableEntity, "weak_password", "Mật khẩu phải dài 8–128 ký tự, có ít nhất một chữ cái và một chữ số")
	ErrInvalidEmail        = httpx.NewError(http.StatusUnprocessableEntity, "invalid_email", "Email không hợp lệ")
	ErrInvalidCode         = httpx.NewError(http.StatusBadRequest, "invalid_code", "Mã xác minh không đúng hoặc đã hết hạn")
	ErrResendTooSoon       = httpx.NewError(http.StatusTooManyRequests, "resend_too_soon", "Vui lòng đợi 60 giây trước khi yêu cầu mã mới")
	ErrInvalidCredentials  = httpx.NewError(http.StatusUnauthorized, "invalid_credentials", "Email hoặc mật khẩu không đúng")
	ErrEmailNotVerified    = httpx.NewError(http.StatusForbidden, "email_not_verified", "Email chưa được xác minh")
	ErrAccountLocked       = httpx.NewError(http.StatusForbidden, "account_locked", "Tài khoản đã bị khóa")
	ErrInvalidRefreshToken = httpx.NewError(http.StatusUnauthorized, "invalid_refresh_token", "Phiên đăng nhập không hợp lệ hoặc đã hết hạn")
	// ErrGoogleLinkRequiresPassword: a Google identity matches an existing
	// account by email, but email_verified is not trusted for that address
	// (not @gmail.com and no hosted domain).
	ErrGoogleLinkRequiresPassword = httpx.NewError(http.StatusConflict, "google_link_requires_password", "Email này đã có tài khoản. Vui lòng đăng nhập bằng mật khẩu.")
	ErrGoogleTokenInvalid         = httpx.NewError(http.StatusUnauthorized, "google_token_invalid", "Đăng nhập Google không hợp lệ")
	// ErrInvalidProfile is the sentinel for profile validation failures; the
	// returned errors carry a field-specific message (see invalidProfile).
	ErrInvalidProfile = httpx.NewError(http.StatusUnprocessableEntity, "invalid_profile", "Thông tin hồ sơ không hợp lệ")
)
