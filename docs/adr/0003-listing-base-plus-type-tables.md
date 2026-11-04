# Listing và Booking: bảng chung + bảng chi tiết riêng cho từng Service Type

Sàn sẽ thêm loại dịch vụ mới, nên cần một lớp chung (`listings`, `bookings`, `booking_items`) để duyệt, tìm kiếm, thanh toán, sổ cái và đánh giá hoạt động như nhau với mọi loại. Phần riêng (loại vé, chuyến, loại xe bao, mẫu xe thuê) nằm ở bảng riêng có khóa ngoại và CHECK constraint. Không dùng một cột JSON chung cho mọi loại, vì giá và sức chứa cần được DB ràng buộc chặt. Thêm Service Type mới là thêm một dòng `service_types` và các bảng chi tiết của nó.
