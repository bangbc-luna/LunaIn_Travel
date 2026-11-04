package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/bangbc-luna/lunain-travel/internal/platform/config"
)

const secret32 = "0123456789abcdef0123456789abcdef"

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("JWT_SECRET", secret32)
	t.Setenv("CODE_PEPPER", secret32)
}

func TestLoad_Defaults(t *testing.T) {
	setRequired(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AppEnv != "dev" {
		t.Errorf("AppEnv = %q, want dev", cfg.AppEnv)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.MailFrom != "LunaIn Travel <no-reply@lunain.vn>" {
		t.Errorf("MailFrom = %q", cfg.MailFrom)
	}
	if string(cfg.JWTSecret) != secret32 {
		t.Errorf("JWTSecret not loaded")
	}
}

func TestLoad_RejectsShortSecret(t *testing.T) {
	setRequired(t)
	t.Setenv("JWT_SECRET", "short")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("err = %v, want error mentioning JWT_SECRET", err)
	}
}

func TestLoad_ProdRequiresSMTP(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("TRUSTED_PROXY_HOPS", "1")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "SMTP_HOST") {
		t.Fatalf("err = %v, want error mentioning SMTP_HOST", err)
	}
}

func TestLoad_RejectsNegativeTrustedProxyHops(t *testing.T) {
	setRequired(t)
	t.Setenv("TRUSTED_PROXY_HOPS", "-1")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_HOPS") {
		t.Fatalf("err = %v, want error mentioning TRUSTED_PROXY_HOPS", err)
	}
}

func TestLoad_RejectsUnknownEnv(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_ENV", "staging")

	if _, err := config.Load(); err == nil {
		t.Fatal("want error for APP_ENV=staging")
	}
}

func TestLoad_ProdRequiresExplicitTrustedProxyHops(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("TRUSTED_PROXY_HOPS", "") // registers restore
	os.Unsetenv("TRUSTED_PROXY_HOPS")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_HOPS") {
		t.Fatalf("err = %v, want error mentioning TRUSTED_PROXY_HOPS", err)
	}

	t.Setenv("TRUSTED_PROXY_HOPS", "0")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load with explicit 0: %v", err)
	}
	if cfg.TrustedProxyHops != 0 {
		t.Errorf("TrustedProxyHops = %d, want 0", cfg.TrustedProxyHops)
	}
}
