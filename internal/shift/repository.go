package shift

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	openOutlet = `
		SELECT timezone,
		       (extract(hour FROM business_day_cutoff) * 60 + extract(minute FROM business_day_cutoff))::int,
		       order_prefix
		FROM outlets WHERE id = $1 AND is_active`

	shiftByID = `SELECT outlet_id::text, status FROM shifts WHERE id = $1`

	openShiftID = `SELECT id::text FROM shifts WHERE outlet_id = $1 AND status = 'open'`

	insertShift = `
		INSERT INTO shifts (id, outlet_id, device_id, business_date, opened_by, opened_at)
		VALUES ($1, $2, $3, $4::date, $5, $6)`

	insertSequence = `INSERT INTO order_sequences (shift_id, prefix) VALUES ($1, $2)`

	lockShift = `SELECT status FROM shifts WHERE id = $1 AND outlet_id = $2 FOR UPDATE`

	closeShift = `
		UPDATE shifts
		SET status = 'closed', closed_at = $2, closed_by = $3, closed_device_id = $4
		WHERE id = $1`

	selectShift = `
		SELECT s.id::text, s.outlet_id::text, s.status, s.business_date::text, s.opened_at,
		       s.opened_by::text, ob.full_name, s.device_id::text,
		       s.closed_at, s.closed_by::text, cb.full_name, s.closed_device_id::text
		FROM shifts s
		JOIN staff ob ON ob.id = s.opened_by
		LEFT JOIN staff cb ON cb.id = s.closed_by`

	getShift        = selectShift + ` WHERE s.id = $1`
	getOutletShift  = selectShift + ` WHERE s.id = $1 AND s.outlet_id = $2`
	getCurrentShift = selectShift + ` WHERE s.outlet_id = $1 AND s.status = 'open'`

	// Keyset pagination in index order; a null filter matches everything.
	listShifts = selectShift + `
		WHERE s.outlet_id = $1
		  AND ($2::date IS NULL OR s.business_date >= $2::date)
		  AND ($3::date IS NULL OR s.business_date <= $3::date)
		  AND ($4::text IS NULL OR s.status = $4::text)
		  AND ($5::date IS NULL OR (s.business_date, s.opened_at, s.id) < ($5::date, $6::timestamptz, $7::uuid))
		ORDER BY s.business_date DESC, s.opened_at DESC, s.id DESC
		LIMIT $8`

	listSequences = `
		SELECT shift_id::text, prefix, last_value
		FROM order_sequences WHERE shift_id = ANY($1::uuid[])
		ORDER BY prefix`

	insertAudit = `
		INSERT INTO audit_logs (id, outlet_id, actor_id, actor_type, action, entity_type, entity_id, after, approved_by)
		VALUES ($1, $2, $3, 'staff', $4, 'shift', $5, $6, $7)`
)

const uniqueViolation = "23505"

type Repository struct {
	db *postgres.DB
}

func NewRepository(db *postgres.DB) *Repository { return &Repository{db: db} }

// openWrite is one authorized open.
type openWrite struct {
	id, outletID, deviceID string
	actorID, approverID    string
	at                     time.Time
}

// closeWrite is one authorized close. guard runs in the closing
// transaction, after the shift row is locked, and rejects the close by
// returning an error.
type closeWrite struct {
	shiftID, outletID, deviceID string
	actorID, approverID         string
	at                          time.Time
	guard                       func(ctx context.Context, q Querier) error
}

// Open creates the shift, its sequences and its audit row in one
// transaction, and reports whether it did: a retry with the ID of the
// outlet's open shift returns that shift unchanged (FR-OPN-2).
//
// Two opens can race past the checks and collide on the primary key or on
// one_open_shift_per_outlet. The loser's insert waits for the winner to
// commit, so one more attempt sees the committed shift and answers from it.
func (r *Repository) Open(ctx context.Context, w *openWrite) (*Shift, bool, error) {
	s, applied, err := r.tryOpen(ctx, w)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		s, applied, err = r.tryOpen(ctx, w)
	}
	return s, applied, err
}

