// Package config loads and validates process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/caarlos0/env/v11"
)

const minSecretBytes = 32

// Config holds all runtime settings.
type Config struct {
	AppEnv         string
	HTTPAddr       string
	DatabaseURL    string
	JWTSecret      []byte
	CodePepper     []byte
	GoogleClientID string
	MailFrom       string
	SMTPHost       string
	SMTPPort       int
	SMTPUser       string
	SMTPPassword   string
	// TrustedProxyHops is how many reverse proxies sit in front of the API
	// (0 = none; X-Forwarded-For is ignored). In prod it must be set
	// explicitly: a wrong 0 behind a load balancer makes every client share
	// the load balancer's rate-limit bucket.
	TrustedProxyHops int
	// S3-compatible object storage (MinIO in dev, S3/R2 in prod).
	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKey       string
	S3SecretKey       string
	S3UsePathStyle    bool
	PublicFileBaseURL string
	// PayOS payment gateway. PaymentProvider is "payos" or "fake".
	PaymentProvider  string
	PayOSClientID    string
	PayOSAPIKey      string
	PayOSChecksumKey string
	PayOSBaseURL     string
	PayOSReturnURL   string
	PayOSCancelURL   string
	// Map provider for charter distances. MapProvider is "goong" or "fake".
	MapProvider  string
	GoongAPIKey  string
	GoongBaseURL string

	trustedProxyHopsSet      bool
	paymentProviderDefaulted bool
}

// PaymentWarnings returns startup warnings about the payment setup: the fake
// provider is active, and PayOS variables that are missing when only some of
// the keys/URLs were provided without an explicit PAYMENT_PROVIDER.
func (c Config) PaymentWarnings() []string {
	if c.PaymentProvider != "fake" {
		return nil
	}
	w := []string{"payment provider is FAKE — webhooks signed with a public test key"}
	var missing []string
	set := 0
	for _, f := range []struct{ name, val string }{
		{"PAYOS_CLIENT_ID", c.PayOSClientID}, {"PAYOS_API_KEY", c.PayOSAPIKey},
		{"PAYOS_CHECKSUM_KEY", c.PayOSChecksumKey},
		{"PAYOS_RETURN_URL", c.PayOSReturnURL}, {"PAYOS_CANCEL_URL", c.PayOSCancelURL},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		} else {
			set++
		}
	}
	if c.paymentProviderDefaulted && set > 0 && len(missing) > 0 {
		w = append(w, "PayOS settings partially configured, falling back to fake; missing: "+strings.Join(missing, ", "))
	}
	return w
}

// MapWarnings returns startup warnings about the map provider setup.
func (c Config) MapWarnings() []string {
	if c.MapProvider == "fake" {
		return []string{"map provider is FAKE — distances are estimates"}
	}
	return nil
}

