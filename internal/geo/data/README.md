# Administrative units data (after the 2025 merger)

`provinces.csv` and `wards.csv` hold Vietnam's administrative units: 34
provinces and 3,321 commune-level units. The list comes from the official
service and is current **as of 2026-11-04**. Its base is the Bảng danh mục và
mã số các đơn vị hành chính Việt Nam of **Quyết định 19/2025/QĐ-TTg** (effective
01/07/2025), plus the official changes made since then, so users see the names
in use today. Codes are the official codes from the decision.

`go run ./internal/geo/gen` turns these files into
`internal/platform/db/migrations/00007_geo_seed.sql`.

## Source

- Official web service of Cục Thống kê ("Danh mục hành chính" system):
  `https://danhmuchanhchinh.nso.gov.vn/DMDVHC.asmx`. Its documentation is at
  `https://danhmuchanhchinh.nso.gov.vn/WebService.aspx`.
- SOAP calls:
  - `DanhMucTinh(DenNgay="04/11/2026")`
  - `DanhMucPhuongXa(DenNgay="04/11/2026", Tinh="", TenTinh="", QuanHuyen="", TenQuanHuyen="")`
- Retrieved: 2026-11-04.
- SHA-256 of the raw responses:
  - provinces: `f3fb4b3d5d04c6d7f8776ce756bcafed9340913d6cad36471a75aa8827a1202b`
  - communes: `0ff480b1febabf7d3ea2e2dede4af923fc79427a1c967043d541ce0dc4925013`

## Counts (current list, 2026-11-04)

| Level | Count |
|---|---|
| Provinces | 34 (25 tỉnh, 9 thành phố trực thuộc trung ương) |
| Commune-level units | 3,321 |
| — xã | 2,599 |
| — phường | 709 |
| — đặc khu | 13 |

For comparison, the same service queried for 01/07/2025 (the decision itself)
returned 34 provinces (28 tỉnh, 6 thành phố) and 3,321 communes (2,621 xã,
687 phường, 13 đặc khu). Those are the totals published with QĐ 19/2025.

## Changes after QĐ 19/2025

Found by comparing the 01/07/2025 and 04/11/2026 responses. No unit was added
or removed, and no code changed.

- Three provinces became centrally-run cities:
  - 22: Tỉnh Quảng Ninh → Thành phố Quảng Ninh
  - 24: Tỉnh Bắc Ninh → Thành phố Bắc Ninh
  - 75: Tỉnh Đồng Nai → Thành phố Đồng Nai
- 22 xã became phường (same name otherwise):
  - Bắc Ninh (24): 07294 Bố Hạ, 07375 Lạng Giang, 07399 Kép, 07444 Lục Nam,
    07840 Hiệp Hoà, 09193 Yên Phong, 09292 Phù Lãng, 09313 Chi Lăng,
    09319 Tiên Du, 09454 Gia Bình, 09475 Nhân Thắng, 09496 Lương Tài.
  - Đồng Nai (75): 25270 Lộc Ninh, 25357 Tân Khai, 25363 Đồng Phú,
    26116 Tân Phú, 26170 Trị An, 26248 Trảng Bom, 26326 Dầu Giây,
    26368 Long Thành, 26425 Xuân Lộc, 26485 Nhơn Trạch.

## Transformation

- Codes are copied unchanged: 2 digits for a province, 5 digits for a commune.
- `provinces.csv` has columns `code,name,sort_order`. `sort_order` is the order
  in the decision (code order), 1..34.
- `wards.csv` has columns `code,province_code,name,kind` and is sorted by code.
  - `name` is the full official name with its prefix (`Phường Bến Thành`).
  - `kind` comes from the source type: Xã → `xa`, Phường → `phuong`,
    Đặc khu → `dac_khu`.
- Text is normalized to Unicode NFC and runs of whitespace are collapsed. This
  changed 6 names in the source:
  - `00070`: `"Phường \nHoàn Kiếm"` (line break inside the name)
  - `00385`, `09784`, `09931`, `20333`: decomposed combining marks
  - `31459`: double space in `"Xã  Vĩnh Tường"`
- `06325`: the source name `xã Bắc Sơn` (lowercase prefix) is written as
  `Xã Bắc Sơn`.

## Slugs

`slug` is not stored in the CSVs. The generator derives it from `name` with
`geo.Slug`: lowercase, no diacritics, `đ` → `d`, every other non-alphanumeric
run → `-`.

Some pairs are distinct official units in the same province whose names
differ only by diacritics, so they would get the same slug. For those pairs,
`geo.WardSlugs` appends `-<ward code>` to the slug of **both** members. All
other slugs are left unchanged. The generator prints the resolved groups.

| Province | Wards | Slugs |
|---|---|---|
| 19 | 02143 Xã Văn Lang / 05665 Xã Văn Lăng | `xa-van-lang-02143` / `xa-van-lang-05665` |
| 31 | 10888 Xã Cẩm Giang / 10903 Xã Cẩm Giàng | `xa-cam-giang-10888` / `xa-cam-giang-10903` |
| 51 | 21484 Xã Ba Tơ / 21523 Xã Ba Tô | `xa-ba-to-21484` / `xa-ba-to-21523` |
| 51 | 21289 Xã Sơn Hà / 21292 Xã Sơn Hạ | `xa-son-ha-21289` / `xa-son-ha-21292` |
| 75 | 25280 Xã Lộc Thạnh / 25294 Xã Lộc Thành | `xa-loc-thanh-25280` / `xa-loc-thanh-25294` |
| 79 | 25807 Xã Thanh An / 27676 Xã Thạnh An | `xa-thanh-an-25807` / `xa-thanh-an-27676` |
| 80 | 25534 Xã Tân Thành / 27826 Xã Tân Thạnh | `xa-tan-thanh-25534` / `xa-tan-thanh-27826` |
| 82 | 29938 Xã Tân Thành / 30157 Xã Tân Thạnh | `xa-tan-thanh-29938` / `xa-tan-thanh-30157` |

Province slugs have no collisions.
