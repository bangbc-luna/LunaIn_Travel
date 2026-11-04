# LunaIn Travel — Thiết kế Backend & Database (giai đoạn 1)

- Ngày: 2026-11-04
- Phạm vi: Backend (Go) + Database (PostgreSQL). Frontend (Next.js) đang thiết kế, nằm ngoài tài liệu này.
- Thuật ngữ: theo [GLOSSARY.md](../../../GLOSSARY.md). Tài liệu dùng tên tiếng Anh trong glossary cho khái niệm, tiếng Việt cho diễn giải.

Ký hiệu: **[Chốt]** là điều chủ dự án đã quyết. **[Builder]** là quyết định kỹ thuật do người viết đề xuất, có thể đổi nếu có lý do tốt.

---

## 1. Mục đích và lý do

LunaIn Travel là sàn trung gian du lịch. Đối tác (khu du lịch, nhà xe, cửa hàng thuê xe) tự đăng dịch vụ. Khách tìm, đặt và thanh toán online cho LunaIn. LunaIn giữ hoa hồng, cộng phần còn lại vào số dư của đối tác và chuyển khoản khi đối tác yêu cầu rút.

Giai đoạn 1 mở hai mảng: vé tham quan khu du lịch và xe (đặt xe có tài xế, thuê xe tự lái). Sau này sẽ thêm loại dịch vụ khác, nên thiết kế không được khóa cứng vào bốn loại hiện tại.

Vì LunaIn **giữ tiền của khách** rồi mới trả cho đối tác, tính đúng tiền là yêu cầu số một. Mọi đồng tiền vào và ra phải truy vết được tới từng Booking.

## 2. Mục tiêu, không làm, và các thất bại không chấp nhận được

### Mục tiêu
1. Khách đăng ký, tìm và đặt được cả bốn Service Type, thanh toán qua PayOS và nhận mã QR.
2. Đối tác quản lý hồ sơ, Listing, giá, chính sách hủy, xác nhận Request Booking, check-in bằng QR, xem doanh thu và rút tiền.
3. Super Admin quản trị toàn sàn: tạo đối tác, bật Service Type, duyệt Listing, đặt Commission Rate, duyệt Payout, chuyển khoản các Refund phát sinh từ hủy đơn, theo dõi khiếu nại, khóa đối tác, ẩn Listing, hủy đơn khi có sự cố, xem báo cáo.
4. Mọi con số tiền (Commission, Refund, Partner Balance) đều đúng và đối soát được.

### Không làm ở giai đoạn 1
- Frontend.
- Phân quyền nhân viên trong đối tác. **[Chốt]** Mỗi đối tác dùng một tài khoản. DB vẫn chừa chỗ để thêm sau (mục 5.2).
- Sơ đồ ghế, chọn ghế cụ thể. **[Chốt]**
- Giá theo ngày hoặc theo mùa. **[Chốt]** Mỗi Ticket Type chỉ có một giá.
- Trang nội dung điểm đến do LunaIn biên tập. **[Chốt]** Nội dung do đối tác viết.
- Thanh toán trả tại cửa hàng. **[Chốt]** Mọi dịch vụ đều trả online qua LunaIn.
- Tự động chuyển tiền cho đối tác hoặc tự động hoàn tiền cho khách. Cả hai do người chuyển khoản tay, hệ thống ghi nhận.
- Kênh Zalo/SMS. **[Chốt]** Chỉ email và thông báo trong web.
- Đăng nhập bằng số điện thoại/OTP, Facebook.
- Tách microservice.
- Hóa đơn VAT điện tử. **[Chốt]** Chưa định nghĩa ở giai đoạn 1. **[Builder]** Booking vẫn lưu đủ chi tiết giá (từng dòng, phụ phí, hoa hồng) để sau này thêm được mà không phải sửa dữ liệu cũ.
- Hoàn tiền ngoài chính sách hủy. **[Chốt]** Không có. Dịch vụ đã dùng xong mà khách không hài lòng thì khách đánh giá hoặc khiếu nại. Chuyện có hoàn tiền hay không do đối tác tự giải quyết với khách, ngoài sàn.
- Đối tác hủy Booking đã xác nhận. **[Chốt]** Đối tác không có quyền này.

### Thất bại không chấp nhận được (dù hệ thống "chạy được")
- Bán quá số ghế của một Trip, hoặc cho thuê quá số xe của một Vehicle Model trong cùng khoảng thời gian.
- Một khoản thanh toán bị ghi nhận hai lần, hoặc tiền đã vào mà Booking không được xác nhận.
- Partner Balance lệch với tổng các bút toán, hoặc không giải thích được một con số.
- Đối tác thấy hoặc sửa được dữ liệu của đối tác khác.
- Đổi Commission Rate hay giá làm thay đổi số tiền của Booking cũ.

## 3. Ràng buộc

- **[Chốt]** Backend Go, DB PostgreSQL.
- **[Chốt]** Một người làm backend, ưu tiên làm kỹ từng phần hơn làm nhanh.
- **[Chốt]** Khoảng 10 đối tác lúc ra mắt. Lượng đơn chưa dự báo được, nên hệ thống phải chịu được đột biến mà không bán vượt và không sập.
- **[Chốt]** Cổng thanh toán PayOS (VietQR + webhook).
- Tiền tệ VND. **[Builder]** Lưu dạng `bigint` (đồng), không dùng số thực.
- Múi giờ nghiệp vụ `Asia/Ho_Chi_Minh`. **[Builder]** Lưu `timestamptz`. Các mốc như "ngày tham quan" lưu `date` kèm khung giờ, quy đổi theo giờ Việt Nam.

## 4. Kiến trúc tổng thể

