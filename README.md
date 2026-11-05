# LunaIn_Travel

## Môi trường dev

- Hạ tầng dev chạy dưới compose project `lunain`:
  `docker compose -p lunain up -d postgres minio minio-init`
- `main` dùng DB `lunain`, do compose tạo sẵn khi khởi động container (`POSTGRES_DB`):
  `DATABASE_URL=postgres://lunain:lunain@localhost:5434/lunain?sslmode=disable`. Không chạy `make db-branch` trên `main`.
- Mỗi nhánh tính năng (worktree) dùng database riêng `lunain_<tên nhánh>` (chữ thường, ký tự không phải chữ hoặc số đổi thành `_`, tối đa 50 ký tự tên nhánh):
  `make db-branch` tạo DB (đã có thì bỏ qua) và in ra `DATABASE_URL` để export. `make db-branch-drop` xóa DB của nhánh hiện tại. Target drop từ chối xóa `lunain` và `lunain_main`; detached HEAD bị từ chối.
- Migration đã merge là **bất biến**: không sửa hay xóa file cũ, `TestMigrationChecksums` sẽ fail. Cần thay đổi thì tạo migration mới, rồi chạy `make migrations-sum` để cập nhật `CHECKSUMS` (chỉ chạy khi **thêm** migration).
