package httpx

import (
	"errors"
	"log/slog"
	"net/http"
)

// AppError is an error that is safe to show to API clients.
type AppError struct {
	Status  int
	Code    string
	Message string
}

func (e *AppError) Error() string { return e.Code + ": " + e.Message }

// NewError creates an AppError. Compare with errors.Is against the same pointer.
func NewError(status int, code, msg string) *AppError {
	return &AppError{Status: status, Code: code, Message: msg}
}

var (
	ErrInvalidRequest  = NewError(http.StatusBadRequest, "invalid_request", "Dữ liệu gửi lên không hợp lệ")
	ErrUnauthorized    = NewError(http.StatusUnauthorized, "unauthorized", "Bạn cần đăng nhập")
	ErrForbidden       = NewError(http.StatusForbidden, "forbidden", "Bạn không có quyền thực hiện thao tác này")
	ErrNotFound        = NewError(http.StatusNotFound, "not_found", "Không tìm thấy")
	ErrTooManyRequests = NewError(http.StatusTooManyRequests, "too_many_requests", "Bạn thao tác quá nhanh, vui lòng thử lại sau")
	ErrInternal        = NewError(http.StatusInternalServerError, "internal_error", "Có lỗi xảy ra, vui lòng thử lại")
)

type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes err as the standard JSON error. Errors that are not an
// *AppError are logged with the request id and reported as ErrInternal.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *AppError
	if !errors.As(err, &appErr) {
		slog.Error("unhandled error",
			"err", err,
			"request_id", RequestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path)
		appErr = ErrInternal
	}
	WriteJSON(w, appErr.Status, errorBody{Error: errorPayload{Code: appErr.Code, Message: appErr.Message}})
}

// NotFound writes ErrNotFound. Register it as the mux's catch-all "/" route
// so unknown paths (and unsupported methods on known paths) get JSON too.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, ErrNotFound)
}
