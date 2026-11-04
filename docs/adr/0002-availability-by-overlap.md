# Số xe thuê trống tính theo khoảng thời gian giao nhau, không dùng bộ đếm

Cửa hàng chỉ khai báo số lượng theo mẫu xe, còn khách đặt trước theo khoảng [giờ nhận, giờ trả). Một bộ đếm "trừ khi thuê, cộng khi trả" không đúng khi có nhiều đơn ở các khoảng thời gian khác nhau. Vì vậy số xe trống = `quantity` trừ số Booking còn hiệu lực có `tstzrange` giao nhau, tính trong transaction có khóa dòng mẫu xe. Xe tự "trống lại" khi Booking kết thúc. Riêng Scheduled Ride vẫn dùng bộ đếm `seats_reserved`, vì mỗi Trip là một thời điểm cố định.
