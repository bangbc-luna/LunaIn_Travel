-- name: ListProvinces :many
SELECT code, name, slug FROM provinces ORDER BY sort_order, code;

-- name: ProvinceExists :one
SELECT EXISTS (SELECT 1 FROM provinces WHERE code = @code);

-- name: ListWardsByProvince :many
SELECT code, province_code, name, kind, slug FROM wards
WHERE province_code = @province_code
ORDER BY name, code;

-- name: WardInProvince :one
SELECT EXISTS (SELECT 1 FROM wards WHERE code = @code AND province_code = @province_code);
