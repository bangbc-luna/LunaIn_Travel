package geo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

func newSvc(t *testing.T) (*geo.Service, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.Postgres(t)
	return geo.NewService(pool), pool
}

func appCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae), "not an AppError: %v", err)
	return ae.Status, ae.Code
}

func TestSeed_Has34Provinces(t *testing.T) {
	svc, _ := newSvc(t)
	list, err := svc.Provinces(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 34)
	// Ordered by sort_order, which follows the decision's code order.
	require.Equal(t, "01", list[0].Code)
	require.Equal(t, "96", list[len(list)-1].Code)
}

func TestSeed_WardCountPlausible(t *testing.T) {
	_, pool := newSvc(t)
	ctx := context.Background()
	var total int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM wards`).Scan(&total))
	require.GreaterOrEqual(t, total, 3200)
	require.LessOrEqual(t, total, 3400)

	var emptyProvinces int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM provinces p
		WHERE NOT EXISTS (SELECT 1 FROM wards w WHERE w.province_code = p.code)`).Scan(&emptyProvinces))
	require.Zero(t, emptyProvinces, "every province has at least one ward")

	var emptySlugs int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM provinces WHERE slug = '')
		     + (SELECT count(*) FROM wards WHERE slug = '')`).Scan(&emptySlugs))
	require.Zero(t, emptySlugs)
}

func TestSeed_KnownEntries(t *testing.T) {
	svc, _ := newSvc(t)
	list, err := svc.Provinces(context.Background())
	require.NoError(t, err)
	byName := map[string]geo.Province{}
	for _, p := range list {
		byName[p.Name] = p
	}
	require.Contains(t, byName, "Thành phố Hồ Chí Minh")
	require.Equal(t, "thanh-pho-ho-chi-minh", byName["Thành phố Hồ Chí Minh"].Slug)
	require.Contains(t, byName, "Thành phố Hà Nội")
	// Current names (upgraded to centrally-run cities after QĐ 19/2025).
	require.Contains(t, byName, "Thành phố Đồng Nai")
	for name := range byName {
		require.NotContains(t, name, "Bà Rịa - Vũng Tàu", "merged into Thành phố Hồ Chí Minh")
	}

	wards, err := svc.Wards(context.Background(), byName["Thành phố Hồ Chí Minh"].Code)
	require.NoError(t, err)
	var benThanh *geo.Ward
	for i := range wards {
		if wards[i].Name == "Phường Bến Thành" {
			benThanh = &wards[i]
		}
	}
	require.NotNil(t, benThanh)
	require.Equal(t, "phuong", benThanh.Kind)
	require.Equal(t, "phuong-ben-thanh", benThanh.Slug)
	require.Equal(t, "79", benThanh.ProvinceCode)
	for i := 1; i < len(wards); i++ {
		require.Equal(t, "79", wards[i].ProvinceCode)
	}
}

func TestWards_UnknownProvince404(t *testing.T) {
	svc, _ := newSvc(t)
	_, err := svc.Wards(context.Background(), "99")
	status, code := appCode(t, err)
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "province_not_found", code)
}

func TestValidateLocation_WardMustBelongToProvince(t *testing.T) {
	svc, pool := newSvc(t)
	ctx := context.Background()
	// 26740 = Phường Sài Gòn (79, HCM); 00004 = Phường Ba Đình (01, Hà Nội).
	check := func(province, ward string) error {
		return db.InTx(ctx, pool, func(tx pgx.Tx) error {
			return svc.ValidateLocation(ctx, tx, province, ward)
		})
	}
	require.NoError(t, check("79", "26740"))
	require.NoError(t, check("01", "00004"))

	for _, c := range [][2]string{{"79", "00004"}, {"01", "26740"}, {"79", "99999"}, {"99", "00004"}, {"", ""}} {
		status, code := appCode(t, check(c[0], c[1]))
		require.Equal(t, http.StatusUnprocessableEntity, status, c)
		require.Equal(t, "invalid_location", code, c)
	}
}

func loadSeedData(t *testing.T) ([]geo.SeedProvince, []geo.SeedWard) {
	t.Helper()
	provinces, err := geo.ReadProvincesCSV("data/provinces.csv")
	require.NoError(t, err)
	wards, err := geo.ReadWardsCSV("data/wards.csv")
	require.NoError(t, err)
	return provinces, wards
}

func TestGen_Deterministic(t *testing.T) {
	provinces, wards := loadSeedData(t)
	require.NoError(t, geo.ValidateSeed(provinces, wards))

	a := geo.Render(provinces, wards)
	// Shuffle the input order: output is sorted by code, so it must not change.
	rp := append([]geo.SeedProvince(nil), provinces...)
	rw := append([]geo.SeedWard(nil), wards...)
	for i, j := 0, len(rp)-1; i < j; i, j = i+1, j-1 {
		rp[i], rp[j] = rp[j], rp[i]
	}
	for i, j := 0, len(rw)-1; i < j; i, j = i+1, j-1 {
		rw[i], rw[j] = rw[j], rw[i]
	}
	b := geo.Render(rp, rw)
	require.True(t, bytes.Equal(a, b), "Render is not deterministic")

	// The committed migration must be exactly what the generator produces.
	committed, err := os.ReadFile("../platform/db/migrations/00007_geo_seed.sql")
	require.NoError(t, err)
	require.True(t, bytes.Equal(a, committed), "00007_geo_seed.sql is stale: run go run ./internal/geo/gen")
}

func TestSeed_SlugCollisionsResolvedWithCode(t *testing.T) {
	svc, pool := newSvc(t)
	ctx := context.Background()
	var dups int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM wards GROUP BY province_code, slug HAVING count(*) > 1) d`).Scan(&dups))
	require.Zero(t, dups)

	// Xã Văn Lang (02143) and Xã Văn Lăng (05665), province 19, share a
	// diacritic-free name: both get their code appended.
	wards, err := svc.Wards(ctx, "19")
	require.NoError(t, err)
	got := map[string]string{}
	for _, w := range wards {
		got[w.Code] = w.Slug
	}
	require.Equal(t, "xa-van-lang-02143", got["02143"])
	require.Equal(t, "xa-van-lang-05665", got["05665"])
}

