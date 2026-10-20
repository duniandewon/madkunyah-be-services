package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"

	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

var errOTPInvalid = domain.NewError(domain.CodeOTPInvalid,
	"Code is invalid or has expired. Request a new code.")

type OTPRequest struct {
	Identifier string
	Purpose    Purpose
	// OutletID is the outlet a device_enroll code is requested for.
	OutletID string
}

type OTPVerify struct {
	Identifier string
	Purpose    Purpose
	Code       string
}

// RequestOTP issues a code if the identifier is eligible. Apart from format
// and rate-limit errors, it returns nil whether or not anything was sent, and
// takes the same path either way: limits, lookup, store, record.
func (s *Service) RequestOTP(ctx context.Context, in OTPRequest, meta RequestMeta) error {
	id, err := domain.ParseIdentifier(in.Identifier)
	if err != nil {
		return err
	}
	now := s.now()
	quotaDay := now.In(s.cfg.QuotaLocation).Format(time.DateOnly)

	// Limits run before the existence lookup so they behave identically for
	// known and unknown identifiers.
	if err := s.checkRequestLimits(ctx, id, meta.IP, quotaDay, now); err != nil {
		return err
	}

	subject, err := s.lookupSubject(ctx, in.Purpose, id, in.OutletID)
	if err != nil {
		return err
	}
	code, err := generateCode(s.cfg.OTPLength)
	if err != nil {
		return err
	}
	mac := s.codeMAC(in.Purpose, id, "", code)
	decoy := in.Purpose.isStaff() && subject == nil
	if decoy {
		// A decoy record counts attempts and expires like a real one but can
		// never verify, so verify reveals nothing either.
		if mac, err = randomHex(sha256.Size); err != nil {
			return err
		}
	}
	if err := s.deps.OTPs.SaveOTP(ctx, string(in.Purpose), id.Value, mac, now, s.cfg.OTPTTL); err != nil {
		return unavailable(err)
	}

	s.record(ctx, &Event{SubjectType: string(in.Purpose.audience()), SubjectID: subjectID(subject),
		Identifier: id.Value, Name: "otp.requested", Meta: meta})
	if !decoy {
		s.deps.Dispatcher.Dispatch(&DispatchJob{To: id, Code: code, Fallback: fallbackEmail(id, subject), QuotaDay: quotaDay})
	}
	return nil
}

func (s *Service) checkRequestLimits(ctx context.Context, id domain.Identifier, ip, quotaDay string, now time.Time) error {
	reason, wait, err := s.deps.Limiter.AllowOTPRequest(ctx, id.Value, ip, quotaDay, credis.OTPLimits{
		IPLimit:    s.cfg.IPHourlyQuota,
		IPWindow:   time.Hour,
		Cooldown:   s.cfg.Cooldown,
		DailyQuota: s.cfg.DailyQuota,
		QuotaTTL:   48 * time.Hour,
	})
	if err != nil {
		return unavailable(err)
	}
	switch reason {
	case credis.LimitCooldown:
		return domain.RateLimitError(domain.CodeOTPCooldown, "Please wait before requesting another code.", wait)
	case credis.LimitIP:
		return domain.RateLimitError(domain.CodeOTPQuotaExceeded, "Too many code requests. Try again later.", wait)
	case credis.LimitQuota:
		return domain.RateLimitError(domain.CodeOTPQuotaExceeded, "Too many code requests. Try again later.",
			untilNextDay(now, s.cfg.QuotaLocation))
	}
	return nil
}

// lookupSubject returns nil (no error) when the identifier is unknown or, for
// staff, inactive or without any role. For device_enroll the staff member
// must also hold device.enroll at outletID.
func (s *Service) lookupSubject(ctx context.Context, purpose Purpose, id domain.Identifier, outletID string) (*Subject, error) {
	switch purpose {
	case PurposeStaffLogin:
		return s.deps.Subjects.FindEligibleStaff(ctx, id)
	case PurposeDeviceEnroll:
		return s.findEnroller(ctx, id, outletID)
	}
	return s.deps.Subjects.FindCustomer(ctx, id)
}

// findEnroller reads permissions straight from Postgres, not the authz
// cache: enrollment hands out a 90-day credential.
func (s *Service) findEnroller(ctx context.Context, id domain.Identifier, outletID string) (*Subject, error) {
	staff, err := s.deps.Subjects.FindEligibleStaff(ctx, id)
	if err != nil || staff == nil {
		return nil, err
	}
	access, err := s.deps.Access.Load(ctx, staff.ID)
	if err != nil {
		return nil, err
	}
	if access == nil || !access.Can(outletID, domain.PermDeviceEnroll) {
		return nil, nil
	}
	return staff, nil
}

