package config_test

import (
	"strings"
	"testing"

	"github.com/bangbc-luna/lunain-travel/internal/platform/config"
)

func TestConfig_MapProvider(t *testing.T) {
	t.Run("dev without key is fake", func(t *testing.T) {
		setRequired(t)
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MapProvider != "fake" || cfg.GoongBaseURL != "https://rsapi.goong.io" {
			t.Errorf("provider = %q base = %q", cfg.MapProvider, cfg.GoongBaseURL)
		}
		if w := cfg.MapWarnings(); len(w) != 1 || !strings.Contains(w[0], "FAKE") {
			t.Errorf("warnings = %v", w)
		}
	})
	t.Run("dev with key is goong", func(t *testing.T) {
		setRequired(t)
		t.Setenv("GOONG_API_KEY", "k")
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MapProvider != "goong" || len(cfg.MapWarnings()) != 0 {
			t.Errorf("provider = %q warnings = %v", cfg.MapProvider, cfg.MapWarnings())
		}
	})
	t.Run("prod without key fails", func(t *testing.T) {
		setProd(t)
		setPayOS(t)
		_, err := config.Load()
		if err == nil || !strings.Contains(err.Error(), "GOONG_API_KEY") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("prod with fake fails", func(t *testing.T) {
		setProd(t)
		setPayOS(t)
		t.Setenv("MAP_PROVIDER", "fake")
		_, err := config.Load()
		if err == nil || !strings.Contains(err.Error(), "MAP_PROVIDER must be goong when APP_ENV=prod") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("prod with http base url fails", func(t *testing.T) {
		setProd(t)
		setPayOS(t)
		t.Setenv("GOONG_API_KEY", "k")
		t.Setenv("GOONG_BASE_URL", "http://rsapi.goong.io")
		if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "https://") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("prod with key ok", func(t *testing.T) {
		setProd(t)
		setPayOS(t)
		t.Setenv("GOONG_API_KEY", "k")
		if _, err := config.Load(); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("goong without key fails", func(t *testing.T) {
		setRequired(t)
		t.Setenv("MAP_PROVIDER", "goong")
		if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "GOONG_API_KEY") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown provider fails", func(t *testing.T) {
		setRequired(t)
		t.Setenv("MAP_PROVIDER", "google")
		if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "must be goong or fake") {
			t.Errorf("err = %v", err)
		}
	})
}