// rawEnv mirrors the environment; secrets are strings because env would
// parse a []byte as a comma-separated list of numbers.
type rawEnv struct {
	AppEnv            string `env:"APP_ENV" envDefault:"dev"`
	HTTPAddr          string `env:"HTTP_ADDR" envDefault:":8080"`
	DatabaseURL       string `env:"DATABASE_URL,notEmpty"`
	JWTSecret         string `env:"JWT_SECRET,notEmpty"`
	CodePepper        string `env:"CODE_PEPPER,notEmpty"`
	GoogleClientID    string `env:"GOOGLE_CLIENT_ID"`
	MailFrom          string `env:"MAIL_FROM" envDefault:"LunaIn Travel <no-reply@lunain.vn>"`
	SMTPHost          string `env:"SMTP_HOST"`
	SMTPPort          int    `env:"SMTP_PORT"`
	SMTPUser          string `env:"SMTP_USER"`
	SMTPPassword      string `env:"SMTP_PASSWORD"`
	TrustedProxyHops  int    `env:"TRUSTED_PROXY_HOPS" envDefault:"0"`
	S3Endpoint        string `env:"S3_ENDPOINT"`
	S3Region          string `env:"S3_REGION" envDefault:"us-east-1"`
	S3Bucket          string `env:"S3_BUCKET"`
	S3AccessKey       string `env:"S3_ACCESS_KEY"`
	S3SecretKey       string `env:"S3_SECRET_KEY"`
	S3UsePathStyle    bool   `env:"S3_USE_PATH_STYLE" envDefault:"true"`
	PublicFileBaseURL string `env:"PUBLIC_FILE_BASE_URL"`
	PaymentProvider   string `env:"PAYMENT_PROVIDER"`
	PayOSClientID     string `env:"PAYOS_CLIENT_ID"`
	PayOSAPIKey       string `env:"PAYOS_API_KEY"`
	PayOSChecksumKey  string `env:"PAYOS_CHECKSUM_KEY"`
	PayOSBaseURL      string `env:"PAYOS_BASE_URL" envDefault:"https://api-merchant.payos.vn"`
	PayOSReturnURL    string `env:"PAYOS_RETURN_URL"`
	PayOSCancelURL    string `env:"PAYOS_CANCEL_URL"`
	MapProvider       string `env:"MAP_PROVIDER"`
	GoongAPIKey       string `env:"GOONG_API_KEY"`
	GoongBaseURL      string `env:"GOONG_BASE_URL" envDefault:"https://rsapi.goong.io"`
}

