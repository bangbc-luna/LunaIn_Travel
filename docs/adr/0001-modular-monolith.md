# Modular monolith bằng Go trên một PostgreSQL

Một người làm backend, khoảng 10 đối tác lúc ra mắt, và phần quan trọng nhất (giữ chỗ + ghi tiền) cần nằm trong một transaction DB. Vì vậy chọn một binary Go chia module theo nghiệp vụ (identity, partner, catalog, booking, payment, ledger, review, notification, reporting), dùng chung một Postgres, việc nền chạy bằng hàng đợi trên Postgres. Không dùng microservice, Redis hay message broker ở giai đoạn 1. Module chỉ gọi nhau qua interface để sau này tách ra được.
