package shift

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	// FOR SHARE makes a concurrent close wait until the caller's
	// transaction ends, so the close guard sees the caller's order
	// (FR-CLS-4).
	currentShift = `
		SELECT id::text, business_date::text
		FROM shifts WHERE outlet_id = $1 AND status = 'open'
		FOR SHARE`

	nextNumber = `
		UPDATE order_sequences q
		SET last_value = q.last_value + 1
		FROM shifts s
		WHERE q.shift_id = $1 AND q.prefix = $2 AND s.id = q.shift_id AND s.status = 'open'
		RETURNING q.last_value`

	shiftStatus = `SELECT status FROM shifts WHERE id = $1`
)

// OpenShift is the outlet's open shift as an order placement needs it.
type OpenShift struct {
	ID           string
	OutletID     string
	BusinessDate string
}

// Current returns the outlet's open shift and locks it against closing for
// the rest of the caller's transaction, or ErrNoOpenShift (FR-SEQ-1). Call
// it inside the transaction that places the order.
func Current(ctx context.Context, q Querier, outletID string) (*OpenShift, error) {
	s := &OpenShift{OutletID: outletID}
	err := q.QueryRow(ctx, currentShift, outletID).Scan(&s.ID, &s.BusinessDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoOpenShift
	}
	if err != nil {
		return nil, fmt.Errorf("get open shift: %w", err)
	}
	return s, nil
}

// NextNumber allocates the shift's next order number under prefix, in the
// caller's transaction, and returns the sequence and its display string
// (FR-SEQ-2). A rollback returns the number, so none is burned. It does not
// deduplicate retries: the caller stores the number on its order row and
// allocates only on first insert (FR-SEQ-5). The caller must hold the lock
// from Current on the same shift.
func NextNumber(ctx context.Context, q Querier, shiftID, prefix string) (int, string, error) {
	var seq int
	err := q.QueryRow(ctx, nextNumber, shiftID, prefix).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", noSequence(ctx, q, shiftID, prefix)
	}
	if err != nil {
		return 0, "", fmt.Errorf("allocate order number: %w", err)
	}
	return seq, FormatNumber(prefix, seq), nil
}

// noSequence explains why NextNumber matched no row.
func noSequence(ctx context.Context, q Querier, shiftID, prefix string) error {
	var status string
	err := q.QueryRow(ctx, shiftStatus, shiftID).Scan(&status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get shift status: %w", err)
	}
	if status == StatusClosed {
		return ErrShiftClosed
	}
	return fmt.Errorf("%w: shift %s, prefix %q", ErrUnknownSequence, shiftID, prefix)
}

// FormatNumber renders an order number: the prefix, then the sequence
// zero-padded to at least three digits (FR-SEQ-3).
func FormatNumber(prefix string, seq int) string {
	return fmt.Sprintf("%s-%03d", prefix, seq)
}