func TestWardSlugs_SuffixOnlyCollidingGroups(t *testing.T) {
	wards := []geo.SeedWard{
		{Code: "00002", ProvinceCode: "01", Name: "Phường Dong", Kind: "phuong"},
		{Code: "00001", ProvinceCode: "01", Name: "Phường Đông", Kind: "phuong"},
		{Code: "00003", ProvinceCode: "02", Name: "Phường Dong", Kind: "phuong"},
		{Code: "00004", ProvinceCode: "01", Name: "Xã Tây", Kind: "xa"},
	}
	slugs, collisions := geo.WardSlugs(wards)
	require.Equal(t, map[string]string{
		"00001": "phuong-dong-00001",
		"00002": "phuong-dong-00002",
		"00003": "phuong-dong", // other province: no collision
		"00004": "xa-tay",
	}, slugs)
	require.Equal(t, [][]string{{"00001", "00002"}}, collisions)
}

func TestValidateSeed_RejectsDuplicateSlugAfterResolution(t *testing.T) {
	provinces := []geo.SeedProvince{{Code: "01", Name: "Thành phố Hà Nội", SortOrder: 1}}
	wards := []geo.SeedWard{
		{Code: "00001", ProvinceCode: "01", Name: "Phường Đông", Kind: "phuong"},
		{Code: "00002", ProvinceCode: "01", Name: "Phường Dong", Kind: "phuong"},
		{Code: "00003", ProvinceCode: "01", Name: "Phường Dong 00001", Kind: "phuong"},
	}
	require.Error(t, geo.ValidateSeed(provinces, wards))
	require.NoError(t, geo.ValidateSeed(provinces, wards[:2]))
}

func TestHTTP_PublicCachedRoutes(t *testing.T) {
	svc, _ := newSvc(t)
	mux := http.NewServeMux()
	svc.Routes(mux)
	mux.HandleFunc("/", httpx.NotFound)
	srv := httptest.NewServer(httpx.Chain(mux, httpx.RequestID, httpx.Recover))
	t.Cleanup(srv.Close)

	get := func(path string) *http.Response {
		res, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}

	res := get("/v1/geo/provinces")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "public, max-age=86400", res.Header.Get("Cache-Control"))
	var provinces []map[string]string
	require.NoError(t, json.NewDecoder(res.Body).Decode(&provinces))
	require.Len(t, provinces, 34)
	require.Equal(t, map[string]string{"code": "01", "name": "Thành phố Hà Nội", "slug": "thanh-pho-ha-noi"}, provinces[0])

	res = get("/v1/geo/provinces/79/wards")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "public, max-age=86400", res.Header.Get("Cache-Control"))
	var wards []map[string]string
	require.NoError(t, json.NewDecoder(res.Body).Decode(&wards))
	require.NotEmpty(t, wards)
	require.ElementsMatch(t, []string{"code", "province_code", "name", "kind", "slug"}, keys(wards[0]))

	res = get("/v1/geo/provinces/99/wards")
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Empty(t, res.Header.Get("Cache-Control"), "errors are not cached")
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
