# LunaIn_Travel

## Môi trường dev

- Hạ tầng dev chạy dưới compose project `lunain`:
  `docker compose -p lunain up -d postgres minio minio-init`
- Mỗi worktree/nhánh dùng database riêng `lunain_<tên nhánh>` (ký tự không phải chữ hoặc số đổi thành `_`):
  `make db-branch` tạo DB (đã có thì bỏ qua) và in ra `DATABASE_URL` để export. `make db-branch-drop` xóa DB của nhánh hiện tại. DB `lunain` chỉ dùng cho `main` và target drop từ chối xóa nó.
- Migration đã merge là **bất biến**: không sửa hay xóa file cũ, `TestMigrationChecksums` sẽ fail. Cần thay đổi thì tạo migration mới, rồi chạy `make migrations-sum` để cập nhật `CHECKSUMS` (chỉ chạy khi **thêm** migration).
