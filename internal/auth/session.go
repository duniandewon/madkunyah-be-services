package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"

	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

var errSessionRevoked = domain.NewError(domain.CodeSessionRevoked,
	"Session has ended. Sign in again.")

// TokenPair is what a successful login or refresh returns.
type TokenPair struct {
	Audience         domain.Audience `json:"aud"`
	AccessToken      string          `json:"access_token"`
	AccessExpiresAt  time.Time       `json:"access_expires_at"`
	RefreshToken     string          `json:"refresh_token"`
	RefreshExpiresAt time.Time       `json:"refresh_expires_at"`
}

// successor is kept for the grace window after a rotation so a client that
// lost the response can retry and receive the same pair.
type successor struct {
	Pair        TokenPair `json:"pair"`
	Fingerprint string    `json:"fp"`
}

type newSession struct {
	Audience   domain.Audience
	SubjectID  string
	Identifier string
	DeviceID   string
	OutletID   string
	Meta       RequestMeta
}

// startSession opens a new refresh-token family.
func (s *Service) startSession(ctx context.Context, ns *newSession) (*TokenPair, error) {
	now := s.now()
	sess := &credis.Session{
		SubjectID:       ns.SubjectID,
		SubjectType:     string(ns.Audience),
		Identifier:      ns.Identifier,
		DeviceID:        ns.DeviceID,
		OutletID:        ns.OutletID,
		FamilyID:        uuid.NewString(),
		IssuedAt:        now,
		FamilyStartedAt: now,
	}
	ttl := s.cfg.RefreshTTL[ns.Audience]
	pair, hash, err := s.mint(sess, now.Add(ttl))
	if err != nil {
		return nil, err
	}
	if err := s.deps.Sessions.CreateSession(ctx, hash, sess, ttl); err != nil {
		return nil, unavailable(err)
	}
	return pair, nil
}

// mint issues an access token and a fresh opaque refresh token for sess.
func (s *Service) mint(sess *credis.Session, refreshExp time.Time) (*TokenPair, string, error) {
	aud := domain.Audience(sess.SubjectType)
	access, accessExp, err := s.deps.Signer.Issue(aud, sess.SubjectID, sess.FamilyID, sess.OutletID, sess.DeviceID)
	if err != nil {
		return nil, "", err
	}
	refresh, err := newRefreshToken()
	if err != nil {
		return nil, "", err
	}
	return &TokenPair{
		Audience:         aud,
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
	}, hashToken(refresh), nil
}

// Refresh rotates a refresh token. Presenting an already-rotated token
// revokes the whole family, unless it is a retry inside the grace window from
// the same client. expected, when set, is the audience the caller asked for.
func (s *Service) Refresh(ctx context.Context, raw string, expected domain.Audience, meta RequestMeta) (*TokenPair, error) {
	if raw == "" {
		return nil, errSessionRevoked
	}
	hash := hashToken(raw)
	// A second pass handles losing a rotation race: the re-read then sees the
	// token as rotated and takes the grace path.
	for range 2 {
		sess, err := s.deps.Sessions.GetSession(ctx, hash)
		if errors.Is(err, credis.ErrNotFound) {
			return nil, s.missingSession(ctx, hash)
		}
		if err != nil {
			return nil, unavailable(err)
		}
		if expected != "" && sess.SubjectType != string(expected) {
			return nil, errSessionRevoked
		}
		if err := s.checkDevice(ctx, sess); err != nil {
			return nil, err
		}
		if sess.Rotated {
			return s.handleRotated(ctx, hash, sess, meta)
		}
		pair, rotated, err := s.rotate(ctx, hash, sess, meta)
		if err != nil || rotated {
			return pair, err
		}
	}
	return nil, errSessionRevoked
}

func (s *Service) rotate(ctx context.Context, hash string, sess *credis.Session, meta RequestMeta) (*TokenPair, bool, error) {
	now := s.now()
	ttl, ok := s.remaining(sess, now)
	if !ok {
		return nil, false, errSessionRevoked
	}
	next := *sess
	next.IssuedAt = now
	next.Rotated = false
	pair, newHash, err := s.mint(&next, now.Add(ttl))
	if err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(successor{Pair: *pair, Fingerprint: fingerprint(sess, meta)})
	if err != nil {
		return nil, false, fmt.Errorf("encode successor: %w", err)
	}
	rotated, err := s.deps.Sessions.RotateSession(ctx, hash, newHash, &next, ttl, s.cfg.RefreshGrace, payload)
	if err != nil {
		return nil, false, unavailable(err)
	}
	if rotated {
		s.record(ctx, sessionEvent(sess, "token.refreshed", meta))
		s.touchDevice(ctx, sess)
	}
	return pair, rotated, nil
}

// missingSession explains an unknown refresh token: device_revoked when its
// device was revoked (FR-DEV-5), session_revoked otherwise.
func (s *Service) missingSession(ctx context.Context, hash string) error {
	revoked, err := s.deps.Sessions.IsDeviceRevoked(ctx, hash)
	if err != nil {
		return unavailable(err)
	}
	if revoked {
		return domain.ErrDeviceRevoked
	}
	return errSessionRevoked
}

// checkDevice refuses a POS session whose device is no longer active at its
// outlet. Postgres is the source of truth, so a revocation whose Redis step
// failed still takes effect here, and the leftover sessions are cleared.
func (s *Service) checkDevice(ctx context.Context, sess *credis.Session) error {
	if sess.SubjectType != string(domain.AudiencePOS) {
		return nil
	}
	active, err := s.deps.Tracker.DeviceActive(ctx, sess.DeviceID, sess.OutletID)
	if err != nil {
		return err
	}
	if active {
		return nil
	}
	if err := s.deps.Revoker.RevokeDevice(ctx, sess.DeviceID); err != nil {
		slog.ErrorContext(ctx, "revoke sessions of inactive device", "device_id", sess.DeviceID, "error", err)
	}
	return domain.ErrDeviceRevoked
}

