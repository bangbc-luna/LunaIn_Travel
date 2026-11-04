-- name: InsertFile :exec
INSERT INTO files (id, storage_key, purpose, visibility, content_type, size_bytes, uploaded_by, created_at, updated_at)
VALUES (@id, @storage_key, @purpose, @visibility, @content_type, @size_bytes, @uploaded_by, @now, @now);

-- name: GetFile :one
SELECT * FROM files WHERE id = @id;
