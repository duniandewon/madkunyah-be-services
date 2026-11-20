package outlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/duniandewon/madkunyah-be-service/internal/auth"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxListed caps the staff outlet list (FR-OTL-5); outlet counts are small.
const maxListed = 500

// pgInvalidParameter is what AT TIME ZONE raises for an unknown zone.
const pgInvalidParameter = "22023"

const (
	outletColumns = `
		o.id::text, o.name, o.address, o.phone, o.timezone, to_char(o.business_day_cutoff, 'HH24:MI'),
		o.order_prefix, o.is_active, o.created_at, o.updated_at`

	openShiftColumn = `(SELECT s.id::text FROM shifts s WHERE s.outlet_id = o.id AND s.status = 'open')`

	selectOutlet = `SELECT ` + outletColumns + `, ` + openShiftColumn + ` FROM outlets o WHERE o.id = $1`

	// $1 lists the outlets to return unless $2 (every outlet); $3 includes
	// inactive ones.
	listOutlets = `
		SELECT ` + outletColumns + `, ` + openShiftColumn + `
		FROM outlets o
		WHERE ($2 OR o.id = ANY($1::uuid[])) AND ($3 OR o.is_active)
		ORDER BY lower(o.name), o.id
		LIMIT $4`

	// The open shift is read in its own statement after the lock, so it
	// sees a shift opened by the transaction the lock waited for (FR-OTL-19).
	lockOutlet = `SELECT ` + outletColumns + ` FROM outlets o WHERE o.id = $1 FOR UPDATE`

	openShift = `SELECT id::text FROM shifts WHERE outlet_id = $1 AND status = 'open'`

	// zoneKnown fails with invalid_parameter_value when Postgres does not
	// know the zone. Reads use AT TIME ZONE on the stored name, so Postgres
	// must accept every name Go accepted.
	zoneKnown = `SELECT 1 WHERE now() AT TIME ZONE $1 IS NOT NULL`

	insertOutlet = `
		INSERT INTO outlets (id, name, address, phone, timezone, business_day_cutoff, order_prefix)
		VALUES ($1, $2, $3, $4, $5, $6::time, $7)
		RETURNING created_at, updated_at`

	updateOutlet = `
		UPDATE outlets
		SET name = $2, address = $3, phone = $4, timezone = $5, business_day_cutoff = $6::time,
		    order_prefix = $7, updated_at = now()
		WHERE id = $1
		RETURNING updated_at`

	setActive = `UPDATE outlets SET is_active = $2, updated_at = now() WHERE id = $1 RETURNING updated_at`

	// revokeDevices revokes every active device at the outlet and audits
	// each as device.revoked does (FR-OTL-14), in one statement.
	revokeDevices = `
		WITH revoked AS (
		  UPDATE devices SET is_active = FALSE, revoked_at = now(), revoked_by = $2
		  WHERE outlet_id = $1 AND is_active
		  RETURNING id, COALESCE(device_fingerprint, '') AS fingerprint
		), audit AS (
		  INSERT INTO audit_logs (id, outlet_id, actor_id, actor_type, action, entity_type, entity_id, before, after)
		  SELECT gen_random_uuid(), $1, $2, 'staff', 'device.revoked', 'device', id,
		         '{"is_active": true}', '{"is_active": false}'
		  FROM revoked
		)
		SELECT id::text, fingerprint FROM revoked`

	insertAudit = `
		INSERT INTO audit_logs (id, outlet_id, actor_id, actor_type, action, entity_type, entity_id, before, after)
		VALUES ($1, $2, $3, 'staff', $4, 'outlet', $2, $5, $6)`
)

type Repository struct {
	db *postgres.DB
}

func NewRepository(db *postgres.DB) *Repository {
	return &Repository{db: db}
}

type scanner interface {
	Scan(dest ...any) error
}

func scanOutlet(row scanner, withShift bool) (*Outlet, error) {
	var o Outlet
	dest := []any{&o.ID, &o.Name, &o.Address, &o.Phone, &o.Timezone, &o.BusinessDayCutoff,
		&o.OrderPrefix, &o.IsActive, &o.CreatedAt, &o.UpdatedAt}
	if withShift {
		dest = append(dest, &o.OpenShiftID)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &o, nil
}

// Create inserts an outlet and its outlet.created audit row.
func (r *Repository) Create(ctx context.Context, actorID string, o *Outlet) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := checkZone(ctx, tx, o.Timezone); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, insertOutlet, o.ID, o.Name, o.Address, o.Phone, o.Timezone,
			o.BusinessDayCutoff, o.OrderPrefix).Scan(&o.CreatedAt, &o.UpdatedAt)
		if err != nil {
			return fmt.Errorf("insert outlet: %w", err)
		}
		return audit(ctx, tx, actorID, o.ID, "outlet.created", nil, o.auditFields())
	})
}

// Get returns one outlet with its open shift, or not_found.
func (r *Repository) Get(ctx context.Context, id string) (*Outlet, error) {
	o, err := scanOutlet(r.db.Pool.QueryRow(ctx, selectOutlet, id), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get outlet: %w", err)
	}
	return o, nil
}