func (r *Repository) tryOpen(ctx context.Context, w *openWrite) (*Shift, bool, error) {
	var s *Shift
	applied := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var tz, prefix string
		var cutoff int
		err := tx.QueryRow(ctx, openOutlet, w.outletID).Scan(&tz, &cutoff, &prefix)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get outlet: %w", err)
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return fmt.Errorf("outlet timezone %q: %w", tz, err)
		}
		existing, err := existingShift(ctx, tx, w)
		if err != nil || existing != nil {
			s = existing
			return err
		}
		if err := insertOpen(ctx, tx, w, businessDate(w.at, loc, cutoff), prefix); err != nil {
			return err
		}
		applied = true
		s, err = loadShift(ctx, tx, getShift, w.id)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return s, applied, nil
}

// existingShift answers an open from what is already stored, when it can:
// the retried shift itself, or an error naming why this ID or this outlet
// cannot open now. It returns nil, nil when the open should proceed.
func existingShift(ctx context.Context, tx pgx.Tx, w *openWrite) (*Shift, error) {
	var outletID, status string
	err := tx.QueryRow(ctx, shiftByID, w.id).Scan(&outletID, &status)
	switch {
	case err == nil && outletID != w.outletID:
		return nil, domain.NewError(domain.CodeConflict, "This shift ID is already in use.")
	case err == nil && status == StatusClosed:
		return nil, ErrShiftClosed
	case err == nil:
		return loadShift(ctx, tx, getShift, w.id)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("get shift: %w", err)
	}

	var openID string
	err = tx.QueryRow(ctx, openShiftID, w.outletID).Scan(&openID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get open shift: %w", err)
	}
	open, err := loadShift(ctx, tx, getShift, openID)
	if err != nil {
		return nil, err
	}
	return nil, &domain.Error{
		Code:    domain.CodeShiftAlreadyOpen,
		Message: "Another shift is already open at this outlet.",
		Details: OpenShiftInfo{ID: open.ID, OpenedAt: open.OpenedAt, OpenedBy: open.OpenedBy,
			BusinessDate: open.BusinessDate},
	}
}

// insertOpen writes the shift, one sequence per prefix starting at zero
// (FR-OPN-3), and the audit row.
func insertOpen(ctx context.Context, tx pgx.Tx, w *openWrite, date, prefix string) error {
	if _, err := tx.Exec(ctx, insertShift, w.id, w.outletID, w.deviceID, date, w.actorID, w.at); err != nil {
		return fmt.Errorf("insert shift: %w", err)
	}
	prefixes := []string{prefix}
	if prefix != PrefixWeb {
		prefixes = append(prefixes, PrefixWeb)
	}
	for _, p := range prefixes {
		if _, err := tx.Exec(ctx, insertSequence, w.id, p); err != nil {
			return fmt.Errorf("insert order sequence: %w", err)
		}
	}
	after := map[string]any{"business_date": date, "device_id": w.deviceID}
	return writeAudit(ctx, tx, w.outletID, w.actorID, w.approverID, "shift.opened", w.id, after)
}

// Close locks the shift, runs the guard, and closes and audits it in one
// transaction (FR-CLS-4). It reports false for a shift already closed
// (FR-CLS-5). A shift at another outlet is not found.
func (r *Repository) Close(ctx context.Context, w *closeWrite) (*Shift, bool, error) {
	var s *Shift
	applied := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, lockShift, w.shiftID, w.outletID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock shift: %w", err)
		}
		if status == StatusOpen {
			if err := closeLocked(ctx, tx, w); err != nil {
				return err
			}
			applied = true
		}
		s, err = loadShift(ctx, tx, getShift, w.shiftID)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return s, applied, nil
}

func closeLocked(ctx context.Context, tx pgx.Tx, w *closeWrite) error {
	if err := w.guard(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, closeShift, w.shiftID, w.at, w.actorID, w.deviceID); err != nil {
		return fmt.Errorf("close shift: %w", err)
	}
	seqs, err := sequences(ctx, tx, []string{w.shiftID})
	if err != nil {
		return err
	}
	after := map[string]any{"closed_device_id": w.deviceID, "sequences": seqs[w.shiftID]}
	return writeAudit(ctx, tx, w.outletID, w.actorID, w.approverID, "shift.closed", w.shiftID, after)
}

