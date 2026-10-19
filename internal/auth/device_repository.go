package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// Locking the outlet row serializes enrollments at one outlet, so two
	// concurrent ones cannot both pass the device limit.
	lockOutlet = `SELECT is_active FROM outlets WHERE id = $1 FOR UPDATE`

	findDeviceByFingerprint = `
		SELECT id, outlet_id, is_active FROM devices
		WHERE device_fingerprint = $1
		FOR UPDATE`

	countOtherActiveDevices = `
		SELECT count(*) FROM devices
		WHERE outlet_id = $1 AND is_active AND device_fingerprint IS DISTINCT FROM $2`

	// replace makes room by revoking the longest-enrolled devices first.
	replaceDevices = `
		UPDATE devices SET is_active = FALSE, revoked_at = now(), revoked_by = $3
		WHERE id IN (
		  SELECT id FROM devices
		  WHERE outlet_id = $1 AND is_active AND device_fingerprint IS DISTINCT FROM $2
		  ORDER BY enrolled_at, id
		  LIMIT $4)
		RETURNING id, COALESCE(device_fingerprint, '')`

	insertDevice = `
		INSERT INTO devices (id, outlet_id, name, device_fingerprint, enrolled_by)
		VALUES ($1, $2, $3, $4, $5)`

	reenrollDevice = `
		UPDATE devices
		SET outlet_id = $2, name = $3, enrolled_by = $4, enrolled_at = now(),
		    is_active = TRUE, revoked_at = NULL, revoked_by = NULL
		WHERE id = $1`

	revokeDevice = `
		UPDATE devices SET is_active = FALSE, revoked_at = now(), revoked_by = $2
		WHERE id = $1 AND is_active
		RETURNING outlet_id, COALESCE(device_fingerprint, '')`

	findDeviceOutlet = `SELECT outlet_id FROM devices WHERE id = $1`

	deviceActive = `
		SELECT EXISTS (SELECT 1 FROM devices WHERE id = $1 AND outlet_id = $2 AND is_active)`

	touchDevice = `UPDATE devices SET last_seen_at = now() WHERE id = $1`

	insertDeviceAudit = `
		INSERT INTO audit_logs (id, outlet_id, actor_id, actor_type, action, entity_type, entity_id, before, after)
		VALUES ($1, $2, $3, 'staff', $4, 'device', $5, $6, $7)`
)

const pgUniqueViolation = "23505"

// deviceState is the audited before/after of a device change.
type deviceState struct {
	IsActive   bool   `json:"is_active"`
	Name       string `json:"name,omitempty"`
	Reenrolled bool   `json:"reenrolled,omitempty"`
	ReplacedBy string `json:"replaced_by,omitempty"`
}

// CountOtherActiveDevices counts active devices at the outlet other than the
// one holding fingerprint.
func (r *Repository) CountOtherActiveDevices(ctx context.Context, outletID, fingerprint string) (int, error) {
	var n int
	if err := r.db.Pool.QueryRow(ctx, countOtherActiveDevices, outletID, fingerprint).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active devices: %w", err)
	}
	return n, nil
}

// EnrollDevice creates or re-enrolls the device, revoking older devices when
// e.Replace allows it, and audits every change, in one transaction.
func (r *Repository) EnrollDevice(ctx context.Context, e *Enrollment) (*EnrollResult, error) {
	var res *EnrollResult
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveOutlet(ctx, tx, e.OutletID); err != nil {
			return err
		}
		existing, err := claimFingerprint(ctx, tx, e)
		if err != nil {
			return err
		}
		replaced, err := makeRoom(ctx, tx, e)
		if err != nil {
			return err
		}
		res = &EnrollResult{DeviceID: e.DeviceID, Existed: existing != "", Replaced: replaced}
		if res.Existed {
			res.DeviceID = existing
		}
		if err := saveDevice(ctx, tx, e, res); err != nil {
			return err
		}
		return auditEnrollment(ctx, tx, e, res)
	})
	return res, err
}

func lockActiveOutlet(ctx context.Context, tx pgx.Tx, outletID string) error {
	var active bool
	err := tx.QueryRow(ctx, lockOutlet, outletID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock outlet: %w", err)
	}
	if !active {
		return domain.ErrNotFound
	}
	return nil
}

// claimFingerprint returns the ID of the row already holding the
// fingerprint, or "" if none does. A fingerprint still active at another
// outlet is a conflict; a revoked one may be re-enrolled anywhere.
func claimFingerprint(ctx context.Context, tx pgx.Tx, e *Enrollment) (string, error) {
	var (
		id, outletID string
		active       bool
	)
	err := tx.QueryRow(ctx, findDeviceByFingerprint, e.Fingerprint).Scan(&id, &outletID, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find device by fingerprint: %w", err)
	}
	if active && outletID != e.OutletID {
		return "", errDeviceElsewhere
	}
	return id, nil
}