// List returns the outlets in ids, or every outlet when all is set.
func (r *Repository) List(ctx context.Context, ids []string, all, inactive bool) ([]Outlet, error) {
	rows, err := r.db.Pool.Query(ctx, listOutlets, ids, all, inactive, maxListed)
	if err != nil {
		return nil, fmt.Errorf("list outlets: %w", err)
	}
	defer rows.Close()
	out := []Outlet{}
	for rows.Next() {
		o, err := scanOutlet(rows, true)
		if err != nil {
			return nil, fmt.Errorf("scan outlet: %w", err)
		}
		out = append(out, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list outlets: %w", err)
	}
	return out, nil
}

// changeFunc decides an outlet's next state from the locked current one
// and its open shift ("" when none), or refuses the change.
type changeFunc func(cur *Outlet, openShift string) (*Outlet, error)

// Update locks the outlet, applies change and writes the changed fields
// with one outlet.updated row. It returns false when nothing changed.
func (r *Repository) Update(ctx context.Context, actorID, id string, change changeFunc) (*Outlet, bool, error) {
	var out *Outlet
	changed := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		cur, shiftID, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		next, err := change(cur, shiftID)
		if err != nil {
			return err
		}
		before, after := diff(cur.auditFields(), next.auditFields())
		if len(after) == 0 {
			out = cur
			return nil
		}
		if next.Timezone != cur.Timezone {
			if err := checkZone(ctx, tx, next.Timezone); err != nil {
				return err
			}
		}
		err = tx.QueryRow(ctx, updateOutlet, id, next.Name, next.Address, next.Phone, next.Timezone,
			next.BusinessDayCutoff, next.OrderPrefix).Scan(&next.UpdatedAt)
		if err != nil {
			return fmt.Errorf("update outlet: %w", err)
		}
		out, changed = next, true
		return audit(ctx, tx, actorID, id, "outlet.updated", before, after)
	})
	return out, changed, err
}

// allowFunc refuses a state change given the outlet's open shift.
type allowFunc func(openShift string) error

// activeChange is the outcome of SetActive.
type activeChange struct {
	outlet  *Outlet
	applied bool
	// devices were revoked by a deactivation; their sessions are ended
	// after commit.
	devices []auth.DeviceRef
}

// SetActive deactivates or reactivates an outlet. A deactivation also
// revokes its active devices in the same transaction (FR-OTL-14).
func (r *Repository) SetActive(ctx context.Context, actorID, id string, active bool, allow allowFunc) (*activeChange, error) {
	res := &activeChange{}
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		cur, shiftID, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		res.outlet = cur
		if cur.IsActive == active {
			return nil
		}
		if err := allow(shiftID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, setActive, id, active).Scan(&cur.UpdatedAt); err != nil {
			return fmt.Errorf("set outlet active: %w", err)
		}
		cur.IsActive, res.applied = active, true
		if active {
			return audit(ctx, tx, actorID, id, "outlet.activated", map[string]any{"is_active": false},
				map[string]any{"is_active": true})
		}
		if res.devices, err = revoke(ctx, tx, actorID, id); err != nil {
			return err
		}
		return audit(ctx, tx, actorID, id, "outlet.deactivated", map[string]any{"is_active": true},
			map[string]any{"is_active": false, "devices_revoked": len(res.devices)})
	})
	return res, err
}

// lock locks the outlet row and then reads its open shift.
func lock(ctx context.Context, tx pgx.Tx, id string) (*Outlet, string, error) {
	cur, err := scanOutlet(tx.QueryRow(ctx, lockOutlet, id), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", domain.ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("lock outlet: %w", err)
	}
	var shiftID string
	err = tx.QueryRow(ctx, openShift, id).Scan(&shiftID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, "", fmt.Errorf("get open shift: %w", err)
	}
	if shiftID != "" {
		cur.OpenShiftID = &shiftID
	}
	return cur, shiftID, nil
}

// checkZone refuses a timezone Postgres does not know (spec §3.1).
func checkZone(ctx context.Context, tx pgx.Tx, name string) error {
	var one int
	err := tx.QueryRow(ctx, zoneKnown, name).Scan(&one)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgInvalidParameter {
		return domain.ValidationError(map[string]string{"timezone": "is not a timezone the database knows"})
	}
	if err != nil {
		return fmt.Errorf("check timezone: %w", err)
	}
	return nil
}

func revoke(ctx context.Context, tx pgx.Tx, actorID, outletID string) ([]auth.DeviceRef, error) {
	rows, err := tx.Query(ctx, revokeDevices, outletID, actorID)
	if err != nil {
		return nil, fmt.Errorf("revoke outlet devices: %w", err)
	}
	devices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auth.DeviceRef, error) {
		var d auth.DeviceRef
		err := row.Scan(&d.ID, &d.Fingerprint)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("revoke outlet devices: %w", err)
	}
	return devices, nil
}

func audit(ctx context.Context, tx pgx.Tx, actorID, outletID, action string, before, after map[string]any) error {
	b, err := jsonOrNil(before)
	if err != nil {
		return err
	}
	a, err := jsonOrNil(after)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertAudit, uuid.NewString(), outletID, actorID, action, b, a); err != nil {
		return fmt.Errorf("insert outlet audit: %w", err)
	}
	return nil
}

func jsonOrNil(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal audit: %w", err)
	}
	return b, nil
}
