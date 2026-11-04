// Command gen writes the geo seed migration from internal/geo/data/*.csv.
// Run it from the repository root: go run ./internal/geo/gen
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bangbc-luna/lunain-travel/internal/geo"
)

func main() {
	provincesPath := flag.String("provinces", "internal/geo/data/provinces.csv", "provinces CSV")
	wardsPath := flag.String("wards", "internal/geo/data/wards.csv", "wards CSV")
	out := flag.String("out", "internal/platform/db/migrations/00007_geo_seed.sql", "output migration")
	flag.Parse()

	if err := run(*provincesPath, *wardsPath, *out); err != nil {
		fmt.Fprintln(os.Stderr, "geo gen:", err)
		os.Exit(1)
	}
}

func run(provincesPath, wardsPath, out string) error {
	provinces, err := geo.ReadProvincesCSV(provincesPath)
	if err != nil {
		return err
	}
	wards, err := geo.ReadWardsCSV(wardsPath)
	if err != nil {
		return err
	}
	if err := geo.ValidateSeed(provinces, wards); err != nil {
		return err
	}
	if err := os.WriteFile(out, geo.Render(provinces, wards), 0o644); err != nil {
		return err
	}
	slugs, collisions := geo.WardSlugs(wards)
	for _, codes := range collisions {
		fmt.Printf("slug collision resolved with ward code suffix: %v -> ", codes)
		for _, c := range codes {
			fmt.Printf("%s ", slugs[c])
		}
		fmt.Println()
	}
	fmt.Printf("wrote %s: %d provinces, %d wards\n", out, len(provinces), len(wards))
	return nil
}