// Load reads the environment and validates it.
func Load() (Config, error) {
	var raw rawEnv
	if err := env.Parse(&raw); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	cfg := Config{
		AppEnv:           raw.AppEnv,
		HTTPAddr:         raw.HTTPAddr,
		DatabaseURL:      raw.DatabaseURL,
		JWTSecret:        []byte(raw.JWTSecret),
		CodePepper:       []byte(raw.CodePepper),
		GoogleClientID:   raw.GoogleClientID,
		MailFrom:         raw.MailFrom,
		SMTPHost:         raw.SMTPHost,
		SMTPPort:         raw.SMTPPort,
		SMTPUser:         raw.SMTPUser,
		SMTPPassword:     raw.SMTPPassword,
		TrustedProxyHops: raw.TrustedProxyHops,

		S3Endpoint:        raw.S3Endpoint,
		S3Region:          raw.S3Region,
		S3Bucket:          raw.S3Bucket,
		S3AccessKey:       raw.S3AccessKey,
		S3SecretKey:       raw.S3SecretKey,
		S3UsePathStyle:    raw.S3UsePathStyle,
		PublicFileBaseURL: raw.PublicFileBaseURL,

		PaymentProvider:  raw.PaymentProvider,
		PayOSClientID:    raw.PayOSClientID,
		PayOSAPIKey:      raw.PayOSAPIKey,
		PayOSChecksumKey: raw.PayOSChecksumKey,
		PayOSBaseURL:     raw.PayOSBaseURL,
		PayOSReturnURL:   raw.PayOSReturnURL,
		PayOSCancelURL:   raw.PayOSCancelURL,

		MapProvider:  raw.MapProvider,
		GoongAPIKey:  raw.GoongAPIKey,
		GoongBaseURL: raw.GoongBaseURL,
	}
	if cfg.MapProvider == "" {
		// Unset: goong when keyed, otherwise fake outside prod (prod's missing
		// key is reported by validate).
		if cfg.GoongAPIKey != "" || cfg.AppEnv == "prod" {
			cfg.MapProvider = "goong"
		} else {
			cfg.MapProvider = "fake"
		}
	}
	cfg.paymentProviderDefaulted = cfg.PaymentProvider == ""
	if cfg.PaymentProvider == "" {
		// Unset: use PayOS when fully keyed, otherwise fake outside prod.
		// In prod, missing keys are reported by validate.
		hasKeys := cfg.PayOSClientID != "" && cfg.PayOSAPIKey != "" && cfg.PayOSChecksumKey != "" &&
			cfg.PayOSReturnURL != "" && cfg.PayOSCancelURL != ""
		if hasKeys || cfg.AppEnv == "prod" {
			cfg.PaymentProvider = "payos"
		} else {
			cfg.PaymentProvider = "fake"
		}
	}
	_, cfg.trustedProxyHopsSet = os.LookupEnv("TRUSTED_PROXY_HOPS")
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch c.AppEnv {
	case "dev", "test", "prod":
	default:
		return fmt.Errorf("APP_ENV must be dev, test or prod, got %q", c.AppEnv)
	}
	if len(c.JWTSecret) < minSecretBytes {
		return fmt.Errorf("JWT_SECRET must be at least %d bytes", minSecretBytes)
	}
	if len(c.CodePepper) < minSecretBytes {
		return fmt.Errorf("CODE_PEPPER must be at least %d bytes", minSecretBytes)
	}
	if c.TrustedProxyHops < 0 {
		return errors.New("TRUSTED_PROXY_HOPS must not be negative")
	}
	if c.AppEnv == "prod" && !c.trustedProxyHopsSet {
		return errors.New("TRUSTED_PROXY_HOPS must be set explicitly when APP_ENV=prod (0 if no reverse proxy)")
	}
	if c.AppEnv == "prod" && c.SMTPHost == "" {
		return errors.New("SMTP_HOST is required when APP_ENV=prod")
	}
	if c.AppEnv == "prod" {
		for _, f := range []struct{ name, val string }{
			{"S3_ENDPOINT", c.S3Endpoint}, {"S3_BUCKET", c.S3Bucket},
			{"S3_ACCESS_KEY", c.S3AccessKey}, {"S3_SECRET_KEY", c.S3SecretKey},
			{"PUBLIC_FILE_BASE_URL", c.PublicFileBaseURL},
		} {
			if f.val == "" {
				return fmt.Errorf("%s is required when APP_ENV=prod", f.name)
			}
		}
	}
	switch c.PaymentProvider {
	case "fake":
		if c.AppEnv == "prod" {
			return errors.New("PAYMENT_PROVIDER must be payos when APP_ENV=prod")
		}
	case "payos":
		for _, f := range []struct{ name, val string }{
			{"PAYOS_CLIENT_ID", c.PayOSClientID}, {"PAYOS_API_KEY", c.PayOSAPIKey},
			{"PAYOS_CHECKSUM_KEY", c.PayOSChecksumKey}, {"PAYOS_BASE_URL", c.PayOSBaseURL},
			{"PAYOS_RETURN_URL", c.PayOSReturnURL}, {"PAYOS_CANCEL_URL", c.PayOSCancelURL},
		} {
			if f.val == "" {
				return fmt.Errorf("%s is required when PAYMENT_PROVIDER=payos", f.name)
			}
		}
		if c.AppEnv == "prod" && !strings.HasPrefix(c.PayOSBaseURL, "https://") {
			return errors.New("PAYOS_BASE_URL must start with https:// when APP_ENV=prod")
		}
	default:
		return fmt.Errorf("PAYMENT_PROVIDER must be payos or fake, got %q", c.PaymentProvider)
	}
	switch c.MapProvider {
	case "fake":
		if c.AppEnv == "prod" {
			return errors.New("MAP_PROVIDER must be goong when APP_ENV=prod")
		}
	case "goong":
		if c.GoongAPIKey == "" {
			return errors.New("GOONG_API_KEY is required when MAP_PROVIDER=goong")
		}
		if c.GoongBaseURL == "" {
			return errors.New("GOONG_BASE_URL is required when MAP_PROVIDER=goong")
		}
		if c.AppEnv == "prod" && !strings.HasPrefix(c.GoongBaseURL, "https://") {
			return errors.New("GOONG_BASE_URL must start with https:// when APP_ENV=prod")
		}
	default:
		return fmt.Errorf("MAP_PROVIDER must be goong or fake, got %q", c.MapProvider)
	}
	return nil
}
