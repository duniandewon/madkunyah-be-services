// Package authz resolves what a staff member may do, per outlet.
package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"

	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

// One row per role grant; global grants have an empty outlet. A staff member
// with no grants yields one row with an empty role.
const loadAccess = `
	SELECT s.is_active,
	       COALESCE(g.outlet_id::text, ''),
	       COALESCE(r.code, ''),
	       COALESCE(r.permissions, '[]'::jsonb)
	FROM staff s
	LEFT JOIN (
	  SELECT staff_id, outlet_id, role_id FROM staff_outlet_roles
	  UNION ALL
	  SELECT staff_id, NULL, role_id FROM staff_global_roles
	) g ON g.staff_id = s.id
	LEFT JOIN roles r ON r.id = g.role_id
	WHERE s.id = $1`

type accessCache interface {
	GetAccess(ctx context.Context, staffID string) ([]byte, error)
	SetAccess(ctx context.Context, staffID string, b []byte, ttl time.Duration) error
	DeleteAccess(ctx context.Context, staffID string) error
}

// Resolver loads staff access from Postgres and caches it in Redis.
type Resolver struct {
	db    *postgres.DB
	cache accessCache
	ttl   time.Duration
}

func NewResolver(db *postgres.DB, cache accessCache, ttl time.Duration) *Resolver {
	return &Resolver{db: db, cache: cache, ttl: ttl}
}

// Access returns the caller's access, from cache when possible. A Redis
// failure falls back to Postgres rather than failing the request. An unknown
// staff ID resolves to an inactive record with no roles.
func (r *Resolver) Access(ctx context.Context, staffID string) (*domain.StaffAccess, error) {
	b, err := r.cache.GetAccess(ctx, staffID)
	if err == nil {
		var a domain.StaffAccess
		if json.Unmarshal(b, &a) == nil {
			return &a, nil
		}
	} else if !errors.Is(err, credis.ErrNotFound) {
		slog.WarnContext(ctx, "authz cache unavailable, using postgres", "error", err)
	}

	a, err := r.Load(ctx, staffID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return &domain.StaffAccess{StaffID: staffID}, nil
	}
	if b, err := json.Marshal(a); err == nil {
		if err := r.cache.SetAccess(ctx, staffID, b, r.ttl); err != nil {
			slog.WarnContext(ctx, "write authz cache", "error", err)
		}
	}
	return a, nil
}

// Load reads access straight from Postgres, bypassing the cache. It returns
// nil, nil when the staff member does not exist.
func (r *Resolver) Load(ctx context.Context, staffID string) (*domain.StaffAccess, error) {
	rows, err := r.db.Pool.Query(ctx, loadAccess, staffID)
	if err != nil {
		return nil, fmt.Errorf("load staff access: %w", err)
	}
	defer rows.Close()

	var a *domain.StaffAccess
	for rows.Next() {
		var (
			active       bool
			outlet, role string
			permissions  []string
		)
		if err := rows.Scan(&active, &outlet, &role, &permissions); err != nil {
			return nil, fmt.Errorf("scan staff access: %w", err)
		}
		if a == nil {
			a = &domain.StaffAccess{StaffID: staffID, Active: active, Outlets: map[string]domain.AccessScope{}}
		}
		if role != "" {
			addGrant(a, outlet, role, permissions)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read staff access: %w", err)
	}
	return a, nil
}

// Invalidate drops the cached record so the next request sees the change.
func (r *Resolver) Invalidate(ctx context.Context, staffID string) error {
	return r.cache.DeleteAccess(ctx, staffID)
}

func addGrant(a *domain.StaffAccess, outlet, role string, permissions []string) {
	scope := a.Global
	if outlet != "" {
		scope = a.Outlets[outlet]
	}
	scope.Roles = append(scope.Roles, role)
	for _, p := range permissions {
		if !slices.Contains(scope.Permissions, p) {
			scope.Permissions = append(scope.Permissions, p)
		}
	}
	if outlet == "" {
		a.Global = scope
	} else {
		a.Outlets[outlet] = scope
	}
}
