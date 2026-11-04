// Package config loads and validates process configuration from the environment.
package config

import (
	"errors"
	"fmt"

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
	// (0 = none; X-Forwarded-For is ignored).
	TrustedProxyHops int
}

// rawEnv mirrors the environment; secrets are strings because env would
// parse a []byte as a comma-separated list of numbers.
type rawEnv struct {
	AppEnv           string `env:"APP_ENV" envDefault:"dev"`
	HTTPAddr         string `env:"HTTP_ADDR" envDefault:":8080"`
	DatabaseURL      string `env:"DATABASE_URL,notEmpty"`
	JWTSecret        string `env:"JWT_SECRET,notEmpty"`
	CodePepper       string `env:"CODE_PEPPER,notEmpty"`
	GoogleClientID   string `env:"GOOGLE_CLIENT_ID"`
	MailFrom         string `env:"MAIL_FROM" envDefault:"LunaIn Travel <no-reply@lunain.vn>"`
	SMTPHost         string `env:"SMTP_HOST"`
	SMTPPort         int    `env:"SMTP_PORT"`
	SMTPUser         string `env:"SMTP_USER"`
	SMTPPassword     string `env:"SMTP_PASSWORD"`
	TrustedProxyHops int    `env:"TRUSTED_PROXY_HOPS" envDefault:"0"`
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
	}
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
	if c.AppEnv == "prod" && c.SMTPHost == "" {
		return errors.New("SMTP_HOST is required when APP_ENV=prod")
	}
	return nil
}