**[Builder]** Modular monolith: một binary Go, một DB Postgres, chia module theo nghiệp vụ. Một người vận hành được, transaction tiền và giữ chỗ nằm gọn trong một DB, sau này tách module ra service khi thật sự cần. Xem [ADR 0001](../../adr/0001-modular-monolith.md).

Các module và trách nhiệm:

| Module | Trách nhiệm |
|---|---|
| `identity` | Tài khoản, đăng ký/đăng nhập, xác minh email, Google, phiên đăng nhập, vai trò |
| `partner` | Hồ sơ đối tác, Service Type được bật, khóa/mở |
| `catalog` | Listing và chi tiết theo Service Type, duyệt, ẩn, tìm kiếm, danh mục địa giới |
| `booking` | Tạo Booking, tính giá, giữ chỗ, máy trạng thái, xác nhận yêu cầu, check-in, hủy, đổi lịch |
| `payment` | Tạo link PayOS, webhook, ghi nhận giao dịch, Refund |
| `ledger` | Bút toán và số dư đối tác, Commission, Payout |
| `review` | Review và Complaint |
| `notification` | Email và thông báo trong web |
| `reporting` | Báo cáo cho Super Admin và đối tác (đọc, không ghi) |

Quy tắc giữa các module **[Builder]**:
- Module chỉ gọi nhau qua interface Go của module đó, không đọc thẳng bảng của module khác. Riêng `reporting` được đọc nhiều bảng.
- Việc phát sinh từ một thay đổi (gửi email, chuông) đi qua bảng **outbox** ghi cùng transaction với thay đổi đó. Worker nền xử lý outbox.
- Việc chạy theo lịch (hết hạn Hold, chuyển No-show, chuyển tiền từ "chờ" sang "khả dụng") chạy bằng job nền dùng hàng đợi trên Postgres (ví dụ thư viện River). Giai đoạn 1 không dùng Redis hay message broker.

API **[Builder]**: REST/JSON, tiền tố `/v1`. Chia ba nhóm `customer`, `partner`, `admin`, kèm các endpoint public để xem và tìm kiếm. Tài liệu bằng OpenAPI.

## 5. Mô hình dữ liệu

Dưới đây là các thực thể, quan hệ và ràng buộc quan trọng. Tên cột chi tiết, index phụ và kiểu dữ liệu nhỏ do builder quyết khi viết migration.

Quy ước chung **[Builder]**: khóa chính `uuid` (v7). Có `created_at` và `updated_at`. Tiền là `bigint` VND. Không xóa cứng dữ liệu nghiệp vụ, chỉ đổi trạng thái.

### 5.1 Tài khoản và định danh

- `users`: `email` (unique, không phân biệt hoa thường), `password_hash` (nullable nếu chỉ dùng Google), `email_verified_at`, `role` ∈ {`customer`, `partner`, `super_admin`}, `status` ∈ {`active`, `locked`}.
- `user_identities`: liên kết OAuth (`provider`=`google`, `provider_subject`). Unique (`provider`, `provider_subject`).
- `email_verifications`: mã 6 số đã hash, hạn dùng, số lần nhập sai, dùng cho đăng ký và quên mật khẩu.
- `sessions` / `refresh_tokens`: refresh token lưu dạng hash, xoay vòng mỗi lần dùng, thu hồi được.
- `customer_profiles`: họ tên, số điện thoại, ngày sinh, ... **[Chốt]** Khách tự cập nhật, hoặc hệ thống tự điền từ thông tin liên hệ ở Booking đầu tiên nếu hồ sơ còn trống.

**[Chốt]** Đăng ký bằng email + mật khẩu, nhập mã xác minh gửi qua email mới hoàn tất. Có đăng nhập Google. **[Chốt]** Phải có tài khoản đã xác minh mới đặt được.

**[Builder]** Tài khoản Google trùng email với tài khoản email/mật khẩu thì liên kết vào cùng `users`, vì Google đã xác minh email. Tài khoản đối tác và Super Admin không đăng ký công khai mà do Super Admin tạo.

### 5.2 Đối tác

- `partners`: tên doanh nghiệp, mã số thuế, giấy phép kinh doanh (file), địa chỉ, liên hệ, `status` ∈ {`active`, `suspended`}.
- `partner_bank_accounts`: ngân hàng, số tài khoản, chủ tài khoản. Lưu lịch sử. Payout ghi lại tài khoản tại thời điểm yêu cầu.
- `partner_members`: (`partner_id`, `user_id`, `role`). Giai đoạn 1 mỗi đối tác chỉ có một dòng `owner`. Bảng này để thêm nhân viên sau mà không phải đổi schema.
- `partner_service_types`: Service Type mà Super Admin đã bật cho đối tác. **[Chốt]** Đối tác chỉ tạo được Listing thuộc các loại đã bật. Một đối tác có thể có nhiều loại.

**[Chốt]** LunaIn tạo tài khoản đối tác sau khi ký hợp đồng. Đối tác tự cập nhật giấy phép, tài khoản ngân hàng, ...

Khóa đối tác **[Chốt]**: mọi Listing của họ ẩn khỏi khách và không nhận Booking mới (kể cả Request Booking mới). Đối tác **vẫn đăng nhập được** để phục vụ nốt các Booking đã có: quét QR, chấp nhận hoặc từ chối yêu cầu đang chờ, đánh dấu hoàn thành. **[Builder]** Khi bị khóa, đối tác không tạo hoặc sửa được Listing và không tạo được Payout mới. Payout đang mở bị treo cho tới khi Super Admin mở khóa hoặc tự xử lý. Vì không còn nhận đơn mới nên `partners.status` dùng giá trị `suspended` để phân biệt với việc khóa đăng nhập (`users.status=locked`).

### 5.3 Service Type và hoa hồng

