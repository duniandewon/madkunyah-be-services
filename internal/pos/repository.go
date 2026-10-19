package pos

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
)

const (
	// Active staff with a role reaching the outlet: an outlet role there, or
	// a global role (which applies at every outlet). Permissions are the
	// union across those roles, sorted so the roster version is stable.
	loadRoster = `
		SELECT s.id, s.full_name, s.pin_hash,
		       COALESCE(array_agg(DISTINCT p.perm ORDER BY p.perm)
		                FILTER (WHERE p.perm IS NOT NULL), '{}')
		FROM staff s
		JOIN (
		  SELECT staff_id, role_id FROM staff_outlet_roles WHERE outlet_id = $1
		  UNION
		  SELECT staff_id, role_id FROM staff_global_roles
		) g ON g.staff_id = s.id
		JOIN roles r ON r.id = g.role_id
		LEFT JOIN LATERAL jsonb_array_elements_text(r.permissions) AS p(perm) ON TRUE
		WHERE s.is_active
		GROUP BY s.id
		ORDER BY s.id`

	// The client-generated ID makes a replayed batch a no-op. Unknown staff
	// IDs are dropped by the join. IP and user agent are left empty: the
	// failure happened earlier, on the device, not on this request.
	insertPINFailures = `
		INSERT INTO auth_events (id, subject_type, subject_id, identifier, event, device_id, created_at)
		SELECT f.id, 'staff', s.id, COALESCE(s.phone, s.email), 'pin.failed', $4, f.occurred_at
		FROM unnest($1::uuid[], $2::uuid[], $3::timestamptz[]) AS f(id, staff_id, occurred_at)
		JOIN staff s ON s.id = f.staff_id
		ON CONFLICT (id) DO NOTHING`
)

type Repository struct {
	db *postgres.DB
}

func NewRepository(db *postgres.DB) *Repository { return &Repository{db: db} }

func (r *Repository) Roster(ctx context.Context, outletID string) ([]RosterEntry, error) {
	rows, err := r.db.Pool.Query(ctx, loadRoster, outletID)
	if err != nil {
		return nil, fmt.Errorf("load roster: %w", err)
	}
	defer rows.Close()

	entries := []RosterEntry{}
	for rows.Next() {
		var (
			e   RosterEntry
			pin sql.NullString
		)
		if err := rows.Scan(&e.StaffID, &e.FullName, &pin, &e.Permissions); err != nil {
			return nil, fmt.Errorf("scan roster entry: %w", err)
		}
		if pin.Valid {
			e.PINHash = &pin.String
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read roster: %w", err)
	}
	return entries, nil
}

func (r *Repository) RecordPINFailures(ctx context.Context, deviceID string, failures []PINFailure) error {
	ids := make([]string, len(failures))
	staffIDs := make([]string, len(failures))
	times := make([]time.Time, len(failures))
	for i, f := range failures {
		ids[i], staffIDs[i], times[i] = f.ID, f.StaffID, f.OccurredAt
	}
	if _, err := r.db.Pool.Exec(ctx, insertPINFailures, ids, staffIDs, times, deviceID); err != nil {
		return fmt.Errorf("insert pin failures: %w", err)
	}
	return nil
}
