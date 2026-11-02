package config

import (
	"fmt"
	"math"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Env     string
	HTTP    HTTP
	DB      DB
	Redis   Redis
	Auth    Auth
	OTP     OTP
	PIN     PIN
	Catalog Catalog
}

type Catalog struct {
	// PriceMax bounds every catalog price field, in rupiah.
	PriceMax int64
	// CacheTTL bounds how long a cached menu or POS bundle lives. Writes
	// invalidate through the generation counter; the TTL bounds staleness
	// when that bump fails.
	CacheTTL time.Duration
	// PublicRateLimit is the per-IP request budget per minute on the public
	// outlet and menu routes.
	PublicRateLimit int
}

type HTTP struct {
	Port            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
	CORSOrigins     []string
	// TrustedProxies are the only peers whose forwarding headers are believed.
	// Empty means the service is reached directly and headers are ignored.
	TrustedProxies []netip.Prefix
}

type DB struct {
	DSN             string
	MaxConns        int32
	MaxConnIdle     time.Duration
	MaxConnLifetime time.Duration
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
	// AuthzCacheTTL bounds how long cached staff permissions live. Changes
	// invalidate the cache explicitly; the TTL is a backstop.
	AuthzCacheTTL time.Duration
	// DeviceLimit is the number of active POS devices allowed per outlet.
	DeviceLimit int
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
	// LogCodes writes plaintext codes to the log notifier's output, for local
	// development only. Refused in production.
	LogCodes bool
}

// PIN configures the local POS unlock PIN. The hash is verified on the POS
// itself, so the argon2id cost must stay under 500 ms on POS hardware
// (NFR-PERF-4).
type PIN struct {
	// Denylist holds common PINs rejected on top of repeated digits and runs.
	Denylist        []string
	Argon2MemoryKiB uint32
	Argon2Time      uint32
	Argon2Threads   uint8
}

// minPepperLength matches the HMAC-SHA256 output size in bytes.
const minPepperLength = 32