- `service_types`: bảng danh mục (`code`: `attraction_ticket`, `scheduled_ride`, `private_charter`, `self_drive_rental`). Mỗi loại có `booking_mode` ∈ {`instant`, `request`}. Thêm Service Type mới là thêm một dòng ở đây cùng bảng chi tiết tương ứng.
- `commission_rates`: (`service_type`, `rate_bps`, `effective_from`, `created_by`). **[Chốt]** Super Admin đặt % theo từng Service Type. **[Builder]** Không sửa dòng cũ, mỗi lần đổi thêm một dòng mới để có lịch sử. Tỷ lệ lưu bằng basis points (10% = 1000) để tránh số thực.

**[Chốt]** Booking lưu lại `commission_rate_bps` tại thời điểm tạo, đổi tỷ lệ sau đó không ảnh hưởng Booking cũ.

### 5.4 Listing (lớp chung)

`listings`: `partner_id`, `service_type`, `title`, `description`, `cover_image`, vị trí (`province_code`, `ward_code`, địa chỉ, toạ độ), `review_status`, `visibility`, điểm đánh giá tổng hợp (`rating_avg`, `rating_count`).

- `review_status` ∈ {`draft`, `pending_review`, `approved`, `rejected`} và `rejection_reason`.
  **[Chốt]** Chỉ duyệt lần đầu. Đã `approved` thì đối tác sửa trực tiếp, không quay lại hàng chờ duyệt.
- `visibility` ∈ {`published`, `unpublished_by_partner`, `hidden_by_admin`} và `hidden_reason`.
  **[Chốt]** Super Admin ẩn được Listing đã duyệt, ví dụ khi bị khiếu nại nhiều sau khi đối tác sửa. Đối tác không tự gỡ được trạng thái `hidden_by_admin`.
- Khách chỉ thấy Listing có `review_status=approved`, `visibility=published` và đối tác đang `active`.
- `listing_images`: nhiều ảnh, có thứ tự.
- `listing_audit_log` **[Builder]**: ghi lại mỗi lần đối tác sửa (ai, lúc nào, trường nào, giá trị cũ/mới). Vì sửa không cần duyệt lại, đây là căn cứ để admin xử lý khiếu nại.
- `cancellation_policies` + `cancellation_policy_tiers`: một Listing có một chính sách, gồm nhiều mốc (`hours_before`, `refund_percent`). **[Chốt]** Đối tác tự đặt, hệ thống tự tính tiền hoàn. **[Builder]** Không có mốc nào khớp thì hoàn 0%.

Danh mục địa giới **[Builder]**: bảng `provinces` và `wards` theo đơn vị hành chính sau sáp nhập 2025 (34 tỉnh/thành, bỏ cấp huyện), import từ dữ liệu chính thức. Dùng cho tìm kiếm và lọc theo khu vực.

Chi tiết theo từng Service Type dùng bảng riêng, liên kết 1-1 hoặc 1-n với `listings` (mục 5.5–5.8). **[Builder]** Không dùng một bảng chung có cột JSON cho mọi loại, vì cần ràng buộc DB chặt cho giá và sức chứa. Xem [ADR 0003](../../adr/0003-listing-base-plus-type-tables.md).

### 5.5 Attraction Ticket

- `attractions` (1-1 với listing): giờ mở cửa, hướng dẫn, ...
- `attraction_ticket_types`: `name`, `description`, `audience` ∈ {`child`, `adult`}, `price`, `active`. **[Chốt]** Đối tác tự định nghĩa loại vé và giá. Ví dụ thường gặp là trẻ em/người lớn × thường/full, nhưng không cố định.
- `attraction_time_slots`: khung giờ hoạt động (`start_time`, `end_time`, các ngày trong tuần áp dụng). **[Chốt]** Đối tác khai báo khi đăng.
- `attraction_closed_dates` **[Builder]**: các ngày nghỉ, khách không chọn được.

**[Chốt]** Không giới hạn số lượng vé mỗi ngày, nên không cần giữ chỗ.

### 5.6 Scheduled Ride

- `ride_routes`: điểm đi, điểm đến (mỗi điểm có tỉnh/phường và địa chỉ hoặc tên bến), thời gian chạy dự kiến, loại xe.
- `ride_trips`: một chuyến (`route_id`, `departure_at`, `seat_capacity`, `seats_reserved`, `price_per_seat`, `status`). **[Builder]** Đối tác tạo chuyến theo lịch lặp (ví dụ mỗi ngày 7h, 13h) và hệ thống sinh sẵn các Trip cho N ngày tới. Đối tác vẫn sửa hoặc hủy được từng chuyến.
- **[Chốt]** Khách chọn số ghế, nhà xe tự xếp chỗ. Không lưu sơ đồ ghế.
- **[Chốt]** Có khứ hồi. **[Builder]** Khứ hồi là **hai Booking riêng** (chiều đi, chiều về) thuộc cùng một `checkout`, thanh toán chung một lần. Mỗi chiều có QR, trạng thái, check-in, No-show và chính sách hủy riêng, tính theo giờ khởi hành của chiều đó. Khách hủy được từng chiều. Như vậy máy trạng thái Booking không phải xử lý trường hợp "một nửa đơn".

Ràng buộc: `CHECK (seats_reserved BETWEEN 0 AND seat_capacity)`. Cách giữ ghế xem mục 7.

### 5.7 Private Charter

