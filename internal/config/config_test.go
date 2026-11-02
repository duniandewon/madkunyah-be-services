package config

import (
	"strings"
	"testing"
)

const testPepper = "0123456789abcdef0123456789abcdef"

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_PRIVATE_KEY", "k")
	t.Setenv("JWT_PUBLIC_KEY", "k")
	t.Setenv("OTP_PEPPER", testPepper)
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OTP.LogCodes {
		t.Error("OTP codes must not be logged by default")
	}
	if len(cfg.HTTP.TrustedProxies) != 0 {
		t.Error("no proxy may be trusted by default")
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing APP_ENV", map[string]string{"APP_ENV": ""}, "APP_ENV"},
		{"production wildcard CORS by default", map[string]string{"APP_ENV": "production"}, "CORS_ORIGINS"},
		{"production explicit wildcard CORS", map[string]string{"APP_ENV": "production", "CORS_ORIGINS": "https://a.example,*"}, "CORS_ORIGINS"},
		{"production OTP logging", map[string]string{"APP_ENV": "production", "CORS_ORIGINS": "https://a.example", "OTP_LOG_CODES": "true"}, "OTP_LOG_CODES"},
		{"bad bool", map[string]string{"OTP_LOG_CODES": "yes please"}, "OTP_LOG_CODES"},
		{"bad duration", map[string]string{"OTP_TTL": "5min"}, "OTP_TTL"},
		{"negative duration", map[string]string{"OTP_TTL": "-5m"}, "OTP_TTL"},
		{"bad integer", map[string]string{"OTP_MAX_ATTEMPTS": "many"}, "OTP_MAX_ATTEMPTS"},
		{"threads overflow uint8", map[string]string{"PIN_ARGON2_THREADS": "256"}, "PIN_ARGON2_THREADS"},
		{"zero threads", map[string]string{"PIN_ARGON2_THREADS": "0"}, "PIN_ARGON2_THREADS"},
		{"short OTP", map[string]string{"OTP_LENGTH": "3"}, "OTP_LENGTH"},
		{"long OTP", map[string]string{"OTP_LENGTH": "11"}, "OTP_LENGTH"},
		{"short pepper", map[string]string{"OTP_PEPPER": "short"}, "OTP_PEPPER"},
		{"bad CIDR", map[string]string{"TRUSTED_PROXIES": "10.0.0.1"}, "TRUSTED_PROXIES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequired(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %s", err, tt.want)
			}
		})
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	setRequired(t)
	t.Setenv("OTP_TTL", "5min")
	t.Setenv("OTP_LENGTH", "3")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OTP_TTL") || !strings.Contains(err.Error(), "OTP_LENGTH") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAcceptsProductionAndProxies(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("CORS_ORIGINS", "https://a.example")
	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.5/32")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HTTP.TrustedProxies) != 2 {
		t.Errorf("proxies = %v", cfg.HTTP.TrustedProxies)
	}
}
