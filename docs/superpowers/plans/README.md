# Lộ trình triển khai Backend

Spec: [2026-11-04-lunain-travel-backend-design.md](../specs/2026-11-04-lunain-travel-backend-design.md)

Mỗi plan cho ra phần mềm chạy và test được. Làm theo thứ tự, plan sau dùng interface của plan trước. Plan chi tiết chỉ được viết khi plan trước đã xong, để bám theo code thật.

| # | Plan | Spec | Trạng thái |
|---|------|------|-----------|
| 1 | [Nền tảng + Identity](2026-11-04-01-foundation-identity.md) | §3, §4, §5.1, §14 | Đã xong, merge `f35daab` |
| 2 | [Đối tác, loại dịch vụ, hoa hồng, địa giới, file, cấu hình](2026-11-04-02-partner-platform.md) | §5.2, §5.3, §5.4 (địa giới); DB §4, §5.1, §5.2 | Đã xong, merge `c326e4a` |
| 3 | [Listing lớp chung + Vé tham quan](2026-11-04-03-listing-attraction.md) | §5.4, §5.5; DB §5.3, §5.4 | Đã xong, merge `99a154b` |
| 4 | [Booking + PayOS + Sổ cái cho vé tham quan](2026-11-05-04-booking-payment-ledger.md) | §5.9, §6.1, §6.3, §6.4, §7, §8, §9, §10 | Đã xong |
| 5 | Payout + Refund admin + đối soát | §6.3, §9 | Chưa |
| 6 | Scheduled Ride (giữ ghế, khứ hồi qua checkout, tiền về trễ) | §5.6, §7, §8 | Chưa |
| 7 | Self-Drive Rental (Request Booking, giữ xe theo khoảng thời gian) | §5.8, §6.2, §7 | Chưa |
| 8 | Private Charter (báo giá theo km, phụ phí, API bản đồ) | §5.7, §6.2 | Chưa |
| 9 | Review, Complaint, thông báo trong web đầy đủ, khóa đối tác/ẩn Listing, admin sửa đối tác và xem audit, cùng các lỗi Minor còn hoãn từ plan 1 (IPv6 /64, giới hạn đăng nhập theo tài khoản, bắt buộc `APP_ENV`, OIDC lười) | §5.2, §11, §12 | Chưa |
| 10 | Báo cáo admin và đối tác | §13 | Chưa |
