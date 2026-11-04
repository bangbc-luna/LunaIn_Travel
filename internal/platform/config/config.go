// Package config loads and validates process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"

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

	trustedProxyHopsSet bool
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
	return nil
}
