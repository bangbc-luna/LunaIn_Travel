# LunaIn_Travel

## Môi trường dev

- Hạ tầng dev chạy dưới compose project `lunain`:
  `docker compose -p lunain up -d postgres minio minio-init`
- Mỗi worktree/nhánh dùng database riêng `lunain_<tên nhánh>` (ký tự không phải chữ hoặc số đổi thành `_`):
  `make db-branch` tạo DB (đã có thì bỏ qua) và in ra `DATABASE_URL` để export. `make db-branch-drop` xóa DB của nhánh hiện tại. DB `lunain` dùng cho `main`; các nhánh khác dùng `lunain_<nhánh>` (tối đa 50 ký tự tên nhánh). Target drop từ chối xóa `lunain` và `lunain_main`; detached HEAD bị từ chối.
- Migration đã merge là **bất biến**: không sửa hay xóa file cũ, `TestMigrationChecksums` sẽ fail. Cần thay đổi thì tạo migration mới, rồi chạy `make migrations-sum` để cập nhật `CHECKSUMS` (chỉ chạy khi **thêm** migration).
