package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Env   string
	HTTP  HTTP
	DB    DB
	Redis Redis
	Auth  Auth
	OTP   OTP
}

type HTTP struct {
	Port            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
	CORSOrigins     []string
}

type DB struct {
	DSN         string
	MaxConns    int32
	MaxConnIdle time.Duration
}

type Redis struct {
	Addr     string
	Password string
	DB       int
}

type Auth struct {
	// JWTPrivateKeyPEM / JWTPublicKeyPEM hold PEM-encoded ECDSA P-256 keys.
	// Asymmetric signing means other services can verify tokens without holding
	// the ability to mint them.
	JWTPrivateKeyPEM string
	JWTPublicKeyPEM  string
	// JWTPreviousPublicKeyPEM is the retired verification key during a key
	// rotation. Keep it set for at least twice the access-token lifetime so
	// tokens signed before the switch still validate.
	JWTPreviousPublicKeyPEM string
	Issuer                  string

	AccessTTL          time.Duration
	CustomerRefreshTTL time.Duration
	StaffRefreshTTL    time.Duration
	DeviceRefreshTTL   time.Duration
	// RefreshGrace lets a client that lost a refresh response retry with the
	// old token and receive the same successor, instead of tripping reuse
	// detection.
	RefreshGrace time.Duration
}

type OTP struct {
	// Pepper is the server-side HMAC key. Codes are never stored in plaintext:
	// Redis snapshots and debug dumps would otherwise contain live credentials
	// for the whole TTL window.
	Pepper              string
	Length              int
	TTL                 time.Duration
	Cooldown            time.Duration
	MaxAttempts         int
	DailyQuota          int
	IPHourlyQuota       int
	VerifyIPHourlyQuota int
	// QuotaTimezone defines the calendar day the daily quota resets on.
	QuotaTimezone string
	// DispatchRetryWindow bounds notifier retries; it must stay well inside
	// TTL so a late delivery is still usable.
	DispatchRetryWindow time.Duration
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	var missing []string
	req := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	cfg := &Config{
		Env: def("APP_ENV", "development"),
		HTTP: HTTP{
			Port:            def("HTTP_PORT", "8080"),
			ReadTimeout:     durDef("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    durDef("HTTP_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: durDef("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
			CORSOrigins:     splitList(def("CORS_ORIGINS", "*")),
		},
		DB: DB{
			DSN:         req("DATABASE_URL"),
			MaxConns:    int32(intDef("DB_MAX_CONNS", 20)),
			MaxConnIdle: durDef("DB_MAX_CONN_IDLE", 5*time.Minute),
		},
		Redis: Redis{
			Addr:     def("REDIS_ADDR", "localhost:6379"),
			Password: os.Getenv("REDIS_PASSWORD"),
			DB:       intDef("REDIS_DB", 0),
		},
		Auth: Auth{
			JWTPrivateKeyPEM:        req("JWT_PRIVATE_KEY"),
			JWTPublicKeyPEM:         req("JWT_PUBLIC_KEY"),
			JWTPreviousPublicKeyPEM: os.Getenv("JWT_PREVIOUS_PUBLIC_KEY"),
			Issuer:                  def("JWT_ISSUER", "fnb-backend"),
			AccessTTL:               durDef("ACCESS_TOKEN_TTL", 15*time.Minute),
			CustomerRefreshTTL:      durDef("CUSTOMER_REFRESH_TTL", 30*24*time.Hour),
			StaffRefreshTTL:         durDef("STAFF_REFRESH_TTL", 12*time.Hour),
			DeviceRefreshTTL:        durDef("DEVICE_REFRESH_TTL", 90*24*time.Hour),
			RefreshGrace:            durDef("REFRESH_GRACE", 30*time.Second),
		},
		OTP: OTP{
			Pepper:              req("OTP_PEPPER"),
			Length:              intDef("OTP_LENGTH", 6),
			TTL:                 durDef("OTP_TTL", 5*time.Minute),
			Cooldown:            durDef("OTP_COOLDOWN", 60*time.Second),
			MaxAttempts:         intDef("OTP_MAX_ATTEMPTS", 5),
			DailyQuota:          intDef("OTP_DAILY_QUOTA", 5),
			IPHourlyQuota:       intDef("OTP_IP_HOURLY_QUOTA", 20),
			VerifyIPHourlyQuota: intDef("OTP_VERIFY_IP_HOURLY_QUOTA", 30),
			QuotaTimezone:       def("OTP_QUOTA_TIMEZONE", "Asia/Jakarta"),
			DispatchRetryWindow: durDef("OTP_DISPATCH_RETRY_WINDOW", 2*time.Minute),
		},
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

func (c *Config) IsProduction() bool { return c.Env == "production" }

func def(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intDef(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return fallback
}

func durDef(key string, fallback time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return fallback
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