- `charter_services` (1-1 với listing): khu vực phục vụ (danh sách tỉnh, số km tối đa).
- `charter_vehicle_classes`: loại xe (4/7/16 chỗ, ...), `base_fare` (giá mở cửa), `price_per_km`.
- `charter_surcharges`: phụ phí của từng loại xe. **[Chốt]** Có các loại phụ phí đối tác tự bật, mặc định 0đ. **[Builder]** Giai đoạn 1 có các loại mà hệ thống tự tính được lúc báo giá:
  - `night`: cộng cố định khi giờ đón nằm trong khung giờ đêm đối tác đặt.
  - `weekend` / `holiday`: cộng theo % hoặc cố định khi ngày đón rơi vào cuối tuần hoặc ngày lễ (danh sách ngày lễ do Super Admin quản lý).
  - `round_trip_waiting`: cộng cố định khi khách chọn khứ hồi.

  Phí phát sinh không biết trước (chờ thêm giờ, cầu đường ngoài dự kiến) không tính trên sàn. Đối tác ghi vào mô tả dịch vụ.

Công thức **[Chốt]**: `giá = base_fare + distance_km × price_per_km + Σ phụ phí áp dụng`. Khứ hồi tính hai chiều. **[Builder]** Quãng đường lấy từ API bản đồ (Goong hoặc Google Distance Matrix) phía server, làm tròn lên 0,1 km. Báo giá lưu kèm `distance_km` và chi tiết từng thành phần vào Booking.

### 5.8 Self-Drive Rental

- `rental_stores` (1-1 với listing): cửa hàng, khu vực, địa chỉ nhận/trả, giờ giao nhận (ví dụ 06:00–23:59), có giao xe tận nơi hay không, Rental Terms (cọc, giấy tờ, phí trả trễ, khu vực được đi). **[Chốt]** Rental Terms chỉ để hiển thị, khách tự xử lý với cửa hàng.
- `rental_vehicle_models`: loại (ô tô/xe máy), tên mẫu, đời xe, số chỗ, đồ đi kèm, nhãn ("không cần cọc", ...), `price_per_day`, `quantity`.
- `rental_pickup_points` **[Builder]**: các điểm nhận xe của cửa hàng, có thể kèm phí giao tận nơi.

**[Chốt]** Khai báo số lượng theo mẫu xe, không quản lý từng chiếc.
Số ngày thuê **[Chốt từ ảnh mẫu]**: tính theo khối 24 giờ kể từ giờ nhận, tối thiểu 1 ngày. **[Builder]** `days = ceil((return_at − pickup_at) / 24h)`. Quy tắc tính thêm khi trả trễ do cửa hàng thu tại chỗ, nằm ngoài sàn.

Số xe trống **[Builder]**: tính theo khoảng thời gian, không dùng bộ đếm. Với một mẫu xe, số xe trống trong khoảng [nhận, trả) = `quantity` − số Booking đang giữ xe có khoảng thời gian giao với khoảng đó. Booking `completed`, `cancelled`, `expired`, `rejected` không giữ xe. Nhờ vậy xe "tự cộng lại" khi khách trả. Xem [ADR 0002](../../adr/0002-availability-by-overlap.md).

### 5.9 Booking

`checkouts` **[Builder]**: một lần khách thanh toán, gồm một hoặc nhiều Booking (hiện chỉ có khứ hồi Scheduled Ride là nhiều hơn một). Thanh toán gắn với `checkout`, còn tiền, hoa hồng, hủy và sổ cái tính theo từng Booking. `checkouts.total_amount` = tổng `gross_amount` của các Booking trong nó.

`bookings` là bảng chung cho mọi Service Type:
- `checkout_id`.
- `code`: mã ngắn dễ đọc, unique, dùng để tra cứu.
- `customer_id`, `partner_id`, `listing_id`, `service_type`, `booking_mode`.
- `status` (mục 6).
- `service_start_at`, `service_end_at`: thời điểm bắt đầu và kết thúc sử dụng. Dùng để áp chính sách hủy, xác định No-show và lúc tiền chuyển sang khả dụng.
- Tiền chụp lại tại thời điểm đặt: `gross_amount`, `commission_rate_bps`, `commission_amount`.
- `cancellation_policy_snapshot` (JSON): **[Builder]** chụp lại các mốc tại thời điểm đặt, để đối tác đổi chính sách sau này không ảnh hưởng đơn cũ.
- Thông tin liên hệ khách nhập lúc đặt (tên, SĐT, email).
- `hold_expires_at`, `payment_due_at`, `confirmed_at`, `checked_in_at`, `completed_at`, `cancelled_at`, `cancel_reason`, `cancelled_by`.
- `reschedule_count` (0 hoặc 1) **[Chốt]**, `original_service_start_at` (giờ sử dụng ban đầu, dùng để tính % hoàn sau khi đã đổi lịch).
- `qr_token`: token ngẫu nhiên đủ dài, chỉ server tra được. Mã QR hiển thị token này.

`booking_items`: các dòng của Booking, mỗi dòng có `unit_price` chụp lại, `quantity`, `line_total` và tham chiếu chi tiết theo loại:
- Attraction: `ticket_type_id`, `visit_date`, `time_slot_id`.
- Scheduled Ride: `trip_id`, `seats`, `direction` (đi/về).
- Private Charter: `vehicle_class_id`, điểm đón/trả, `pickup_at`, `distance_km`, chi tiết phụ phí, `is_round_trip`.
- Self-Drive Rental: `vehicle_model_id`, `pickup_at`, `return_at`, `pickup_point_id`, `days`.

**[Builder]** Mỗi loại có bảng chi tiết riêng (`booking_attraction_items`, ...) để giữ ràng buộc chặt. `booking_items` chỉ chứa phần chung về giá.

`booking_status_history`: lưu mọi lần chuyển trạng thái (từ, đến, ai, lúc nào, lý do). Dùng để đối soát và xử lý khiếu nại.

## 6. Vòng đời Booking

### 6.1 Instant Booking (Attraction Ticket, Scheduled Ride)

