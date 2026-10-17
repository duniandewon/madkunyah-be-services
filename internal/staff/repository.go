package staff

import (
	"context"
	"errors"
	"fmt"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	listOutletStaff = `
		SELECT s.id, s.full_name, COALESCE(s.phone, ''), COALESCE(s.email, ''), s.is_active,
		       array_agg(r.code ORDER BY r.code)
		FROM staff s
		JOIN staff_outlet_roles sor ON sor.staff_id = s.id
		JOIN roles r ON r.id = sor.role_id
		WHERE sor.outlet_id = $1
		GROUP BY s.id
		ORDER BY s.full_name, s.id`

	outletExists = `SELECT EXISTS (SELECT 1 FROM outlets WHERE id = $1)`

	insertStaff = `
		INSERT INTO staff (id, full_name, phone, email)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''))`

	grantOutletRole = `
		INSERT INTO staff_outlet_roles (staff_id, outlet_id, role_id)
		SELECT $1, $2, r.id FROM roles r WHERE r.code = $3
		ON CONFLICT DO NOTHING`

	revokeOutletRole = `
		DELETE FROM staff_outlet_roles sor
		USING roles r
		WHERE sor.role_id = r.id AND sor.staff_id = $1 AND sor.outlet_id = $2 AND r.code = $3`

	grantGlobalRole = `
		INSERT INTO staff_global_roles (staff_id, role_id)
		SELECT $1, r.id FROM roles r WHERE r.code = $2
		ON CONFLICT DO NOTHING`

	revokeGlobalRole = `
		DELETE FROM staff_global_roles sgr
		USING roles r
		WHERE sgr.role_id = r.id AND sgr.staff_id = $1 AND r.code = $2`

	deactivateStaff = `
		UPDATE staff SET is_active = FALSE, deactivated_at = now(), updated_at = now()
		WHERE id = $1 AND is_active`

	insertAuditLog = `
		INSERT INTO audit_logs (id, outlet_id, actor_id, actor_type, action, entity_type, entity_id, before, after)
		VALUES ($1, NULLIF($2, '')::uuid, $3, 'staff', $4, 'staff', $5, $6, $7)`
)

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

var errIdentifierTaken = domain.NewError(domain.CodeConflict,
	"A staff member with this phone or email already exists.")

// Member is a staff member as seen from one outlet.
type Member struct {
	ID       string   `json:"id"`
	FullName string   `json:"full_name"`
	Phone    string   `json:"phone,omitempty"`
	Email    string   `json:"email,omitempty"`
	IsActive bool     `json:"is_active"`
	Roles    []string `json:"roles"`
}

// grant is the audited payload of a role change. OutletID is empty for a
// global grant.
type grant struct {
	OutletID string `json:"outlet_id,omitempty"`
	Role     string `json:"role"`
}

type Repository struct {
	db *postgres.DB
}

func NewRepository(db *postgres.DB) *Repository { return &Repository{db: db} }

func (r *Repository) ListOutletStaff(ctx context.Context, outletID string) ([]Member, error) {
	rows, err := r.db.Pool.Query(ctx, listOutletStaff, outletID)
	if err != nil {
		return nil, fmt.Errorf("list outlet staff: %w", err)
	}
	defer rows.Close()

	members := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.FullName, &m.Phone, &m.Email, &m.IsActive, &m.Roles); err != nil {
			return nil, fmt.Errorf("scan outlet staff: %w", err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read outlet staff: %w", err)
	}
	return members, nil
}

// Create inserts a staff member with an initial role at outletID, auditing
// the grant, in one transaction.
func (r *Repository) Create(ctx context.Context, actorID, outletID string, m *Member) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, outletExists, outletID).Scan(&exists); err != nil {
			return fmt.Errorf("check outlet: %w", err)
		}
		if !exists {
			return domain.ErrNotFound
		}
		if _, err := tx.Exec(ctx, insertStaff, m.ID, m.FullName, m.Phone, m.Email); err != nil {
			if pgCode(err) == pgUniqueViolation {
				return errIdentifierTaken
			}
			return fmt.Errorf("insert staff: %w", err)
		}
		g := grant{OutletID: outletID, Role: m.Roles[0]}
		if _, err := tx.Exec(ctx, grantOutletRole, m.ID, outletID, g.Role); err != nil {
			return fmt.Errorf("grant initial role: %w", err)
		}
		return audit(ctx, tx, actorID, outletID, "role.granted", m.ID, nil, g)
	})
}

// GrantOutletRole is idempotent: granting a role already held changes
// nothing and writes no audit entry.
func (r *Repository) GrantOutletRole(ctx context.Context, actorID, staffID string, g grant) (bool, error) {
	return r.change(ctx, grantOutletRole, []any{staffID, g.OutletID, g.Role},
		actorID, g.OutletID, "role.granted", staffID, nil, g)
}

func (r *Repository) RevokeOutletRole(ctx context.Context, actorID, staffID string, g grant) (bool, error) {
	return r.change(ctx, revokeOutletRole, []any{staffID, g.OutletID, g.Role},
		actorID, g.OutletID, "role.revoked", staffID, g, nil)
}

func (r *Repository) GrantGlobalRole(ctx context.Context, actorID, staffID, role string) (bool, error) {
	return r.change(ctx, grantGlobalRole, []any{staffID, role},
		actorID, "", "role.granted", staffID, nil, grant{Role: role})
}

func (r *Repository) RevokeGlobalRole(ctx context.Context, actorID, staffID, role string) (bool, error) {
	return r.change(ctx, revokeGlobalRole, []any{staffID, role},
		actorID, "", "role.revoked", staffID, grant{Role: role}, nil)
}

// Deactivate reports false when the staff member was already inactive.
func (r *Repository) Deactivate(ctx context.Context, actorID, staffID string) (bool, error) {
	return r.change(ctx, deactivateStaff, []any{staffID},
		actorID, "", "staff.deactivated", staffID,
		map[string]bool{"is_active": true}, map[string]bool{"is_active": false})
}

// change runs one statement and, if it affected a row, the matching audit
// entry, in a single transaction. A missing staff member or outlet is
// not_found.
func (r *Repository) change(ctx context.Context, query string, args []any,
	actorID, outletID, action, staffID string, before, after any) (bool, error) {
	changed := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			if pgCode(err) == pgForeignKeyViolation {
				return domain.ErrNotFound
			}
			return fmt.Errorf("%s: %w", action, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		changed = true
		return audit(ctx, tx, actorID, outletID, action, staffID, before, after)
	})
	return changed, err
}

func audit(ctx context.Context, tx pgx.Tx, actorID, outletID, action, staffID string, before, after any) error {
	_, err := tx.Exec(ctx, insertAuditLog, uuid.NewString(), outletID, actorID, action, staffID,
		before, after)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

func pgCode(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code
	}
	return ""
}
