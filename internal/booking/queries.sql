-- name: GetBooking :one
SELECT * FROM bookings WHERE id = @id;
