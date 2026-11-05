-- name: GetSettings :one
SELECT * FROM platform_settings WHERE id = 1;

-- name: GetSettingsForUpdate :one
SELECT * FROM platform_settings WHERE id = 1 FOR UPDATE;

-- name: UpdateSettings :exec
UPDATE platform_settings SET
    hold_minutes                   = @hold_minutes,
    hold_grace_minutes             = @hold_grace_minutes,
    request_response_hours         = @request_response_hours,
    request_payment_hours          = @request_payment_hours,
    charter_late_confirm_min_hours = @charter_late_confirm_min_hours,
    ride_checkin_window_minutes    = @ride_checkin_window_minutes,
    ride_no_show_after_minutes     = @ride_no_show_after_minutes,
    charter_auto_complete_hours    = @charter_auto_complete_hours,
    release_delay_days             = @release_delay_days,
    min_payout_amount              = @min_payout_amount,
    reschedule_window_days         = @reschedule_window_days,
    ride_booking_cutoff_minutes    = @ride_booking_cutoff_minutes,
    updated_by                     = @updated_by,
    updated_at                     = @now
WHERE id = 1;

-- name: ListHolidaysByYear :many
SELECT date, name FROM holidays
WHERE date >= @from_date AND date < @to_date
ORDER BY date;

-- name: InsertHoliday :execrows
INSERT INTO holidays (date, name, created_at, updated_at)
VALUES (@date, @name, @now, @now)
ON CONFLICT (date) DO NOTHING;

-- name: GetHolidayName :one
SELECT name FROM holidays WHERE date = @date;

-- name: DeleteHoliday :execrows
DELETE FROM holidays WHERE date = @date;

-- name: HolidayExists :one
SELECT EXISTS (SELECT 1 FROM holidays WHERE date = @date);
