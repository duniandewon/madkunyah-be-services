// Package shift opens and closes an outlet's trading shifts and allocates
// the per-shift order numbers (shifts-spec).
package shift

import (
	"context"
	"errors"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/jackc/pgx/v5"
)

const (
	StatusOpen   = "open"
	StatusClosed = "closed"
)

// PrefixWeb numbers online orders; counter orders use the outlet's own
// order_prefix.
const PrefixWeb = "WEB"

// Querier is the caller's transaction (or pool). Orders pass their own
// transaction so the shift lock and the number live and die with the order.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	// ErrNoOpenShift is returned by Current when the outlet has no open
	// shift.
	ErrNoOpenShift = errors.New("no open shift at the outlet")
	// ErrUnknownSequence is returned by NextNumber for a prefix the shift
	// has no sequence for. Sequences are only created at open (FR-SEQ-4).
	ErrUnknownSequence = errors.New("shift has no sequence for the prefix")
	// ErrShiftClosed is returned when an operation needs an open shift and
	// names a closed one.
	ErrShiftClosed = domain.NewError(domain.CodeShiftClosed, "This shift is closed.")
)

// StaffRef names a staff member.
type StaffRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Sequence is how many numbers a shift has allocated under one prefix. It
// is a count of numbers, not a sales figure.
type Sequence struct {
	Prefix    string `json:"prefix"`
	LastValue int    `json:"last_value"`
}

// Shift is a shift as clients see it (FR-RD-4).
type Shift struct {
	ID             string     `json:"id"`
	OutletID       string     `json:"outlet_id"`
	Status         string     `json:"status"`
	BusinessDate   string     `json:"business_date"`
	OpenedAt       time.Time  `json:"opened_at"`
	OpenedBy       StaffRef   `json:"opened_by"`
	DeviceID       string     `json:"device_id"`
	ClosedAt       *time.Time `json:"closed_at"`
	ClosedBy       *StaffRef  `json:"closed_by"`
	ClosedDeviceID *string    `json:"closed_device_id"`
	Sequences      []Sequence `json:"sequences"`
}

// Result is the outcome of an open or close. Applied is false for a retry
// that changed nothing (FR-OPN-2, FR-CLS-5).
type Result struct {
	Applied bool `json:"applied"`
	*Shift
}

// OpenShiftInfo is the detail of a shift_already_open error: the shift the
// POS should adopt.
type OpenShiftInfo struct {
	ID           string    `json:"id"`
	OpenedAt     time.Time `json:"opened_at"`
	OpenedBy     StaffRef  `json:"opened_by"`
	BusinessDate string    `json:"business_date"`
}

// OrderRef is an unfinished order blocking a close (FR-CLS-3).
type OrderRef struct {
	ID          string `json:"id"`
	OrderNumber string `json:"order_number"`
	Status      string `json:"status"`
	Channel     string `json:"channel"`
}

// UnfinishedOrders is the detail of a shift_has_unfinished_orders error.
// Orders holds at most maxBlockingOrders; Count is the total.
type UnfinishedOrders struct {
	Orders []OrderRef `json:"orders"`
	Count  int        `json:"count"`
}

// OrderGuard reports the unfinished orders of a shift, read in the closing
// transaction: up to limit of them, and the total. Orders implements it;
// NoOrders stands in until then.
type OrderGuard interface {
	UnfinishedOrders(ctx context.Context, q Querier, shiftID string, limit int) ([]OrderRef, int, error)
}

// NoOrders is the OrderGuard used until the Orders feature exists: with no
// orders table, no order can block a close.
type NoOrders struct{}

func (NoOrders) UnfinishedOrders(context.Context, Querier, string, int) ([]OrderRef, int, error) {
	return nil, 0, nil
}

// OpenInput is one open request from a device, on behalf of Acting.
type OpenInput struct {
	ID         string
	OutletID   string
	DeviceID   string
	Acting     *domain.StaffAccess
	ApprovedBy string
}

// CloseInput is one close request from a device, on behalf of Acting.
type CloseInput struct {
	ShiftID    string
	OutletID   string
	DeviceID   string
	Acting     *domain.StaffAccess
	ApprovedBy string
}

// ListFilter selects an outlet's shifts by business date, newest first.
type ListFilter struct {
	OutletID string
	From, To string // inclusive YYYY-MM-DD, "" for open-ended
	Status   string // "" for any
	Limit    int
	After    *cursor
}

// Page is one page of a shift listing.
type Page struct {
	Shifts     []Shift `json:"shifts"`
	NextCursor *string `json:"next_cursor"`
}
