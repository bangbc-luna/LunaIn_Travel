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

**Ride Schedule** (Lịch chạy):
Lịch lặp của một Route (giờ khởi hành, các thứ trong tuần, khoảng ngày hiệu lực, số ghế, giá mỗi ghế). Hệ thống sinh sẵn các Trip từ lịch này cho 60 ngày tới.

**Round Trip** (Khứ hồi):
Hai Booking riêng (chiều đi và chiều về) trong cùng một Checkout, thanh toán chung một lần. Chiều về là Route ngược lại của cùng đối tác, cùng số ghế; hai chiều được hủy cùng nhau.

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

**Response Deadline** (Hạn phản hồi):
Mốc đối tác phải chấp nhận hoặc từ chối một Request Booking: sớm hơn trong hai mốc "12 giờ sau khi khách gửi" và "2 giờ trước giờ bắt đầu dịch vụ"; quá hạn thì yêu cầu hết hiệu lực.

**Accept** (Chấp nhận):
Việc đối tác nhận một Request Booking còn trong Response Deadline; từ lúc đó khách có 2 giờ để thanh toán (Payment Deadline). Đã chấp nhận thì đối tác không rút lại được.

**Reject** (Từ chối):
Việc đối tác không nhận một Request Booking chưa được chấp nhận, kèm lý do; chỗ được nhả ngay.

**Payment Deadline** (Hạn thanh toán):
Mốc khách phải thanh toán một Request Booking đã được chấp nhận; quá hạn (cộng thời gian đệm) thì Booking hết hiệu lực.

**Withdraw** (Rút yêu cầu):
Việc khách tự hủy miễn phí một Request Booking chưa thanh toán (đang chờ đối tác hoặc đang chờ thanh toán); chỗ được nhả ngay. Không nhầm với Payout.

**Handover** (Giao xe):
Check-in của Self-Drive Rental: đối tác quét QR khi giao xe cho khách, từ 2 giờ trước giờ nhận đến trước giờ trả; Booking chuyển sang đang thuê.

**Vehicle Return** (Nhận lại xe):
Việc đối tác xác nhận đã nhận lại xe thuê, sớm hay đúng hạn; Booking hoàn thành, xe trống lại ngay, trả sớm không hoàn tiền. Đối tác quên bấm thì hệ thống tự hoàn thành sau giờ trả 24 giờ.

**Hold** (Giữ chỗ):
Khoảng thời gian ghế hoặc xe được giữ cho một Booking chưa thanh toán; quá hạn thì Booking hết hiệu lực và chỗ được nhả.

**Check-in**:
Việc đối tác quét mã QR của Booking để xác nhận khách đã đến sử dụng dịch vụ.

**No-show** (Bỏ vé):
Booking đã thanh toán mà khách không đến sử dụng dịch vụ; không hoàn tiền, đối tác vẫn nhận tiền như đã hoàn thành. Vé tham quan: qua ngày tham quan mà chưa Check-in. Xe tuyến: quá giờ khởi hành một khoảng mà chưa Check-in. Thuê xe tự lái: đối tác đánh dấu khách không đến nhận xe (từ giờ nhận xe trở đi), hoặc tới giờ trả mà xe chưa được giao.

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