// touchDevice records a successful POS refresh (FR-DEV-8). A failure is
// logged: last_seen_at is informational and must not block the POS.
func (s *Service) touchDevice(ctx context.Context, sess *credis.Session) {
	if sess.SubjectType != string(domain.AudiencePOS) {
		return
	}
	if err := s.deps.Tracker.TouchDevice(ctx, sess.DeviceID); err != nil {
		slog.ErrorContext(ctx, "update device last_seen_at", "device_id", sess.DeviceID, "error", err)
	}
}

func (s *Service) handleRotated(ctx context.Context, hash string, sess *credis.Session, meta RequestMeta) (*TokenPair, error) {
	payload, err := s.deps.Sessions.GetSuccessor(ctx, hash)
	if err != nil && !errors.Is(err, credis.ErrNotFound) {
		return nil, unavailable(err)
	}
	if err == nil {
		var succ successor
		if json.Unmarshal(payload, &succ) == nil && succ.Fingerprint == fingerprint(sess, meta) {
			return &succ.Pair, nil
		}
	}
	// Reuse of a rotated token is a theft signal: end the whole family.
	if err := s.deps.Revoker.RevokeFamily(ctx, sess.FamilyID); err != nil {
		return nil, unavailable(err)
	}
	slog.WarnContext(ctx, "refresh token reuse detected",
		"subject_type", sess.SubjectType, "family_id", sess.FamilyID)
	s.record(ctx, sessionEvent(sess, "token.reuse_detected", meta))
	return nil, errSessionRevoked
}

// remaining is the TTL for the next token in the family: a fresh full window
// for sliding (POS) sessions, the rest of the absolute window otherwise.
func (s *Service) remaining(sess *credis.Session, now time.Time) (time.Duration, bool) {
	aud := domain.Audience(sess.SubjectType)
	lifetime := s.cfg.RefreshTTL[aud]
	if aud == domain.AudiencePOS {
		return lifetime, true
	}
	ttl := sess.FamilyStartedAt.Add(lifetime).Sub(now)
	return ttl, ttl > 0
}

// Logout ends the session behind a refresh token and blocks the presented
// access token. It succeeds even when there is nothing to revoke.
func (s *Service) Logout(ctx context.Context, rawRefresh, bearer string, meta RequestMeta) error {
	allowed := []domain.Audience{domain.AudienceCustomer, domain.AudienceStaff}
	if rawRefresh != "" {
		sess, err := s.deps.Sessions.GetSession(ctx, hashToken(rawRefresh))
		switch {
		case err == nil:
			if err := s.deps.Revoker.RevokeFamily(ctx, sess.FamilyID); err != nil {
				return unavailable(err)
			}
			s.record(ctx, sessionEvent(sess, "session.revoked", meta))
			allowed = []domain.Audience{domain.Audience(sess.SubjectType)}
		case !errors.Is(err, credis.ErrNotFound):
			return unavailable(err)
		}
	}
	if bearer == "" {
		return nil
	}
	p, err := s.deps.Signer.Verify(bearer, allowed...)
	if err != nil {
		return nil
	}
	return s.denyAccessToken(ctx, p)
}

// RevokeAll ends every session the caller holds, on every device.
func (s *Service) RevokeAll(ctx context.Context, p *domain.Principal, meta RequestMeta) error {
	if err := s.deps.Revoker.RevokeSubject(ctx, string(p.Audience), p.Subject); err != nil {
		return unavailable(err)
	}
	if err := s.denyAccessToken(ctx, p); err != nil {
		return err
	}
	prof, err := s.deps.Profiles.FindProfile(ctx, p.Audience, p.Subject)
	if err != nil {
		return err
	}
	if prof != nil {
		identifier := prof.Phone
		if identifier == "" {
			identifier = prof.Email
		}
		s.record(ctx, &Event{SubjectType: string(p.Audience), SubjectID: p.Subject,
			Identifier: identifier, Name: "session.revoked", Meta: meta})
	}
	return nil
}

func (s *Service) denyAccessToken(ctx context.Context, p *domain.Principal) error {
	if err := s.deps.Revoker.DenyToken(ctx, p.TokenID, p.ExpiresAt.Sub(s.now())); err != nil {
		return unavailable(err)
	}
	return nil
}

func sessionEvent(sess *credis.Session, name string, meta RequestMeta) *Event {
	return &Event{SubjectType: eventSubjectType(sess.SubjectType), SubjectID: sess.SubjectID,
		Identifier: sess.Identifier, Name: name, DeviceID: sess.DeviceID, Meta: meta}
}

// eventSubjectType maps a session audience to an auth_events subject type:
// a POS session's subject is its device.
func eventSubjectType(audience string) string {
	if audience == string(domain.AudiencePOS) {
		return subjectDevice
	}
	return audience
}

// fingerprint identifies the client for the grace window: the device for a
// POS session, the user agent otherwise.
func fingerprint(sess *credis.Session, meta RequestMeta) string {
	if sess.DeviceID != "" {
		return "dev:" + sess.DeviceID
	}
	sum := sha256.Sum256([]byte(meta.UserAgent))
	return "ua:" + hex.EncodeToString(sum[:])
}

func newRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is the only form of a refresh token that is ever stored.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