// makeRoom enforces the device limit (FR-DEV-3): over the limit it fails
// unless e.Replace is set, in which case it revokes just enough devices.
func makeRoom(ctx context.Context, tx pgx.Tx, e *Enrollment) ([]DeviceRef, error) {
	var n int
	if err := tx.QueryRow(ctx, countOtherActiveDevices, e.OutletID, e.Fingerprint).Scan(&n); err != nil {
		return nil, fmt.Errorf("count active devices: %w", err)
	}
	if n < e.Limit {
		return nil, nil
	}
	if !e.Replace {
		return nil, errDeviceLimit
	}
	rows, err := tx.Query(ctx, replaceDevices, e.OutletID, e.Fingerprint, e.StaffID, n-e.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("replace devices: %w", err)
	}
	defer rows.Close()

	var replaced []DeviceRef
	for rows.Next() {
		var d DeviceRef
		if err := rows.Scan(&d.ID, &d.Fingerprint); err != nil {
			return nil, fmt.Errorf("scan replaced device: %w", err)
		}
		replaced = append(replaced, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read replaced devices: %w", err)
	}
	return replaced, nil
}

func saveDevice(ctx context.Context, tx pgx.Tx, e *Enrollment, res *EnrollResult) error {
	if res.Existed {
		if _, err := tx.Exec(ctx, reenrollDevice, res.DeviceID, e.OutletID, e.Name, e.StaffID); err != nil {
			return fmt.Errorf("re-enroll device: %w", err)
		}
		return nil
	}
	_, err := tx.Exec(ctx, insertDevice, res.DeviceID, e.OutletID, e.Name, e.Fingerprint, e.StaffID)
	if err != nil {
		// Only a concurrent enrollment of the same fingerprint at another
		// outlet gets here; same-outlet ones are serialized by the lock.
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgUniqueViolation {
			return errDeviceElsewhere
		}
		return fmt.Errorf("insert device: %w", err)
	}
	return nil
}

func auditEnrollment(ctx context.Context, tx pgx.Tx, e *Enrollment, res *EnrollResult) error {
	for _, d := range res.Replaced {
		err := auditDevice(ctx, tx, e.OutletID, e.StaffID, "device.revoked", d.ID,
			deviceState{IsActive: true}, deviceState{ReplacedBy: res.DeviceID})
		if err != nil {
			return err
		}
	}
	return auditDevice(ctx, tx, e.OutletID, e.StaffID, "device.enrolled", res.DeviceID,
		nil, deviceState{IsActive: true, Name: e.Name, Reenrolled: res.Existed})
}

// RevokeDevice deactivates an active device and audits it. It returns the
// device fingerprint and false when the device was already inactive.
func (r *Repository) RevokeDevice(ctx context.Context, actorID, deviceID string) (string, bool, error) {
	var fingerprint string
	changed := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var outletID string
		err := tx.QueryRow(ctx, revokeDevice, deviceID, actorID).Scan(&outletID, &fingerprint)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("revoke device: %w", err)
		}
		changed = true
		return auditDevice(ctx, tx, outletID, actorID, "device.revoked", deviceID,
			deviceState{IsActive: true}, deviceState{})
	})
	return fingerprint, changed, err
}

// DeviceOutlet returns the outlet a device belongs to, or not_found.
func (r *Repository) DeviceOutlet(ctx context.Context, deviceID string) (string, error) {
	var outletID string
	err := r.db.Pool.QueryRow(ctx, findDeviceOutlet, deviceID).Scan(&outletID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find device outlet: %w", err)
	}
	return outletID, nil
}

// DeviceActive reports whether the device is active and still bound to
// outletID.
func (r *Repository) DeviceActive(ctx context.Context, deviceID, outletID string) (bool, error) {
	var active bool
	if err := r.db.Pool.QueryRow(ctx, deviceActive, deviceID, outletID).Scan(&active); err != nil {
		return false, fmt.Errorf("check device active: %w", err)
	}
	return active, nil
}

func (r *Repository) TouchDevice(ctx context.Context, deviceID string) error {
	if _, err := r.db.Pool.Exec(ctx, touchDevice, deviceID); err != nil {
		return fmt.Errorf("touch device: %w", err)
	}
	return nil
}

func auditDevice(ctx context.Context, tx pgx.Tx, outletID, actorID, action, deviceID string, before, after any) error {
	_, err := tx.Exec(ctx, insertDeviceAudit, uuid.NewString(), outletID, actorID, action, deviceID, before, after)
	if err != nil {
		return fmt.Errorf("insert device audit log: %w", err)
	}
	return nil
}
