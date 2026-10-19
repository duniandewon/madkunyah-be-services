package staff

import (
	"context"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
)

const errAuthUnavailable = "Change saved, but sessions could not be updated. Retry the request."

type repository interface {
	ListOutletStaff(ctx context.Context, outletID string) ([]Member, error)
	Create(ctx context.Context, actorID, outletID string, m *Member) error
	GrantOutletRole(ctx context.Context, actorID, staffID string, g grant) (bool, error)
	RevokeOutletRole(ctx context.Context, actorID, staffID string, g grant) (bool, error)
	GrantGlobalRole(ctx context.Context, actorID, staffID, role string) (bool, error)
	RevokeGlobalRole(ctx context.Context, actorID, staffID, role string) (bool, error)
	Deactivate(ctx context.Context, actorID, staffID string) (bool, error)
	SetPIN(ctx context.Context, staffID, hash string, meta RequestMeta) (bool, error)
	ResetPIN(ctx context.Context, actorID, staffID string, meta RequestMeta) error
}

type accessStore interface {
	Load(ctx context.Context, staffID string) (*domain.StaffAccess, error)
	Invalidate(ctx context.Context, staffID string) error
}

type sessionRevoker interface {
	RevokeSubject(ctx context.Context, subjectType, subjectID string) error
}

type Service struct {
	repo     repository
	access   accessStore
	sessions sessionRevoker
	pin      PINConfig
}

func NewService(repo repository, access accessStore, sessions sessionRevoker, pin PINConfig) *Service {
	return &Service{repo: repo, access: access, sessions: sessions, pin: pin}
}

// RequestMeta is the caller context recorded on PIN auth events.
type RequestMeta struct {
	IP        string
	UserAgent string
}

type CreateInput struct {
	FullName string
	Phone    string
	Email    string
	Role     string
}

// List returns staff with a role at the outlet. The route has already
// checked staff.manage there.
func (s *Service) List(ctx context.Context, outletID string) ([]Member, error) {
	return s.repo.ListOutletStaff(ctx, outletID)
}

// Create adds a staff member with an initial role at the outlet. They can
// sign in with OTP as soon as this returns.
func (s *Service) Create(ctx context.Context, actor *domain.StaffAccess, outletID string, in CreateInput) (*Member, error) {
	if err := canAssign(actor, outletID, in.Role); err != nil {
		return nil, err
	}
	m := &Member{ID: uuid.NewString(), FullName: in.FullName, IsActive: true, Roles: []string{in.Role}}
	var err error
	if m.Phone, err = parseAs(in.Phone, domain.IdentifierPhone, "phone"); err != nil {
		return nil, err
	}
	if m.Email, err = parseAs(in.Email, domain.IdentifierEmail, "email"); err != nil {
		return nil, err
	}
	if m.Phone == "" && m.Email == "" {
		return nil, domain.ValidationError(map[string]string{"phone": "phone or email is required"})
	}
	if err := s.repo.Create(ctx, actor.StaffID, outletID, m); err != nil {
		return nil, err
	}
	return m, nil
}

// GrantRole grants an outlet role. Granting a role already held succeeds.
func (s *Service) GrantRole(ctx context.Context, actor *domain.StaffAccess, staffID string, g grant) error {
	if err := s.checkRoleChange(actor, staffID, g); err != nil {
		return err
	}
	changed, err := s.repo.GrantOutletRole(ctx, actor.StaffID, staffID, g)
	return s.afterRoleChange(ctx, staffID, changed, err, true)
}

func (s *Service) RevokeRole(ctx context.Context, actor *domain.StaffAccess, staffID string, g grant) error {
	if err := s.checkRoleChange(actor, staffID, g); err != nil {
		return err
	}
	changed, err := s.repo.RevokeOutletRole(ctx, actor.StaffID, staffID, g)
	return s.afterRoleChange(ctx, staffID, changed, err, false)
}

// GrantGlobalRole and RevokeGlobalRole are reserved for global owners.
func (s *Service) GrantGlobalRole(ctx context.Context, actor *domain.StaffAccess, staffID, role string) error {
	if err := checkGlobalChange(actor, staffID); err != nil {
		return err
	}
	changed, err := s.repo.GrantGlobalRole(ctx, actor.StaffID, staffID, role)
	return s.afterRoleChange(ctx, staffID, changed, err, true)
}

func (s *Service) RevokeGlobalRole(ctx context.Context, actor *domain.StaffAccess, staffID, role string) error {
	if err := checkGlobalChange(actor, staffID); err != nil {
		return err
	}
	changed, err := s.repo.RevokeGlobalRole(ctx, actor.StaffID, staffID, role)
	return s.afterRoleChange(ctx, staffID, changed, err, false)
}