// VerifyOTP checks a code and, on success, starts a session. Every failure
// cause returns the same otp_invalid error.
func (s *Service) VerifyOTP(ctx context.Context, in OTPVerify, meta RequestMeta) (*TokenPair, error) {
	if !in.Purpose.opensSession() {
		return nil, errOTPInvalid
	}
	id, err := domain.ParseIdentifier(in.Identifier)
	if err != nil {
		return nil, err
	}
	if err := s.allowVerify(ctx, meta.IP); err != nil {
		return nil, err
	}

	ok, err := s.checkCode(ctx, in.Purpose, id, "", in.Code)
	if err != nil {
		return nil, unavailable(err)
	}
	verifiedID := ""
	if ok {
		verifiedID, err = s.resolveVerifiedSubject(ctx, in.Purpose, id)
		if err != nil {
			return nil, err
		}
	}

	ev := &Event{SubjectType: string(in.Purpose.audience()), SubjectID: verifiedID, Identifier: id.Value, Meta: meta}
	if verifiedID == "" {
		ev.Name = "otp.failed"
		if subject, err := s.lookupSubject(ctx, in.Purpose, id, ""); err == nil {
			ev.SubjectID = subjectID(subject)
		}
		s.record(ctx, ev)
		return nil, errOTPInvalid
	}
	ev.Name = "otp.verified"
	s.record(ctx, ev)
	return s.startSession(ctx, &newSession{Audience: in.Purpose.audience(), SubjectID: verifiedID,
		Identifier: id.Value, Meta: meta})
}

// allowVerify applies the per-IP limit on code submissions (FR-VER-5).
func (s *Service) allowVerify(ctx context.Context, ip string) error {
	wait, err := s.deps.Limiter.AllowOTPVerify(ctx, ip, s.cfg.VerifyIPHourlyQuota, time.Hour)
	if err != nil {
		return unavailable(err)
	}
	if wait > 0 {
		return domain.RateLimitError(domain.CodeRateLimited,
			"Too many verification attempts. Try again later.", wait)
	}
	return nil
}

// resolveVerifiedSubject returns the subject to open a session for, or "" if
// a staff member stopped being eligible after the code was sent.
func (s *Service) resolveVerifiedSubject(ctx context.Context, purpose Purpose, id domain.Identifier) (string, error) {
	if purpose == PurposeCustomerLogin {
		// Guest-order linking (FR-CUS-1) lands with the orders feature.
		return s.deps.Subjects.UpsertVerifiedCustomer(ctx, id)
	}
	staff, err := s.deps.Subjects.FindEligibleStaff(ctx, id)
	if err != nil || staff == nil {
		return "", err
	}
	return staff.ID, nil
}

// checkCode counts the attempt, compares in constant time, and consumes the
// record on success.
func (s *Service) checkCode(ctx context.Context, purpose Purpose, id domain.Identifier, bind, code string) (bool, error) {
	stored, match, err := s.matchCode(ctx, purpose, id, bind, code)
	if err != nil || !match {
		return false, err
	}
	return s.consumeCode(ctx, purpose, id, stored)
}

// matchCode counts the attempt and compares in constant time, deleting the
// record once attempts are exhausted. A match is not consumed: the caller
// must call consumeCode before acting on it.
func (s *Service) matchCode(ctx context.Context, purpose Purpose, id domain.Identifier, bind, code string) (string, bool, error) {
	stored, attempts, err := s.deps.OTPs.AttemptOTP(ctx, string(purpose), id.Value)
	if errors.Is(err, credis.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	match := attempts <= s.cfg.MaxAttempts &&
		hmac.Equal([]byte(stored), []byte(s.codeMAC(purpose, id, bind, code)))
	if !match && attempts >= s.cfg.MaxAttempts {
		_, err := s.deps.OTPs.DeleteOTP(ctx, string(purpose), id.Value, stored)
		return "", false, err
	}
	return stored, match, nil
}

// consumeCode deletes a matched record. Of concurrent correct submissions
// exactly one wins, because only one caller's conditional delete removes it.
func (s *Service) consumeCode(ctx context.Context, purpose Purpose, id domain.Identifier, stored string) (bool, error) {
	return s.deps.OTPs.DeleteOTP(ctx, string(purpose), id.Value, stored)
}

// codeMAC binds the code to its purpose and identifier, so a code issued for
// one flow can never be replayed against another. A non-empty bind also ties
// it to one subject, so only the requester can redeem it.
func (s *Service) codeMAC(purpose Purpose, id domain.Identifier, bind, code string) string {
	msg := string(purpose) + "\x00" + id.Value
	if bind != "" {
		msg += "\x00" + bind
	}
	m := hmac.New(sha256.New, s.cfg.Pepper)
	m.Write([]byte(msg + "\x00" + code))
	return hex.EncodeToString(m.Sum(nil))
}

// generateCode returns a uniformly distributed numeric code, leading zeros
// allowed.
func generateCode(length int) (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(length)), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("generate otp: %w", err)
	}
	return fmt.Sprintf("%0*d", length, n), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func untilNextDay(now time.Time, loc *time.Location) time.Duration {
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
	return next.Sub(now)
}

func subjectID(s *Subject) string {
	if s == nil {
		return ""
	}
	return s.ID
}

// fallbackEmail is the email to try if WhatsApp delivery fails. Unknown
// identifiers have no fallback.
func fallbackEmail(id domain.Identifier, s *Subject) string {
	if s == nil || id.Kind != domain.IdentifierPhone {
		return ""
	}
	return s.Email
}
