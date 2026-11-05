# LunaIn Travel

Sàn trung gian du lịch: đối tác đăng dịch vụ (vé tham quan, xe), khách đặt và thanh toán online cho LunaIn, LunaIn giữ hoa hồng và trả phần còn lại cho đối tác khi đối tác yêu cầu rút tiền.

## Người tham gia

**Customer** (Khách hàng):
Người có tài khoản đã xác minh email, tìm và đặt dịch vụ trên sàn.
_Avoid_: User, buyer, client

**Partner** (Đối tác):
Doanh nghiệp đã ký hợp đồng với LunaIn (khu du lịch, nhà xe, cửa hàng thuê xe), được LunaIn cấp tài khoản để đăng và vận hành dịch vụ.
_Avoid_: Vendor, merchant, supplier, seller

**Super Admin**:
Nhân sự LunaIn quản trị toàn sàn.
_Avoid_: Admin (khi không rõ là admin của ai), operator

## Dịch vụ và sản phẩm

**Service Type** (Loại dịch vụ):
Một ngành hàng của sàn, mỗi loại có cách đặt, giá và % hoa hồng riêng. Hiện có: Attraction Ticket, Scheduled Ride, Private Charter, Self-Drive Rental.
_Avoid_: Category, product type

**Listing** (Sản phẩm đăng bán):
Một thứ đối tác đăng lên sàn để khách đặt, thuộc đúng một Service Type: một khu du lịch, một tuyến xe, một dịch vụ xe bao chuyến, hoặc một cửa hàng thuê xe.
_Avoid_: Product, post, item

**Attraction** (Khu du lịch):
Listing thuộc loại Attraction Ticket, mô tả một khu du lịch và các loại vé của nó.
_Avoid_: Destination, place, spot

**Ticket Type** (Loại vé):
Một hạng vé do đối tác định nghĩa trong một Attraction, có tên, đối tượng (trẻ em/người lớn) và giá.
_Avoid_: Ticket category, fare

**Time Slot** (Khung giờ):
Một khung giờ hoạt động của Attraction để khách chọn khi đặt vé.

**Scheduled Ride** (Xe tuyến cố định):
Loại dịch vụ xe có tài xế chạy theo tuyến và giờ khởi hành định sẵn; khách mua theo số ghế, đối tác tự xếp chỗ.
_Avoid_: Bus ticket, shuttle

**Route** (Tuyến):
Đường đi cố định từ điểm đi đến điểm đến của một Scheduled Ride.

**Trip** (Chuyến):
Một lần chạy cụ thể của một Route vào một ngày giờ khởi hành, có số ghế giới hạn.
_Avoid_: Departure, journey

**Private Charter** (Xe bao chuyến):
Loại dịch vụ xe có tài xế mà khách bao nguyên xe; giá tính theo km cộng phụ phí.
_Avoid_: Taxi, private transfer

**Self-Drive Rental** (Thuê xe tự lái):
Loại dịch vụ cho khách thuê xe tự lái, tính theo ngày 24 giờ kể từ giờ nhận xe.
_Avoid_: Car hire, vehicle rental

**Vehicle Model** (Mẫu xe):
Một mẫu xe cửa hàng cho thuê, kèm số lượng xe đang có; sàn không quản lý từng chiếc.
_Avoid_: Vehicle, car (khi ý là mẫu)

**Rental Terms** (Điều khoản thuê):
Thông tin đối tác khai báo về cọc, giấy tờ, phí trả trễ, khu vực được đi; LunaIn chỉ hiển thị, khách tự xử lý với đối tác.

## Đặt chỗ

**Booking** (Đơn đặt):
Một lần khách đặt một Listing, có trạng thái riêng, thanh toán một lần và có mã QR riêng.
_Avoid_: Order, reservation

**Instant Booking** (Đặt ngay):
Booking được xác nhận ngay khi khách thanh toán xong (Attraction Ticket, Scheduled Ride).

**Request Booking** (Đặt theo yêu cầu):
Booking mà đối tác phải chấp nhận trước, rồi khách mới thanh toán (Private Charter, Self-Drive Rental).
_Avoid_: Inquiry, quote request

**Hold** (Giữ chỗ):
Khoảng thời gian ghế hoặc xe được giữ cho một Booking chưa thanh toán; quá hạn thì Booking hết hiệu lực và chỗ được nhả.

**Check-in**:
Việc đối tác quét mã QR của Booking để xác nhận khách đã đến sử dụng dịch vụ.

**No-show** (Bỏ vé):
Booking vé tham quan đã qua ngày tham quan mà chưa Check-in; không hoàn tiền, tính như đã hoàn thành.

**Reschedule** (Đổi lịch):
Việc khách đổi ngày hoặc khung giờ của một Booking vé tham quan; tối đa một lần, phải làm trong 14 ngày kể từ lúc thanh toán.
_Avoid_: Rebook, change date

**Cancellation Policy** (Chính sách hủy):
Danh sách mốc do đối tác đặt: hủy trước bao nhiêu giờ so với giờ sử dụng thì được hoàn bao nhiêu phần trăm.

## Tiền

**Gross Amount** (Giá trị đơn):
Tổng tiền khách trả cho một Booking.

**Commission** (Hoa hồng):
Phần LunaIn giữ trên một Booking, bằng Gross Amount nhân % hoa hồng của Service Type tại thời điểm đặt; không giảm khi hoàn tiền.
_Avoid_: Fee, platform fee, cut

**Commission Rate** (% hoa hồng):
Tỷ lệ hoa hồng do Super Admin đặt cho từng Service Type.

**Partner Earning** (Phần của đối tác):
Gross Amount trừ Commission trừ Refund; có thể âm.

**Refund** (Hoàn tiền):
Số tiền LunaIn trả lại khách khi Booking bị hủy, tính theo Cancellation Policy.

**Partner Balance** (Số dư đối tác):
Tổng Partner Earning chưa rút của một đối tác, gồm phần đang chờ (dịch vụ chưa diễn ra) và phần khả dụng (được rút).
_Avoid_: Wallet, account balance

**Payout** (Rút tiền):
Yêu cầu đối tác rút phần số dư khả dụng; Super Admin duyệt và chuyển khoản.
_Avoid_: Withdrawal (trong code dùng Payout), settlement

**Statement** (Sao kê):
Danh sách bút toán ngăn `available` giữa hai Payout (theo `seq`), kèm số dư đầu kỳ và cuối kỳ.

**Adjustment** (Điều chỉnh):
Bút toán tay của Super Admin vào số dư khả dụng của đối tác, bắt buộc có lý do.

**Reconciliation** (Đối soát):
Job hằng ngày so payments với PayOS và so số dư cache với sổ cái.

## Kiểm duyệt

**Listing Review** (Duyệt sản phẩm):
Lần duyệt bắt buộc của Super Admin trước khi một Listing hiện cho khách lần đầu; sửa sau đó không cần duyệt lại.

**Complaint** (Khiếu nại):
Phản ánh của khách về một Booking hoặc Listing, gửi cho Super Admin xử lý.
_Avoid_: Report, ticket (dễ nhầm với vé)

**Review** (Đánh giá):
Điểm sao và nhận xét khách để lại cho một Booking đã hoàn thành; mỗi Booking tối đa một Review.
_Avoid_: Rating (khi ý là cả nhận xét), feedback
