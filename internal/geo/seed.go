package geo

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// SeedProvince is one row of data/provinces.csv.
type SeedProvince struct {
	Code      string
	Name      string
	SortOrder int
}

// SeedWard is one row of data/wards.csv.
type SeedWard struct {
	Code         string
	ProvinceCode string
	Name         string
	Kind         string
}

// seedBatch is the number of rows per INSERT statement in the seed migration.
const seedBatch = 500

// Slug lowercases s, strips Vietnamese diacritics (đ becomes d), and joins
// the remaining runs of ASCII letters and digits with "-".
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == 'đ' || r == 'Đ':
			r = 'd'
		default:
			r = unicode.ToLower(r)
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	return b.String()
}

func readCSV(path string, header []string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = len(header)
	got, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: read header: %w", path, err)
	}
	if !slices.Equal(got, header) {
		return nil, fmt.Errorf("%s: header %v, want %v", path, got, header)
	}
	var rows [][]string
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rows = append(rows, rec)
	}
}

// ReadProvincesCSV reads a code,name,sort_order file.
func ReadProvincesCSV(path string) ([]SeedProvince, error) {
	rows, err := readCSV(path, []string{"code", "name", "sort_order"})
	if err != nil {
		return nil, err
	}
	out := make([]SeedProvince, 0, len(rows))
	for _, r := range rows {
		n, err := strconv.Atoi(r[2])
		if err != nil {
			return nil, fmt.Errorf("%s: province %s: sort_order: %w", path, r[0], err)
		}
		out = append(out, SeedProvince{Code: r[0], Name: r[1], SortOrder: n})
	}
	return out, nil
}

// ReadWardsCSV reads a code,province_code,name,kind file.
func ReadWardsCSV(path string) ([]SeedWard, error) {
	rows, err := readCSV(path, []string{"code", "province_code", "name", "kind"})
	if err != nil {
		return nil, err
	}
	out := make([]SeedWard, 0, len(rows))
	for _, r := range rows {
		out = append(out, SeedWard{Code: r[0], ProvinceCode: r[1], Name: r[2], Kind: r[3]})
	}
	return out, nil
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// WardSlugs returns the slug of every ward keyed by ward code. Some distinct
// wards of one province have names that differ only by diacritics (Xã Văn
// Lang / Xã Văn Lăng) and so share Slug(name); every member of such a group
// gets "-<ward code>" appended instead. collisions lists those groups (ward
// codes, sorted) ordered by province code then slug, for the generator to
// report.
func WardSlugs(wards []SeedWard) (slugs map[string]string, collisions [][]string) {
	groups := map[[2]string][]string{}
	for _, w := range wards {
		key := [2]string{w.ProvinceCode, Slug(w.Name)}
		groups[key] = append(groups[key], w.Code)
	}
	slugs = make(map[string]string, len(wards))
	keys := make([][2]string, 0, len(groups))
	for key, codes := range groups {
		if len(codes) == 1 || key[1] == "" {
			for _, c := range codes {
				slugs[c] = key[1]
			}
			continue
		}
		keys = append(keys, key)
		for _, c := range codes {
			slugs[c] = key[1] + "-" + c
		}
	}
	slices.SortFunc(keys, func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	})
	for _, key := range keys {
		codes := slices.Clone(groups[key])
		slices.Sort(codes)
		collisions = append(collisions, codes)
	}
	return slugs, collisions
}