```
pending_payment ──thanh toán──▶ confirmed ──check-in──▶ checked_in ──▶ completed
      │                            │  │
      │ quá hạn Hold               │  └─ qua service_end_at, chưa check-in ─▶ no_show
      ▼                            │
   expired                         └─ khách hủy ─▶ cancelled (kèm Refund theo chính sách)
```

- `expired` không hẳn là trạng thái cuối: tiền về trễ có thể đưa Booking `expired → confirmed` (mục 8). Áp dụng cho cả Request Booking.
- **[Builder]** Hold 15 phút (kèm 5 phút đệm, mục 7), Super Admin chỉnh được. Attraction không giới hạn chỗ nên Hold chỉ là hạn thanh toán. Scheduled Ride thì Hold giữ ghế thật.
- Attraction: check-in xong thì `completed` luôn. **[Chốt]** Qua ngày tham quan mà chưa check-in thì `no_show`, không hoàn tiền, đối tác được nhận tiền như đơn hoàn thành.
- Scheduled Ride: check-in khi khách lên xe. **[Builder]** Qua giờ khởi hành + X giờ mà chưa check-in cũng là `no_show`, xử lý tiền giống Attraction. Khứ hồi là hai Booking nên mỗi chiều tự đi hết vòng đời của nó.
- **[Chốt]** Đối tác không hủy được Booking đã xác nhận. **[Builder]** Vì vậy đối tác không hủy hoặc giảm sức chứa được một Trip đã có Booking `confirmed` (giảm `seat_capacity` không được thấp hơn `seats_reserved`). Gặp sự cố thật (xe hỏng, thiên tai), đối tác liên hệ LunaIn và **Super Admin** hủy giúp, áp ngoại lệ "lỗi không ở phía khách" (mục 6.3).

### 6.2 Request Booking (Private Charter, Self-Drive Rental)

```
requested ──đối tác chấp nhận──▶ awaiting_payment ──thanh toán──▶ confirmed ──▶ in_service ──▶ completed
    │                                  │                             │
    │ đối tác từ chối / quá hạn phản hồi │ quá payment_due_at           └─ khách hủy ─▶ cancelled (+Refund)
    ▼                                  ▼
 rejected / expired                 expired
```

- **[Chốt]** Đối tác xác nhận trước, hệ thống báo khách, rồi khách mới thanh toán.
- **[Chốt]** Đối tác chỉ được từ chối ở bước `requested`. Đã chấp nhận (`awaiting_payment`) thì đối tác **không rút lại được** trên hệ thống. Nếu sau đó xe có vấn đề (hỏng, đã cho người khác thuê), đối tác tự chịu trách nhiệm với khách ngoài sàn: bồi thường, hoặc giao xe cùng phân khúc hoặc cao hơn. Booking vẫn giữ nguyên mẫu xe và giá ban đầu. Khách vẫn có thể không thanh toán để đơn tự hết hạn.
- **[Builder]** Đối tác có 12 giờ để phản hồi (không quá giờ bắt đầu dịch vụ). Khách có 2 giờ để thanh toán sau khi được chấp nhận. Super Admin chỉnh được cả hai.
- **[Builder]** Self-Drive Rental: xe được giữ ngay từ `requested` cho đến khi đơn kết thúc, để đối tác không chấp nhận hai đơn cho cùng một chiếc xe. Private Charter không giữ sức chứa nào, việc nhận chuyến là quyết định của đối tác.
- `in_service`: Rental khi đối tác quét QR lúc giao xe. Charter khi đối tác quét QR lúc đón khách.
- `completed`: Rental khi đối tác bấm "đã nhận lại xe". Charter khi đối tác bấm "hoàn thành chuyến", hoặc **[Builder]** hệ thống tự hoàn thành sau `service_end_at` + 24 giờ nếu đối tác quên bấm.
- Khách không đến nhận xe hoặc không đến điểm đón: đối tác đánh dấu `no_show`, không hoàn tiền.

### 6.3 Hủy và hoàn tiền

**[Chốt]** Áp `cancellation_policy_snapshot` theo số giờ từ lúc hủy tới giờ sử dụng. Ví dụ: trước 48 giờ hoàn 100%, trước 24 giờ hoàn 50%, sau đó 0%. **[Chốt]** Booking đã đổi lịch vẫn được hủy, nhưng % hoàn tính theo **giờ sử dụng ban đầu** (`original_service_start_at`), không theo ngày mới. Nếu giờ ban đầu đã qua thì hoàn 0%. Cách này chặn việc đổi lịch ra xa rồi hủy để lấy 100%.

Chỉ **khách** (hủy đơn của mình) và **Super Admin** (sự cố thật) được hủy Booking đã xác nhận. Đối tác không được hủy.

**[Chốt]** Công thức tiền:
```
refund_amount     = gross_amount × refund_percent
commission_amount = gross_amount × commission_rate      (không đổi khi hoàn)
partner_earning   = gross_amount − commission_amount − refund_amount   (có thể âm)
```
Ví dụ: vé 1.000.000đ, hoa hồng 10%, hoàn 50% → khách nhận lại 500.000đ, LunaIn giữ 100.000đ, đối tác nhận 400.000đ. Nếu hoàn 100% → đối tác −100.000đ, trừ vào số dư của họ.

Ngoại lệ **[Builder]**: khi Super Admin hủy vì sự cố không do khách (xe hỏng, khu du lịch đóng cửa đột xuất, ...), hoàn 100% và miễn hoa hồng. Khi đó đối tác nhận 0, không bị âm. Super Admin có thể chọn thay vào đó áp chính sách hủy thông thường nếu sự cố do phía đối tác và LunaIn vẫn muốn giữ hoa hồng. Lựa chọn và lý do được ghi audit.

