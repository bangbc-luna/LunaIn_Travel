# Số dư đối tác là tổng các bút toán bất biến, có ngăn pending/available

LunaIn giữ tiền khách, hoa hồng không giảm khi hoàn tiền, và số dư đối tác có thể âm. Nếu chỉ lưu một con số số dư thì không giải thích được nó. Vì vậy mọi biến động (thu tiền, hoàn, giải phóng, rút, điều chỉnh) là một dòng `ledger_entries` không được sửa. `partner_balances` chỉ là cache cập nhật cùng transaction và được đối soát định kỳ. Không làm double-entry đầy đủ ở giai đoạn 1, vì sổ của LunaIn chỉ cần ghi nhận Commission theo Booking.