func Load() (*Config, error) {
	_ = godotenv.Load()

	l := &loader{}

	var missing []string
	req := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	cfg := &Config{
		Env: req("APP_ENV"),
		HTTP: HTTP{
			Port:            def("HTTP_PORT", "8080"),
			ReadTimeout:     l.dur("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    l.dur("HTTP_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: l.dur("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
			CORSOrigins:     splitList(def("CORS_ORIGINS", "*")),
			TrustedProxies:  l.prefixes("TRUSTED_PROXIES"),
		},
		DB: DB{
			DSN:             req("DATABASE_URL"),
			MaxConns:        int32(l.integer("DB_MAX_CONNS", 20, 1, math.MaxInt32)),
			MaxConnIdle:     l.dur("DB_MAX_CONN_IDLE", 5*time.Minute),
			MaxConnLifetime: l.dur("DB_MAX_CONN_LIFETIME", time.Hour),
		},
		Redis: Redis{
			Addr:     def("REDIS_ADDR", "localhost:6379"),
			Password: os.Getenv("REDIS_PASSWORD"),
			DB:       l.integer("REDIS_DB", 0, 0, math.MaxInt32),
		},
		Auth: Auth{
			JWTPrivateKeyPEM:        req("JWT_PRIVATE_KEY"),
			JWTPublicKeyPEM:         req("JWT_PUBLIC_KEY"),
			JWTPreviousPublicKeyPEM: os.Getenv("JWT_PREVIOUS_PUBLIC_KEY"),
			Issuer:                  def("JWT_ISSUER", "fnb-backend"),
			AccessTTL:               l.dur("ACCESS_TOKEN_TTL", 15*time.Minute),
			CustomerRefreshTTL:      l.dur("CUSTOMER_REFRESH_TTL", 30*24*time.Hour),
			StaffRefreshTTL:         l.dur("STAFF_REFRESH_TTL", 12*time.Hour),
			DeviceRefreshTTL:        l.dur("DEVICE_REFRESH_TTL", 90*24*time.Hour),
			RefreshGrace:            l.dur("REFRESH_GRACE", 30*time.Second),
			AuthzCacheTTL:           l.dur("AUTHZ_CACHE_TTL", 10*time.Minute),
			DeviceLimit:             l.integer("DEVICE_LIMIT_PER_OUTLET", 1, 1, math.MaxInt32),
		},
		OTP: OTP{
			Pepper:              req("OTP_PEPPER"),
			Length:              l.integer("OTP_LENGTH", 6, 4, 10),
			TTL:                 l.dur("OTP_TTL", 5*time.Minute),
			Cooldown:            l.dur("OTP_COOLDOWN", 60*time.Second),
			MaxAttempts:         l.integer("OTP_MAX_ATTEMPTS", 5, 1, math.MaxInt32),
			DailyQuota:          l.integer("OTP_DAILY_QUOTA", 5, 1, math.MaxInt32),
			IPHourlyQuota:       l.integer("OTP_IP_HOURLY_QUOTA", 20, 1, math.MaxInt32),
			VerifyIPHourlyQuota: l.integer("OTP_VERIFY_IP_HOURLY_QUOTA", 30, 1, math.MaxInt32),
			QuotaTimezone:       def("OTP_QUOTA_TIMEZONE", "Asia/Jakarta"),
			DispatchRetryWindow: l.dur("OTP_DISPATCH_RETRY_WINDOW", 2*time.Minute),
			LogCodes:            l.boolean("OTP_LOG_CODES"),
		},
		PIN: PIN{
			Denylist: splitList(def("PIN_DENYLIST",
				"121212,112233,123123,111222,123321,101010,696969,000123")),
			// OWASP's minimum argon2id profile (19 MiB, 2 passes).
			Argon2MemoryKiB: uint32(l.integer("PIN_ARGON2_MEMORY_KIB", 19456, 1, math.MaxUint32)),
			Argon2Time:      uint32(l.integer("PIN_ARGON2_TIME", 2, 1, math.MaxUint32)),
			Argon2Threads:   uint8(l.integer("PIN_ARGON2_THREADS", 1, 1, math.MaxUint8)),
		},
	}

	cfg.Catalog = Catalog{
		PriceMax:        int64(l.integer("CATALOG_PRICE_MAX", 100_000_000, 1, math.MaxInt)),
		CacheTTL:        l.dur("CATALOG_CACHE_TTL", 5*time.Minute),
		PublicRateLimit: l.integer("CATALOG_PUBLIC_RATE_LIMIT", 120, 1, math.MaxInt32),
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if problems := append(l.problems, cfg.unsafeSettings()...); len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}

// unsafeSettings reports valid values that must not be combined. Messages name
// the variable only, so a secret never reaches the startup log.
func (c *Config) unsafeSettings() []string {
	var problems []string
	if len(c.OTP.Pepper) < minPepperLength {
		problems = append(problems, fmt.Sprintf("OTP_PEPPER must be at least %d characters", minPepperLength))
	}
	if !c.IsProduction() {
		return problems
	}
	if c.OTP.LogCodes {
		problems = append(problems, "OTP_LOG_CODES must not be enabled in production")
	}
	for _, origin := range c.HTTP.CORSOrigins {
		if origin == "*" {
			problems = append(problems, "CORS_ORIGINS must list explicit origins in production")
			break
		}
	}
	return problems
}

func (c *Config) IsProduction() bool { return c.Env == "production" }

func def(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loader reads typed variables and collects every bad value, so one startup
// failure reports them all. An unset or empty variable takes its default.
type loader struct {
	problems []string
}

func (l *loader) fail(key, reason string) {
	l.problems = append(l.problems, key+" "+reason)
}

func (l *loader) integer(key string, fallback, minVal, maxVal int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.fail(key, "must be an integer")
		return fallback
	}
	if v < minVal || v > maxVal {
		l.fail(key, fmt.Sprintf("must be between %d and %d", minVal, maxVal))
		return fallback
	}
	return v
}

func (l *loader) dur(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.fail(key, "must be a duration such as 30s or 5m")
		return fallback
	}
	if v < 0 {
		l.fail(key, "must not be negative")
		return fallback
	}
	return v
}

func (l *loader) boolean(key string) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return false
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		l.fail(key, "must be true or false")
		return false
	}
	return v
}

func (l *loader) prefixes(key string) []netip.Prefix {
	var out []netip.Prefix
	for _, raw := range splitList(os.Getenv(key)) {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			l.fail(key, "must be a comma-separated list of CIDRs")
			return nil
		}
		out = append(out, p.Masked())
	}
	return out
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