Quy trình Refund **[Builder]**: PayOS chỉ thu tiền vào, nên mỗi lần hủy có hoàn tiền sẽ tạo một bản ghi `refunds` với `status` ∈ {`awaiting_bank_info`, `pending`, `completed`}.
- Khách tự hủy: khách nhập tài khoản ngân hàng nhận hoàn ngay ở bước hủy → `pending`.
- Hủy không do khách khởi tạo (Super Admin hủy, thanh toán trễ): bắt đầu ở `awaiting_bank_info`, hệ thống gửi email và chuông yêu cầu khách nhập tài khoản → `pending`.
- Khách có thể lưu sẵn tài khoản nhận hoàn trong hồ sơ để điền tự động.
- Super Admin chuyển khoản tay rồi đánh dấu `completed`, đính kèm mã giao dịch.

Trạng thái Booking và bút toán sổ cái được ghi **ngay khi hủy**, không chờ tiền chuyển xong.

### 6.4 Đổi lịch (chỉ Attraction Ticket)

**[Chốt]**: mỗi Booking đổi tối đa 1 lần. Khách phải thao tác đổi trong 14 ngày kể từ lúc thanh toán. Ngày mới chọn tự do.
**[Builder]**: chỉ đổi được khi Booking đang `confirmed` (chưa check-in, chưa qua ngày tham quan cũ). Giá giữ nguyên theo Booking, không thu thêm hay hoàn lại chênh lệch. Đổi lịch không tính là hủy. `service_start_at` cập nhật theo ngày mới nên mốc check-in và No-show tính theo ngày mới. **[Chốt]** % hoàn khi hủy vẫn tính theo `original_service_start_at` (mục 6.3).

## 7. Giữ chỗ và chống bán vượt

Yêu cầu: không bao giờ bán quá sức chứa, kể cả khi nhiều khách đặt cùng lúc.

- **Scheduled Ride** **[Builder]**: trong một transaction, chạy `UPDATE ride_trips SET seats_reserved = seats_reserved + :n WHERE id = :trip AND seats_reserved + :n <= seat_capacity` rồi kiểm tra số dòng bị ảnh hưởng. Khi Booking `expired` hoặc `cancelled` thì trừ lại trong cùng transaction chuyển trạng thái. Có `CHECK` constraint làm lưới an toàn cuối.
- **Self-Drive Rental** **[Builder]**: trong một transaction, `SELECT ... FOR UPDATE` dòng `rental_vehicle_models` để khóa theo mẫu xe, đếm số Booking đang giữ xe có khoảng thời gian giao nhau (dùng `tstzrange` và toán tử `&&`, có index GiST), so với `quantity` rồi mới tạo Booking. Đối tác giảm `quantity` xuống dưới số đã giữ thì bị chặn.
- **Attraction / Charter**: không có sức chứa phải giữ.
- **Thời gian đệm** **[Chốt]**: khách thấy hạn thanh toán (15 phút với Instant Booking, 2 giờ với Request Booking), và link PayOS hết hạn đúng lúc đó. Hệ thống giữ chỗ thêm **5 phút đệm** (Super Admin chỉnh được) rồi mới nhả, để chuyển khoản chậm vẫn kịp xác nhận. `hold_expires_at` là mốc khách thấy, mốc nhả chỗ thực tế là `hold_expires_at + grace`.
- **Job hết hạn**: quét Booking quá mốc nhả chỗ thực tế để chuyển `expired` và nhả chỗ. Khi tạo Booking mới, phép đếm cũng tự bỏ qua Booking đã quá hạn nhưng job chưa chạy tới, để không chiếm chỗ oan.

Chịu tải **[Builder]**: rate limit theo IP và theo tài khoản cho các endpoint đăng nhập, gửi mã và tạo Booking. Pool kết nối DB có giới hạn. Timeout cho mọi lời gọi ra ngoài (PayOS, bản đồ, email). Email gửi qua outbox nên không chặn request. Endpoint tạo Booking nhận `Idempotency-Key` để khách bấm hai lần không tạo hai đơn.

## 8. Thanh toán (PayOS)

- Khi một `checkout` cần thanh toán, `payment` tạo một payment link trên PayOS với `orderCode` (số nguyên unique do hệ thống sinh, ánh xạ 1-1 với một lần thanh toán), số tiền bằng `checkouts.total_amount` và hạn bằng hạn Hold hoặc `payment_due_at`. Bảng `payments` lưu `checkout_id`, `provider_order_code`, `amount`, `status` ∈ {`pending`, `paid`, `expired`, `cancelled`} và payload thô.
- **Webhook** **[Builder]**:
  1. Xác minh chữ ký bằng checksum key. Sai chữ ký thì từ chối.
  2. Ghi `payment_events` với unique trên mã sự kiện/giao dịch. Sự kiện trùng thì bỏ qua và vẫn trả 200 (idempotent).
  3. Trong một transaction: khóa `payments` và `bookings`, kiểm tra số tiền khớp, chuyển `payments.paid` và Booking sang `confirmed`, ghi bút toán sổ cái, ghi outbox thông báo.
  4. Gọi API PayOS kiểm tra lại trạng thái trước khi xác nhận, không tin hoàn toàn vào webhook.