// Deactivate blocks the staff member immediately: no new logins (OTP
// eligibility requires is_active), every refresh family revoked, and the
// authz cache dropped so outstanding access tokens fail on their next use.
// Repeating it re-runs the revocation, so a partial failure can be retried.
func (s *Service) Deactivate(ctx context.Context, actor *domain.StaffAccess, staffID string) error {
	if err := s.checkManage(ctx, actor, staffID); err != nil {
		return err
	}
	if _, err := s.repo.Deactivate(ctx, actor.StaffID, staffID); err != nil {
		return err
	}
	return s.cutOff(ctx, staffID)
}

// RevokeSessions ends every session of another staff member (FR-TOK-9).
func (s *Service) RevokeSessions(ctx context.Context, actor *domain.StaffAccess, staffID string) error {
	if err := s.checkManage(ctx, actor, staffID); err != nil {
		return err
	}
	if err := s.sessions.RevokeSubject(ctx, string(domain.AudienceStaff), staffID); err != nil {
		return domain.WrapError(domain.CodeAuthUnavailable, errAuthUnavailable, err)
	}
	return nil
}

// SetPIN sets the caller's own POS unlock PIN (FR-PIN-3, FR-PIN-4). The
// route accepts only staff tokens, so a PIN is always set after an OTP
// login, never from a PIN-unlocked POS session.
func (s *Service) SetPIN(ctx context.Context, staffID, pin string, meta RequestMeta) error {
	if err := checkPIN(pin, s.pin.Denylist); err != nil {
		return err
	}
	hash, err := hashPIN(pin, s.pin)
	if err != nil {
		return err
	}
	ok, err := s.repo.SetPIN(ctx, staffID, hash, meta)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrNotFound
	}
	return nil
}

// ResetPIN clears another staff member's PIN (FR-PIN-5). They then set a
// new one after an OTP login. Devices drop the old PIN on their next roster
// fetch.
func (s *Service) ResetPIN(ctx context.Context, actor *domain.StaffAccess, staffID string, meta RequestMeta) error {
	target, err := s.access.Load(ctx, staffID)
	if err != nil {
		return err
	}
	if target == nil {
		return domain.ErrNotFound
	}
	if err := canResetPIN(actor, target); err != nil {
		return err
	}
	return s.repo.ResetPIN(ctx, actor.StaffID, staffID, meta)
}

func (s *Service) checkRoleChange(actor *domain.StaffAccess, staffID string, g grant) error {
	if actor.StaffID == staffID {
		return errSelfChange
	}
	return canAssign(actor, g.OutletID, g.Role)
}

func checkGlobalChange(actor *domain.StaffAccess, staffID string) error {
	if actor.StaffID == staffID {
		return errSelfChange
	}
	if !actor.IsGlobalOwner() {
		return errEscalation
	}
	return nil
}

func (s *Service) checkManage(ctx context.Context, actor *domain.StaffAccess, staffID string) error {
	target, err := s.access.Load(ctx, staffID)
	if err != nil {
		return err
	}
	if target == nil {
		return domain.ErrNotFound
	}
	return canManage(actor, target)
}

// afterRoleChange drops the target's cached access so the change applies on
// their next request. Revoking a role that was not held is not_found.
func (s *Service) afterRoleChange(ctx context.Context, staffID string, changed bool, err error, isGrant bool) error {
	if err != nil {
		return err
	}
	if !changed {
		if isGrant {
			return nil
		}
		return domain.ErrNotFound
	}
	if err := s.access.Invalidate(ctx, staffID); err != nil {
		return domain.WrapError(domain.CodeAuthUnavailable, errAuthUnavailable, err)
	}
	return nil
}

func (s *Service) cutOff(ctx context.Context, staffID string) error {
	if err := s.sessions.RevokeSubject(ctx, string(domain.AudienceStaff), staffID); err != nil {
		return domain.WrapError(domain.CodeAuthUnavailable, errAuthUnavailable, err)
	}
	if err := s.access.Invalidate(ctx, staffID); err != nil {
		return domain.WrapError(domain.CodeAuthUnavailable, errAuthUnavailable, err)
	}
	return nil
}

// parseAs normalizes an optional identifier field and checks it is the
// expected kind.
func parseAs(raw string, kind domain.IdentifierKind, field string) (string, error) {
	if raw == "" {
		return "", nil
	}
	id, err := domain.ParseIdentifier(raw)
	if err != nil || id.Kind != kind {
		return "", domain.ValidationError(map[string]string{field: "is not a valid " + field})
	}
	return id.Value, nil
}
