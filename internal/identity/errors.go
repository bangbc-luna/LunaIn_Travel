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
	ErrGoogleTokenInvalid  = httpx.NewError(http.StatusUnauthorized, "google_token_invalid", "Đăng nhập Google không hợp lệ")
)