- **Tiền đến trong khoảng đệm**: Booking chưa bị nhả chỗ, xác nhận bình thường.
- **Tiền đến sau khi Booking đã `expired`** **[Chốt]**: hệ thống ghi nhận `payments.paid` rồi **thử xác nhận lại tự động** trong một transaction:
  - Điều kiện: dịch vụ chưa bắt đầu, Listing vẫn hiển thị, đối tác chưa bị đình chỉ, và giữ lại được sức chứa theo đúng quy tắc mục 7 (Scheduled Ride còn đủ ghế, Self-Drive Rental còn xe trong khoảng ngày đó). Attraction không giới hạn số lượng nên chỉ cần ngày tham quan chưa qua. **[Builder]** Private Charter không có sức chứa trên hệ thống, nên xác nhận lại nếu tiền về trước giờ đón ít nhất **3 giờ** (Super Admin chỉnh được).
  - Thỏa điều kiện: Booking chuyển `expired → confirmed` (ghi vào lịch sử trạng thái với lý do "thanh toán trễ, xác nhận lại"), giữ lại chỗ, ghi bút toán như xác nhận bình thường và báo cả khách lẫn đối tác. Với Request Booking, đối tác vẫn phải phục vụ vì đã chấp nhận từ trước.
  - Không thỏa: Booking giữ `expired`, tạo Refund 100% ở trạng thái `awaiting_bank_info`, báo khách. **Không ghi bút toán nào cho đối tác**, vì đơn chưa từng được xác nhận. Khoản này chỉ là tiền LunaIn giữ hộ khách, xuất hiện trong báo cáo đối soát.
  - Khứ hồi: cả hai chiều trong cùng `checkout` phải xác nhận lại được thì mới xác nhận. Nếu chỉ một chiều được thì hoàn toàn bộ, để khách không bị kẹt nửa chặng.
- **Số tiền không khớp** (thiếu hoặc thừa): không xác nhận đơn, đánh dấu `payments` cần xử lý tay và báo Super Admin.
- **Đối soát** **[Builder]**: job hằng ngày đối chiếu `payments` với danh sách giao dịch PayOS, lệch thì báo Super Admin.

## 9. Sổ cái, số dư đối tác và rút tiền

Xem [ADR 0004](../../adr/0004-partner-ledger.md).

**[Builder]** Không lưu số dư bằng một con số tự cộng trừ. Mọi thay đổi tiền của đối tác là một dòng bất biến trong `ledger_entries`:

| Bút toán | Khi nào | Số tiền | Ngăn |
|---|---|---|---|
| `booking_earning` | Booking `confirmed` (đã thu tiền) | + (gross − commission) | `pending` |
| `refund_debit` | Booking bị hủy có hoàn tiền | − refund | `pending` |
| `commission_waiver` | Super Admin hủy theo ngoại lệ miễn hoa hồng | + commission | `pending` |
| `release` | Booking kết thúc: `completed`/`no_show` + thời gian chờ, hoặc ngay khi `cancelled` | chuyển toàn bộ phần ròng còn lại của Booking ở `pending` sang `available` (có thể là số âm) | — |
| `payout_hold` | Đối tác tạo yêu cầu rút | − số tiền rút | `available` |
| `payout_reversal` | Super Admin từ chối yêu cầu rút | + số tiền rút | `available` |
| `adjustment` | Super Admin điều chỉnh tay (bắt buộc có lý do) | ± | `available` |

- Mỗi dòng có `partner_id`, `booking_id` hoặc `payout_id`, `bucket` (`pending`/`available`), `amount` có dấu, `created_by`, `reason`. `release` ghi thành hai dòng (− ở `pending`, + ở `available`).
- **Bất biến** **[Builder]**: sau khi một Booking kết thúc (`completed`, `no_show`, `cancelled`), tổng bút toán `pending` của Booking đó bằng 0. Mỗi Booking `release` đúng một lần (unique theo `booking_id` + loại). Test phải kiểm được bất biến này.
- Ví dụ khép vòng: vé 1.000.000đ, hoa hồng 10%, khách hủy hoàn 50% → `booking_earning` +900.000 (pending), `refund_debit` −500.000 (pending), `release` 400.000 sang available. Super Admin hủy miễn hoa hồng → +900.000, `refund_debit` −1.000.000, `commission_waiver` +100.000, `release` 0.
- Sổ hoa hồng của LunaIn: `platform_commission_entries` ghi + commission khi Booking `confirmed` và − commission khi có `commission_waiver`. Doanh thu sàn = tổng sổ này.
- `partner_balances` (`pending`, `available`) là bản cache, cập nhật trong cùng transaction với bút toán và khóa dòng của đối tác. Job đối soát định kỳ so cache với tổng bút toán, lệch thì cảnh báo.
- **Thời gian chờ** **[Builder]**: tiền chuyển sang `available` sau khi Booking `completed`/`no_show` + N ngày (mặc định 1 ngày, Super Admin chỉnh được), để còn thời gian cho khiếu nại.
- **Số dư âm**: `available` được phép âm. Bút toán mới tự bù vào. Đối tác chỉ rút được khi `available > 0` và chỉ tới mức đó.

Payout:
- `payouts`: `partner_id`, `amount`, tài khoản ngân hàng chụp lại, `status` ∈ {`requested`, `approved`, `paid`, `rejected`}, `transfer_reference`, `proof_file`, `processed_by`.
- **[Chốt]** Đối tác chủ động yêu cầu. LunaIn thống kê, đối soát rồi chuyển khoản.
- **[Builder]** Tạo yêu cầu thì trừ ngay `available` (`payout_hold`) để không rút hai lần. Super Admin `approved` → chuyển khoản → `paid` kèm mã giao dịch và chứng từ. `rejected` thì hoàn lại số dư. Mỗi lúc chỉ có một yêu cầu đang mở. Có số tiền rút tối thiểu do Super Admin đặt.
- **Bảng kê** **[Builder]**: số tiền rút là số bất kỳ ≤ `available`, không phân bổ tiền theo từng Booking. Mỗi Payout kèm **sao kê** liệt kê mọi bút toán `available` phát sinh từ Payout trước đến lúc tạo Payout này (release theo Booking, điều chỉnh, ...), kèm số dư đầu kỳ và cuối kỳ, để đối tác và admin đối chiếu. Không cần bảng nối giữa bút toán và Payout.

