package shift

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/authz"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
)

const (
	// maxBlockingOrders caps the orders listed in a blocked close; the
	// count still reports all of them (FR-CLS-3).
	maxBlockingOrders = 50

	defaultPageSize = 50
	maxPageSize     = 200
	// maxRangeDays caps a listing's business-date range (FR-RD-2).
	maxRangeDays = 92
)

type repository interface {
	Open(ctx context.Context, w *openWrite) (*Shift, bool, error)
	Close(ctx context.Context, w *closeWrite) (*Shift, bool, error)
	CurrentShift(ctx context.Context, outletID string) (*Shift, error)
	Get(ctx context.Context, outletID, shiftID string) (*Shift, error)
	List(ctx context.Context, f *ListFilter) ([]Shift, error)
}

type Service struct {
	repo   repository
	access authz.AccessSource
	guard  OrderGuard
	now    func() time.Time
}

// NewService takes the access source used to check step-up approvers and
// the guard that reports a shift's unfinished orders.
func NewService(repo repository, access authz.AccessSource, guard OrderGuard) *Service {
	return &Service{repo: repo, access: access, guard: guard, now: time.Now}
}

// Open opens a shift at the device's outlet. The acting staff member needs
// shift.open there, or an approver who holds it (FR-STP). The shift is
// stamped with server time (FR-SH-4).
func (s *Service) Open(ctx context.Context, in *OpenInput) (*Result, error) {
	approver, err := authz.Approver(ctx, s.access, in.Acting, in.OutletID, domain.PermShiftOpen, in.ApprovedBy)
	if err != nil {
		return nil, err
	}
	sh, applied, err := s.repo.Open(ctx, &openWrite{
		id: in.ID, outletID: in.OutletID, deviceID: in.DeviceID,
		actorID: in.Acting.StaffID, approverID: approver, at: s.now(),
	})
	if err != nil {
		return nil, err
	}
	return &Result{Applied: applied, Shift: sh}, nil
}

// Close closes a shift at the device's outlet, unless it still has
// unfinished orders (FR-CLS-3). The acting staff member needs shift.close
// there, or an approver who holds it.
func (s *Service) Close(ctx context.Context, in *CloseInput) (*Result, error) {
	approver, err := authz.Approver(ctx, s.access, in.Acting, in.OutletID, domain.PermShiftClose, in.ApprovedBy)
	if err != nil {
		return nil, err
	}
	sh, applied, err := s.repo.Close(ctx, &closeWrite{
		shiftID: in.ShiftID, outletID: in.OutletID, deviceID: in.DeviceID,
		actorID: in.Acting.StaffID, approverID: approver, at: s.now(),
		guard: func(ctx context.Context, q Querier) error { return s.checkOrders(ctx, q, in.ShiftID) },
	})
	if err != nil {
		return nil, err
	}
	return &Result{Applied: applied, Shift: sh}, nil
}

// checkOrders rejects a close while the shift has unfinished orders,
// listing them so staff can finish or cancel them.
func (s *Service) checkOrders(ctx context.Context, q Querier, shiftID string) error {
	orders, count, err := s.guard.UnfinishedOrders(ctx, q, shiftID, maxBlockingOrders)
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if len(orders) > maxBlockingOrders {
		orders = orders[:maxBlockingOrders]
	}
	return &domain.Error{
		Code:    domain.CodeShiftUnfinishedOrder,
		Message: "This shift still has unfinished orders.",
		Details: UnfinishedOrders{Orders: orders, Count: count},
	}
}

// Current returns the outlet's open shift, or nil.
func (s *Service) Current(ctx context.Context, outletID string) (*Shift, error) {
	return s.repo.CurrentShift(ctx, outletID)
}

func (s *Service) Get(ctx context.Context, outletID, shiftID string) (*Shift, error) {
	return s.repo.Get(ctx, outletID, shiftID)
}

// List returns one page of the outlet's shifts and the cursor of the next.
func (s *Service) List(ctx context.Context, f *ListFilter) (*Page, error) {
	if f.Limit == 0 {
		f.Limit = defaultPageSize
	}
	// One extra row tells whether another page follows.
	page := *f
	page.Limit = f.Limit + 1
	shifts, err := s.repo.List(ctx, &page)
	if err != nil {
		return nil, err
	}
	p := &Page{Shifts: shifts}
	if len(shifts) > f.Limit {
		p.Shifts = shifts[:f.Limit]
		last := p.Shifts[f.Limit-1]
		next := cursor{date: last.BusinessDate, openedAt: last.OpenedAt, id: last.ID}.String()
		p.NextCursor = &next
	}
	return p, nil
}

// cursor is the position after the last shift of a page, in listing order.
type cursor struct {
	date     string
	openedAt time.Time
	id       string
}

func (c cursor) String() string {
	raw := c.date + "|" + c.openedAt.UTC().Format(time.RFC3339Nano) + "|" + c.id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// parseCursor decodes a cursor produced by String.
func parseCursor(s string) (*cursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return nil, false
	}
	if _, err := time.Parse(dateLayout, parts[0]); err != nil {
		return nil, false
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return nil, false
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return nil, false
	}
	return &cursor{date: parts[0], openedAt: at, id: id.String()}, true
}
