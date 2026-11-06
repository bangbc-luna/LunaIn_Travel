package config_test

import (
	"strings"
	"testing"

	"github.com/bangbc-luna/lunain-travel/internal/platform/config"
)

func setProd(t *testing.T) {
	t.Helper()
	setRequired(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("TRUSTED_PROXY_HOPS", "1")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("S3_ENDPOINT", "http://s3")
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("S3_ACCESS_KEY", "a")
	t.Setenv("S3_SECRET_KEY", "s")
	t.Setenv("PUBLIC_FILE_BASE_URL", "http://s3/b")
}

func setPayOS(t *testing.T) {
	t.Helper()
	t.Setenv("PAYOS_CLIENT_ID", "c")
	t.Setenv("PAYOS_API_KEY", "k")
	t.Setenv("PAYOS_CHECKSUM_KEY", "s")
	t.Setenv("PAYOS_RETURN_URL", "https://x/return")
	t.Setenv("PAYOS_CANCEL_URL", "https://x/cancel")
}

func TestConfig_PaymentProvider(t *testing.T) {
	t.Run("dev without keys defaults to fake", func(t *testing.T) {
		setRequired(t)
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PaymentProvider != "fake" {
			t.Errorf("provider = %q, want fake", cfg.PaymentProvider)
		}
		if cfg.PayOSBaseURL != "https://api-merchant.payos.vn" {
			t.Errorf("base url = %q", cfg.PayOSBaseURL)
		}
	})
	t.Run("dev with keys defaults to payos", func(t *testing.T) {
		setRequired(t)
		setPayOS(t)
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PaymentProvider != "payos" {
			t.Errorf("provider = %q, want payos", cfg.PaymentProvider)
		}
	})
	t.Run("prod without keys fails", func(t *testing.T) {
		setProd(t)
		_, err := config.Load()
		if err == nil || !strings.Contains(err.Error(), "PAYOS_") {
			t.Fatalf("err = %v, want PAYOS_ error", err)
		}
	})
	t.Run("prod rejects fake", func(t *testing.T) {
		setProd(t)
		t.Setenv("PAYMENT_PROVIDER", "fake")
		_, err := config.Load()
		if err == nil || !strings.Contains(err.Error(), "PAYMENT_PROVIDER") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("prod with all keys ok", func(t *testing.T) {
		setProd(t)
		t.Setenv("PAYOS_CLIENT_ID", "c")
		t.Setenv("PAYOS_API_KEY", "k")
		t.Setenv("PAYOS_CHECKSUM_KEY", "s")
		t.Setenv("PAYOS_RETURN_URL", "https://x/return")
		t.Setenv("PAYOS_CANCEL_URL", "https://x/cancel")
		t.Setenv("GOONG_API_KEY", "k")
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PaymentProvider != "payos" {
			t.Errorf("provider = %q", cfg.PaymentProvider)
		}
	})
	t.Run("unknown provider fails", func(t *testing.T) {
		setRequired(t)
		t.Setenv("PAYMENT_PROVIDER", "stripe")
		if _, err := config.Load(); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestConfig_ExplicitPayOSRequiresEverythingInDev(t *testing.T) {
	setRequired(t)
	t.Setenv("PAYMENT_PROVIDER", "payos")
	t.Setenv("PAYOS_CLIENT_ID", "c")
	t.Setenv("PAYOS_API_KEY", "k")
	t.Setenv("PAYOS_CHECKSUM_KEY", "s")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "PAYOS_RETURN_URL") {
		t.Fatalf("err = %v, want PAYOS_RETURN_URL", err)
	}
}

func TestConfig_ProdRequiresHTTPSBaseURL(t *testing.T) {
	setProd(t)
	setPayOS(t)
	t.Setenv("PAYOS_BASE_URL", "http://api.example.com")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "PAYOS_BASE_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfig_PaymentWarnings(t *testing.T) {
	setRequired(t)
	t.Setenv("PAYOS_CLIENT_ID", "c")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(cfg.PaymentWarnings(), "|")
	if !strings.Contains(w, "FAKE") || !strings.Contains(w, "PAYOS_API_KEY") || !strings.Contains(w, "PAYOS_CHECKSUM_KEY") {
		t.Errorf("warnings = %q", w)
	}
	t.Setenv("PAYOS_CLIENT_ID", "")
	cfg, _ = config.Load()
	if w := strings.Join(cfg.PaymentWarnings(), "|"); !strings.Contains(w, "FAKE") || strings.Contains(w, "missing") {
		t.Errorf("warnings = %q", w)
	}
}