## 10. Check-in bằng QR

- QR của Booking chứa `qr_token`, không chứa dữ liệu nhạy cảm.
- Đối tác quét trên web đối tác → `POST /v1/partner/check-ins` với token. Server kiểm tra token thuộc đúng đối tác đang đăng nhập, Booking đang ở trạng thái cho phép và đúng ngày sử dụng. **[Builder]** Attraction cho check-in trong ngày tham quan, Scheduled Ride trong khoảng ±X giờ quanh giờ khởi hành.
- Quét lại một mã đã check-in thì trả về "đã check-in lúc …", không lỗi và không đổi dữ liệu.
- Đối tác có thể nhập tay mã Booking nếu không quét được.

## 11. Review và Complaint

- **[Chốt]** Chỉ Booking `completed` mới được đánh giá, mỗi Booking tối đa một Review. Khách đánh giá từ màn lịch sử đơn.
- `reviews`: `booking_id` (unique), `listing_id`, `partner_id`, `rating` 1–5, `comment`, ảnh, `status` ∈ {`visible`, `hidden_by_admin`}. **[Builder]** Booking `no_show` không đánh giá được. Đối tác được trả lời một lần cho mỗi Review.
- `listings.rating_avg`/`rating_count` và chỉ số tương ứng của đối tác được cập nhật khi Review được tạo hoặc bị ẩn.
- `complaints`: khách gửi về một Booking của mình (**[Builder]** chỉ Booking đã thanh toán), có loại, nội dung, ảnh, `status` ∈ {`open`, `in_progress`, `resolved`, `rejected`}, ghi chú xử lý. **[Chốt]** Khiếu nại **không** tạo hoàn tiền trên sàn. Đối tác tự giải quyết với khách. **[Builder]** Đối tác thấy và trả lời khiếu nại về đơn của mình. Super Admin theo dõi, đóng khiếu nại, và dùng số khiếu nại để quyết định ẩn Listing. Màn admin hiển thị số khiếu nại theo Listing kèm `listing_audit_log`.

## 12. Thông báo

**[Chốt]** Email và chuông trong web.
- `notifications`: `user_id`, `type`, `title`, `body`, `link`, `read_at`.
- Mọi sự kiện nghiệp vụ ghi vào outbox trong cùng transaction. Worker tạo `notifications` và gửi email qua nhà cung cấp SMTP/API (**[Builder]** chọn sau: Amazon SES, Resend, ...), có retry và lưu lỗi.
- Sự kiện tối thiểu: mã xác minh email; Booking xác nhận (kèm QR); đối tác chấp nhận/từ chối Request Booking; nhắc thanh toán sắp hết hạn; Booking hết hạn/hủy; Refund xong; đối tác có Booking mới hoặc yêu cầu mới; Listing được duyệt/từ chối/bị ẩn; Payout được duyệt/chuyển/từ chối; có Complaint mới (gửi admin).

## 13. Báo cáo

**[Chốt]** Super Admin thấy toàn bộ hệ thống. Tối thiểu:
- GMV, Commission, Refund theo khoảng ngày, theo Service Type, theo đối tác.
- Số khách mới và khách hoạt động, số Booking theo trạng thái, tỷ lệ hủy và No-show.
- Tổng số dư đang chờ và khả dụng của mọi đối tác, Payout đang mở.

Đối tác: doanh thu, Commission, Booking theo Listing/ngày, số dư, lịch sử Payout.

**[Builder]** Giai đoạn 1 truy vấn trực tiếp trên Postgres (có index theo ngày và đối tác). Chậm thì thêm materialized view, chưa cần kho dữ liệu riêng.

## 14. Bảo mật và phân quyền

- Middleware xác định `role`. Mọi truy vấn phía đối tác luôn lọc theo `partner_id` lấy từ phiên đăng nhập, không lấy từ tham số client gửi lên. Có test riêng cho việc truy cập chéo giữa các đối tác.
- Mật khẩu hash bằng argon2id. Mã xác minh và refresh token lưu dạng hash.
- File (giấy phép, ảnh) lưu trên object storage tương thích S3 qua URL ký có hạn. File giấy tờ đối tác là private.
- Mọi thao tác của Super Admin lên tiền, trạng thái đối tác và Listing được ghi `admin_audit_log`.

## 15. Phần để builder quyết

Cấu trúc thư mục Go, framework HTTP, cách truy cập DB (khuyến nghị `sqlc` + `pgx` để viết SQL rõ ràng cho phần tiền và giữ chỗ), công cụ migration, nhà cung cấp email, API bản đồ, object storage, cách deploy, định dạng mã Booking, tên cột và index chi tiết, hình dạng request/response cụ thể.

## 16. Thứ tự làm đề xuất

Gợi ý cho bước lập kế hoạch, sẽ chi tiết ở plan:
1. Nền tảng: project Go, cấu hình, migration, logging, khung lỗi, test với Postgres thật.
2. `identity`: đăng ký, xác minh email, đăng nhập, Google, phân quyền.
3. `partner` + `service_types` + `commission_rates` + danh mục địa giới.
4. `catalog` lớp chung + duyệt/ẩn, rồi Attraction (đơn giản nhất, không giữ chỗ).
5. `booking` + `payment` + `ledger` cho Attraction, xuyên suốt từ đặt, thanh toán, check-in, hủy/hoàn, đổi lịch đến No-show. Đây là lát cắt chứng minh phần tiền đúng.
6. Payout.
7. Scheduled Ride (giữ ghế).
8. Self-Drive Rental (Request Booking + giữ xe theo khoảng thời gian).
9. Private Charter (báo giá theo km).
10. Review, Complaint, thông báo đầy đủ, báo cáo.
