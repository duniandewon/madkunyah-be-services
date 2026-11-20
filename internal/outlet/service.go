package outlet

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/auth"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/google/uuid"
)

// bumpTimeout bounds the post-commit cache bump, which runs even if the
// request was cancelled after its write committed.
const bumpTimeout = time.Second

type repository interface {
	Create(ctx context.Context, actorID string, o *Outlet) error
	Get(ctx context.Context, id string) (*Outlet, error)
	List(ctx context.Context, ids []string, all, inactive bool) ([]Outlet, error)
	Update(ctx context.Context, actorID, id string, change changeFunc) (*Outlet, bool, error)
	SetActive(ctx context.Context, actorID, id string, active bool, allow allowFunc) (*activeChange, error)
}

// docCache drops an outlet's cached menu and POS bundle.
type docCache interface {
	BumpOutletCatalogGeneration(ctx context.Context, outletID string) error
}

// deviceSessions ends the sessions of a device revoked by a deactivation.
type deviceSessions interface {
	EndDeviceSessions(ctx context.Context, d auth.DeviceRef, meta auth.RequestMeta) error
}

type Service struct {
	repo     repository
	cache    docCache
	sessions deviceSessions
}

// NewService takes a nil cache when caching is off.
func NewService(repo repository, cache docCache, sessions deviceSessions) *Service {
	return &Service{repo: repo, cache: cache, sessions: sessions}
}

// Create adds an active outlet with the defaults for omitted fields
// (FR-OTL-1..3). Nothing else is created: it inherits the catalog, and its
// first shift open creates its sequences.
func (s *Service) Create(ctx context.Context, actor *domain.StaffAccess, f *Fields) (*Outlet, error) {
	if bad := f.normalize(true); len(bad) > 0 {
		return nil, domain.ValidationError(bad)
	}
	o := f.apply(&Outlet{ID: uuid.NewString(), Timezone: defaultTimezone, BusinessDayCutoff: defaultCutoff,
		OrderPrefix: defaultPrefix, IsActive: true})
	if err := s.repo.Create(ctx, actor.StaffID, o); err != nil {
		return nil, err
	}
	return o, nil
}

// Get returns one outlet; the route has checked the caller reaches it.
func (s *Service) Get(ctx context.Context, id string) (*Outlet, error) {
	return s.repo.Get(ctx, id)
}

// ListMine returns the outlets the caller reaches: every outlet through a
// global role, else those they hold a role at. Inactive outlets are listed
// only for outlet.manage holders (FR-OTL-5).
func (s *Service) ListMine(ctx context.Context, a *domain.StaffAccess) ([]Outlet, error) {
	all := len(a.Global.Roles) > 0
	var ids []string
	if !all {
		for id, scope := range a.Outlets {
			if len(scope.Roles) > 0 {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return []Outlet{}, nil
		}
	}
	return s.repo.List(ctx, ids, all, a.CanGlobal(domain.PermOutletManage))
}

// Update changes the present fields. Day settings are locked while a shift
// is open; the order prefix is not, and applies at the next open
// (FR-OTL-7..12).
func (s *Service) Update(ctx context.Context, actor *domain.StaffAccess, id string, f *Fields) (*Outlet, error) {
	if bad := f.normalize(false); len(bad) > 0 {
		return nil, domain.ValidationError(bad)
	}
	docs := false
	o, changed, err := s.repo.Update(ctx, actor.StaffID, id, func(cur *Outlet, openShift string) (*Outlet, error) {
		next := f.apply(cur)
		if openShift != "" && daySettingsChanged(cur, next) {
			return nil, shiftOpen(openShift)
		}
		docs = inCachedDocs(cur, next)
		return next, nil
	})
	s.afterWrite(ctx, id, changed && docs, err)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// Deactivate closes an outlet: refused while a shift is open, and its
// devices are revoked. Staff roles are kept (FR-OTL-13..15).
func (s *Service) Deactivate(ctx context.Context, actor *domain.StaffAccess, id string, meta auth.RequestMeta) (*Result, error) {
	return s.setActive(ctx, actor, id, false, meta)
}

// Activate reopens an outlet. Revoked devices stay revoked (FR-OTL-16).
func (s *Service) Activate(ctx context.Context, actor *domain.StaffAccess, id string) (*Result, error) {
	return s.setActive(ctx, actor, id, true, auth.RequestMeta{})
}

func (s *Service) setActive(ctx context.Context, actor *domain.StaffAccess, id string, active bool,
	meta auth.RequestMeta) (*Result, error) {
	res, err := s.repo.SetActive(ctx, actor.StaffID, id, active, func(openShift string) error {
		if !active && openShift != "" {
			return shiftOpen(openShift)
		}
		return nil
	})
	s.afterWrite(ctx, id, res != nil && res.applied, err)
	if err != nil {
		return nil, err
	}
	s.endSessions(ctx, res.devices, meta)
	return &Result{Applied: res.applied, Outlet: res.outlet}, nil
}

// endSessions ends revoked devices' sessions after commit. A failure is
// logged, not returned: the device row is already inactive, and
// RequireActiveDevice refuses every POS call on it.
func (s *Service) endSessions(ctx context.Context, devices []auth.DeviceRef, meta auth.RequestMeta) {
	ctx = context.WithoutCancel(ctx)
	for _, d := range devices {
		if err := s.sessions.EndDeviceSessions(ctx, d, meta); err != nil {
			slog.ErrorContext(ctx, "end sessions of a deactivated outlet's device", "device_id", d.ID, "error", err)
		}
	}
}

// afterWrite drops the outlet's cached documents when a write changed what
// they embed, or its commit outcome is unknown (FR-OTL-12, FR-OTL-18).
func (s *Service) afterWrite(ctx context.Context, outletID string, changed bool, err error) {
	if s.cache == nil || (!changed && !errors.Is(err, postgres.ErrCommitUncertain)) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bumpTimeout)
	defer cancel()
	if err := s.cache.BumpOutletCatalogGeneration(ctx, outletID); err != nil {
		slog.ErrorContext(ctx, "catalog cache generation bump failed",
			"metric", "catalog_cache_gen_bump_failures", "error", err)
	}
}

func shiftOpen(shiftID string) error {
	return &domain.Error{
		Code:    domain.CodeOutletShiftOpen,
		Message: "Close the outlet's open shift first.",
		Details: map[string]string{"shift_id": shiftID},
	}
}