// ValidateSeed checks codes, kinds and slugs so that a bad CSV fails in the
// generator instead of when the migration runs. Name collisions are resolved
// by WardSlugs; any (province, slug) duplicate left after that is an error.
func ValidateSeed(provinces []SeedProvince, wards []SeedWard) error {
	var errs []error
	pCodes := map[string]bool{}
	pSlugs := map[string]string{}
	for _, p := range provinces {
		if !isDigits(p.Code, 2) {
			errs = append(errs, fmt.Errorf("province %q: code must be 2 digits", p.Code))
		}
		if pCodes[p.Code] {
			errs = append(errs, fmt.Errorf("province %s: duplicate code", p.Code))
		}
		pCodes[p.Code] = true
		s := Slug(p.Name)
		if s == "" {
			errs = append(errs, fmt.Errorf("province %s: empty slug", p.Code))
		} else if other, ok := pSlugs[s]; ok {
			errs = append(errs, fmt.Errorf("province %s: slug %q already used by %s", p.Code, s, other))
		}
		pSlugs[s] = p.Code
	}
	slugs, _ := WardSlugs(wards)
	wCodes := map[string]bool{}
	wSlugs := map[[2]string]string{}
	for _, w := range wards {
		if !isDigits(w.Code, 5) {
			errs = append(errs, fmt.Errorf("ward %q: code must be 5 digits", w.Code))
		}
		if wCodes[w.Code] {
			errs = append(errs, fmt.Errorf("ward %s: duplicate code", w.Code))
		}
		wCodes[w.Code] = true
		if !pCodes[w.ProvinceCode] {
			errs = append(errs, fmt.Errorf("ward %s: unknown province %q", w.Code, w.ProvinceCode))
		}
		switch w.Kind {
		case "xa", "phuong", "dac_khu":
		default:
			errs = append(errs, fmt.Errorf("ward %s: invalid kind %q", w.Code, w.Kind))
		}
		s := slugs[w.Code]
		key := [2]string{w.ProvinceCode, s}
		if s == "" {
			errs = append(errs, fmt.Errorf("ward %s: empty slug", w.Code))
		} else if other, ok := wSlugs[key]; ok {
			errs = append(errs, fmt.Errorf("ward %s: slug %q already used by %s in province %s", w.Code, s, other, w.ProvinceCode))
		}
		wSlugs[key] = w.Code
	}
	return errors.Join(errs...)
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func writeInserts(buf *bytes.Buffer, head string, rows []string) {
	for start := 0; start < len(rows); start += seedBatch {
		end := min(start+seedBatch, len(rows))
		buf.WriteString(head)
		buf.WriteString(" VALUES\n")
		for i, r := range rows[start:end] {
			buf.WriteString("    ")
			buf.WriteString(r)
			if i < end-start-1 {
				buf.WriteString(",\n")
			} else {
				buf.WriteString(";\n\n")
			}
		}
	}
}

// Render produces the 00007_geo_seed.sql goose migration. Output depends only
// on the input set: rows are sorted by code.
func Render(provinces []SeedProvince, wards []SeedWard) []byte {
	ps := slices.Clone(provinces)
	slices.SortFunc(ps, func(a, b SeedProvince) int { return cmp.Compare(a.Code, b.Code) })
	ws := slices.Clone(wards)
	slices.SortFunc(ws, func(a, b SeedWard) int { return cmp.Compare(a.Code, b.Code) })

	var buf bytes.Buffer
	buf.WriteString("-- Code generated by go run ./internal/geo/gen from internal/geo/data; DO NOT EDIT.\n\n")
	buf.WriteString("-- +goose Up\n")

	pRows := make([]string, len(ps))
	for i, p := range ps {
		pRows[i] = fmt.Sprintf("(%s, %s, %s, %d)", sqlString(p.Code), sqlString(p.Name), sqlString(Slug(p.Name)), p.SortOrder)
	}
	writeInserts(&buf, "INSERT INTO provinces (code, name, slug, sort_order)", pRows)

	slugs, _ := WardSlugs(ws)
	wRows := make([]string, len(ws))
	for i, w := range ws {
		wRows[i] = fmt.Sprintf("(%s, %s, %s, %s, %s)",
			sqlString(w.Code), sqlString(w.ProvinceCode), sqlString(w.Name), sqlString(w.Kind), sqlString(slugs[w.Code]))
	}
	writeInserts(&buf, "INSERT INTO wards (code, province_code, name, kind, slug)", wRows)

	buf.WriteString("-- +goose Down\n")
	buf.WriteString("DELETE FROM wards;\n")
	buf.WriteString("DELETE FROM provinces;\n")
	return buf.Bytes()
}
