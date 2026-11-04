package catalog

import (
	"net/http"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

var (
	ErrInvalidServiceType   = httpx.NewError(http.StatusUnprocessableEntity, "invalid_service_type", "Loại dịch vụ không hợp lệ")
	ErrInvalidCommission    = httpx.NewError(http.StatusUnprocessableEntity, "invalid_commission_rate", "Tỷ lệ hoa hồng hoặc thời điểm hiệu lực không hợp lệ")
	ErrCommissionRateExists = httpx.NewError(http.StatusConflict, "commission_rate_exists", "Đã có tỷ lệ hoa hồng với thời điểm hiệu lực này")
	ErrNoCommissionRate     = httpx.NewError(http.StatusUnprocessableEntity, "no_commission_rate", "Loại dịch vụ này chưa được cấu hình hoa hồng")
)
