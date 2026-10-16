// Package auth implements passwordless OTP login, JWT access tokens and
// rotating refresh-token sessions.
package auth

import (
	"context"
	"log/slog"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"

	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

// Purpose scopes an OTP to one login flow; each has its own Redis key.
type Purpose string

const (
	PurposeCustomerLogin Purpose = "customer_login"
	PurposeStaffLogin    Purpose = "staff_login"
)

func (p Purpose) audience() domain.Audience {
	if p == PurposeStaffLogin {
		return domain.AudienceStaff
	}
	return domain.AudienceCustomer
}

var errAuthUnavailable = domain.NewError(domain.CodeAuthUnavailable,
	"Authentication is temporarily unavailable. Try again shortly.")

// unavailable wraps a Redis failure: auth state cannot be checked, so the
// request fails closed.
func unavailable(err error) error {
	return domain.WrapError(domain.CodeAuthUnavailable, errAuthUnavailable.Message, err)
}

// RequestMeta is the caller context recorded on every auth event.
type RequestMeta struct {
	IP        string
	UserAgent string
}

// Subject is a staff member or customer matched by identifier.
type Subject struct {
	ID    string
	Phone string
	Email string
}

type Profile struct {
	ID       string `json:"id"`
	Phone    string `json:"phone,omitempty"`
	Email    string `json:"email,omitempty"`
	FullName string `json:"full_name,omitempty"`
}

type Event struct {
	SubjectType string
	SubjectID   string
	Identifier  string
	Name        string
	DeviceID    string
	Meta        RequestMeta
}

type otpStore interface {
	SaveOTP(ctx context.Context, purpose, identifier, mac string, sentAt time.Time, ttl time.Duration) error
	AttemptOTP(ctx context.Context, purpose, identifier string) (string, int, error)
	DeleteOTP(ctx context.Context, purpose, identifier, mac string) (bool, error)
}

type otpLimiter interface {
	AllowOTPRequest(ctx context.Context, identifier, ip, date string, l credis.OTPLimits) (string, time.Duration, error)
	AllowOTPVerify(ctx context.Context, ip string, limit int, window time.Duration) (time.Duration, error)
}

type sessionStore interface {
	CreateSession(ctx context.Context, hash string, s *credis.Session, ttl time.Duration) error
	GetSession(ctx context.Context, hash string) (*credis.Session, error)
	RotateSession(ctx context.Context, oldHash, newHash string, s *credis.Session, ttl, grace time.Duration, successor []byte) (bool, error)
	GetSuccessor(ctx context.Context, oldHash string) ([]byte, error)
}

type sessionRevoker interface {
	RevokeFamily(ctx context.Context, familyID string) error
	RevokeSubject(ctx context.Context, subjectType, subjectID string) error
	DenyToken(ctx context.Context, jti string, ttl time.Duration) error
}

type subjectRepository interface {
	FindEligibleStaff(ctx context.Context, id domain.Identifier) (*Subject, error)
	FindCustomer(ctx context.Context, id domain.Identifier) (*Subject, error)
	UpsertVerifiedCustomer(ctx context.Context, id domain.Identifier) (string, error)
}

type profileRepository interface {
	FindProfile(ctx context.Context, aud domain.Audience, id string) (*Profile, error)
}

type eventRecorder interface {
	RecordEvent(ctx context.Context, e *Event) error
}

type otpDispatcher interface {
	Dispatch(job *DispatchJob)
}

type Config struct {
	Pepper              []byte
	OTPLength           int
	OTPTTL              time.Duration
	Cooldown            time.Duration
	MaxAttempts         int
	DailyQuota          int
	IPHourlyQuota       int
	VerifyIPHourlyQuota int
	QuotaLocation       *time.Location
	RefreshTTL          map[domain.Audience]time.Duration
	RefreshGrace        time.Duration
}

type Deps struct {
	OTPs       otpStore
	Limiter    otpLimiter
	Sessions   sessionStore
	Revoker    sessionRevoker
	Subjects   subjectRepository
	Profiles   profileRepository
	Events     eventRecorder
	Dispatcher otpDispatcher
	Signer     *Signer
}

type Service struct {
	cfg  Config
	deps Deps
	now  func() time.Time
}

func NewService(cfg Config, deps Deps) *Service {
	return &Service{cfg: cfg, deps: deps, now: time.Now}
}

// Profile returns the caller's own profile.
func (s *Service) Profile(ctx context.Context, p *domain.Principal) (*Profile, error) {
	prof, err := s.deps.Profiles.FindProfile(ctx, p.Audience, p.Subject)
	if err != nil {
		return nil, err
	}
	if prof == nil {
		return nil, domain.ErrNotFound
	}
	return prof, nil
}

// record writes an auth event. A failed write is logged rather than failing
// the request, so the response cannot reveal whether auditing succeeded.
func (s *Service) record(ctx context.Context, e *Event) {
	if err := s.deps.Events.RecordEvent(ctx, e); err != nil {
		slog.ErrorContext(ctx, "record auth event", "event", e.Name, "error", err)
	}
}