// writeAudit records an open or close in the caller's transaction, with
// the step-up approver (FR-SAUD-1).
func writeAudit(ctx context.Context, tx pgx.Tx, outletID, actorID, approverID, action, shiftID string,
	after map[string]any) error {
	_, err := tx.Exec(ctx, insertAudit, uuid.NewString(), outletID, actorID, action, shiftID, after, approverID)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// CurrentShift returns the outlet's open shift, or nil (FR-RD-1).
func (r *Repository) CurrentShift(ctx context.Context, outletID string) (*Shift, error) {
	s, err := loadShift(ctx, r.db.Pool, getCurrentShift, outletID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	return s, err
}

// Get returns one of the outlet's shifts (FR-RD-3).
func (r *Repository) Get(ctx context.Context, outletID, shiftID string) (*Shift, error) {
	return loadShift(ctx, r.db.Pool, getOutletShift, shiftID, outletID)
}

// List returns up to f.Limit shifts, newest business date first (FR-RD-2).
func (r *Repository) List(ctx context.Context, f *ListFilter) ([]Shift, error) {
	var afterDate, afterID *string
	var afterAt *time.Time
	if f.After != nil {
		afterDate, afterAt, afterID = &f.After.date, &f.After.openedAt, &f.After.id
	}
	rows, err := r.db.Pool.Query(ctx, listShifts, f.OutletID, nullIfEmpty(f.From), nullIfEmpty(f.To),
		nullIfEmpty(f.Status), afterDate, afterAt, afterID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list shifts: %w", err)
	}
	defer rows.Close()
	shifts := []Shift{}
	for rows.Next() {
		s, err := scanShift(rows)
		if err != nil {
			return nil, fmt.Errorf("scan shift: %w", err)
		}
		shifts = append(shifts, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read shifts: %w", err)
	}
	return shifts, attachSequences(ctx, r.db.Pool, shifts)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanShift(row rowScanner) (*Shift, error) {
	var s Shift
	var closedBy, closedByName *string
	err := row.Scan(&s.ID, &s.OutletID, &s.Status, &s.BusinessDate, &s.OpenedAt,
		&s.OpenedBy.ID, &s.OpenedBy.Name, &s.DeviceID,
		&s.ClosedAt, &closedBy, &closedByName, &s.ClosedDeviceID)
	if err != nil {
		return nil, err
	}
	if closedBy != nil && closedByName != nil {
		s.ClosedBy = &StaffRef{ID: *closedBy, Name: *closedByName}
	}
	return &s, nil
}

// loadShift reads one shift with its sequences, or ErrNotFound.
func loadShift(ctx context.Context, q Querier, query string, args ...any) (*Shift, error) {
	s, err := scanShift(q.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get shift: %w", err)
	}
	seqs, err := sequences(ctx, q, []string{s.ID})
	if err != nil {
		return nil, err
	}
	s.Sequences = seqs[s.ID]
	return s, nil
}

func attachSequences(ctx context.Context, q Querier, shifts []Shift) error {
	if len(shifts) == 0 {
		return nil
	}
	ids := make([]string, len(shifts))
	for i := range shifts {
		ids[i] = shifts[i].ID
	}
	seqs, err := sequences(ctx, q, ids)
	if err != nil {
		return err
	}
	for i := range shifts {
		shifts[i].Sequences = seqs[shifts[i].ID]
	}
	return nil
}

// sequences loads the sequences of the given shifts, keyed by shift ID.
// Every requested shift has an entry, empty if it has none.
func sequences(ctx context.Context, q Querier, shiftIDs []string) (map[string][]Sequence, error) {
	out := make(map[string][]Sequence, len(shiftIDs))
	for _, id := range shiftIDs {
		out[id] = []Sequence{}
	}
	rows, err := q.Query(ctx, listSequences, shiftIDs)
	if err != nil {
		return nil, fmt.Errorf("list order sequences: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var s Sequence
		if err := rows.Scan(&id, &s.Prefix, &s.LastValue); err != nil {
			return nil, fmt.Errorf("scan order sequence: %w", err)
		}
		out[id] = append(out[id], s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read order sequences: %w", err)
	}
	return out, nil
}
