package db

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestMigrationChecksums(t *testing.T) {
	const dir = "migrations"
	raw, err := os.ReadFile(filepath.Join(dir, "CHECKSUMS"))
	if err != nil {
		t.Fatalf("không đọc được CHECKSUMS: %v — chạy `make migrations-sum`", err)
	}
	listed := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			t.Fatalf("dòng CHECKSUMS sai định dạng: %q", line)
		}
		if !sha256Hex.MatchString(parts[0]) {
			t.Fatalf("dòng CHECKSUMS có hash không hợp lệ (cần 64 ký tự hex): %q", line)
		}
		if _, dup := listed[parts[1]]; dup {
			t.Fatalf("CHECKSUMS có mục trùng cho %s", parts[1])
		}
		listed[parts[1]] = parts[0]
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	present := map[string]bool{}
	for _, name := range files {
		present[name] = true
		want, ok := listed[name]
		if !ok {
			t.Errorf("migration %s chưa có trong CHECKSUMS — chạy `make migrations-sum` (chỉ khi thêm migration mới)", name)
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("migration %s đã bị sửa: migration đã merge là bất biến — hãy tạo migration mới", name)
		}
	}
	for name := range listed {
		if !present[name] {
			t.Errorf("CHECKSUMS liệt kê %s nhưng file không tồn tại (migration đã merge là bất biến, không được xóa)", name)
		}
	}
}
