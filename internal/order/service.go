package order

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/authz"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
	// maxRangeDays caps a dashboard listing's business-date range (FR-RD-3).
	maxRangeDays = 92
	dateLayout   = "2006-01-02"
)

type repository interface {
	Place(ctx context.Context, w *placeWrite) (*Detail, bool, error)
	AddLines(ctx context.Context, w *addWrite) (*Detail, bool, error)
	VoidLine(ctx context.Context, w *voidWrite) (*Detail, bool, error)
	Cancel(ctx context.Context, w *cancelWrite) (*Detail, bool, error)
	Pay(ctx context.Context, w *payWrite) (*Detail, bool, error)
	Control(ctx context.Context, w *controlWrite) (*Detail, bool, error)
	Get(ctx context.Context, outletID, orderID string) (*Detail, error)
	List(ctx context.Context, f *ListFilter) ([]Order, error)
	OpenShiftID(ctx context.Context, outletID string) (string, error)
	BusinessToday(ctx context.Context, outletID string, at time.Time) (string, error)
}

type Service struct {
	repo   repository
	access authz.AccessSource
	now    func() time.Time
}

// NewService takes the access source used to check step-up approvers
// (pos-controls FR-STP-1).
func NewService(repo repository, access authz.AccessSource) *Service {
	return &Service{repo: repo, access: access, now: time.Now}
}

// at is the server time of a change, at the database's precision, so what
// is returned is what is stored (FR-ORD-5).
func (s *Service) at() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// authorize checks the acting staff member's permission at the device's
// outlet, server side (§3.8).
func authorize(a *Actor, perm string) error {
	if a.Acting == nil || !a.Acting.Can(a.OutletID, perm) {
		return domain.ErrForbidden
	}
	return nil
}

// Place processes a ticket into a placed, numbered order (FR-PLC).
func (s *Service) Place(ctx context.Context, in *PlaceInput) (*Result, error) {
	if err := authorize(&in.Actor, domain.PermOrderCreate); err != nil {
		return nil, err
	}
	if err := checkLineIDs(in.Lines); err != nil {
		return nil, err
	}
	w := &placeWrite{write: write{at: s.at(), actorID: in.Acting.StaffID}, in: in}
	ap := s.newApprovals(&in.Actor)
	w.stepUp = approveLines(ctx, ap, in.Lines)
	if w.stepUp == nil && in.Discount != nil {
		in.Discount.approver, w.stepUp = ap.approve(ctx, domain.PermOrderDiscount, in.Discount.ApprovedBy)
	}
	d, applied, err := s.repo.Place(ctx, w)
	return result(d, applied, err)
}

// AddLines adds lines to an open bill (FR-ADD).
func (s *Service) AddLines(ctx context.Context, in *AddLinesInput) (*Result, error) {
	if err := authorize(&in.Actor, domain.PermOrderCreate); err != nil {
		return nil, err
	}
	if err := checkLineIDs(in.Lines); err != nil {
		return nil, err
	}
	w := &addWrite{write: write{at: s.at(), actorID: in.Acting.StaffID}, in: in}
	w.stepUp = approveLines(ctx, s.newApprovals(&in.Actor), in.Lines)
	d, applied, err := s.repo.AddLines(ctx, w)
	return result(d, applied, err)
}

// VoidLine removes one line from an open bill (FR-VOID).
func (s *Service) VoidLine(ctx context.Context, in *VoidInput) (*Result, error) {
	if err := authorize(&in.Actor, domain.PermOrderVoidUnpaid); err != nil {
		return nil, err
	}
	d, applied, err := s.repo.VoidLine(ctx, &voidWrite{write: write{at: s.at(), actorID: in.Acting.StaffID}, in: in})
	return result(d, applied, err)
}

// Cancel ends an open bill without payment (FR-CAN).
func (s *Service) Cancel(ctx context.Context, in *CancelInput) (*Result, error) {
	if err := authorize(&in.Actor, domain.PermOrderVoidUnpaid); err != nil {
		return nil, err
	}
	d, applied, err := s.repo.Cancel(ctx, &cancelWrite{write: write{at: s.at(), actorID: in.Acting.StaffID}, in: in})
	return result(d, applied, err)
}

// Pay records the payment that completes an order (FR-PAY).
func (s *Service) Pay(ctx context.Context, in *PayInput) (*Result, error) {
	if err := authorize(&in.Actor, domain.PermPaymentRecord); err != nil {
		return nil, err
	}
	d, applied, err := s.repo.Pay(ctx, &payWrite{write: write{at: s.at(), actorID: in.Acting.StaffID}, in: in})
	return result(d, applied, err)
}

// SetOrderDiscount applies or replaces the order discount (FR-OPN-1).
func (s *Service) SetOrderDiscount(ctx context.Context, in *ControlInput) (*Result, error) {
	return s.control(ctx, in, controlOrderDiscount, domain.PermOrderDiscount, in.Discount.ApprovedBy)
}

// RemoveOrderDiscount removes the order discount; removing needs the same
// permission as applying (FR-STP-3).
func (s *Service) RemoveOrderDiscount(ctx context.Context, in *ControlInput) (*Result, error) {
	return s.control(ctx, in, controlOrderDiscount, domain.PermOrderDiscount, in.ApprovedBy)
}

// SetLineDiscount applies or replaces a line's discount.
func (s *Service) SetLineDiscount(ctx context.Context, in *ControlInput) (*Result, error) {
	return s.control(ctx, in, controlLineDiscount, domain.PermOrderDiscount, in.Discount.ApprovedBy)
}

// RemoveLineDiscount removes a line's discount.
func (s *Service) RemoveLineDiscount(ctx context.Context, in *ControlInput) (*Result, error) {
	return s.control(ctx, in, controlLineDiscount, domain.PermOrderDiscount, in.ApprovedBy)
}

// SetPrice applies or replaces a line's price override.
func (s *Service) SetPrice(ctx context.Context, in *ControlInput) (*Result, error) {
	return s.control(ctx, in, controlPrice, domain.PermOrderPriceOverride, in.Override.ApprovedBy)
}

// RemovePrice restores a line's quoted price.
func (s *Service) RemovePrice(ctx context.Context, in *ControlInput) (*Result, error) {
	return s.control(ctx, in, controlPrice, domain.PermOrderPriceOverride, in.ApprovedBy)
}

// control checks the step-up before the transaction (FR-STP-5); the
// repository returns a failure only once it knows the request changes
// something, so a no-op answers without it (FR-STP-6).
func (s *Service) control(ctx context.Context, in *ControlInput, kind controlKind, perm, approvedBy string) (*Result, error) {
	if in.Acting == nil {
		return nil, domain.ErrForbidden
	}
	w := &controlWrite{write: write{at: s.at(), actorID: in.Acting.StaffID}, in: in, kind: kind}
	w.approver, w.stepUp = s.newApprovals(&in.Actor).approve(ctx, perm, approvedBy)
	d, applied, err := s.repo.Control(ctx, w)
	return result(d, applied, err)
}

// approveLines checks every control on the lines, one approval per control
// (FR-STP-4), and records each approver. It returns the first failure.
func approveLines(ctx context.Context, ap *approvals, lines []LineInput) error {
	var err error
	for i := range lines {
		l := &lines[i]
		if l.Override != nil {
			if l.Override.approver, err = ap.approve(ctx, domain.PermOrderPriceOverride, l.Override.ApprovedBy); err != nil {
				return err
			}
		}
		if l.Discount != nil {
			if l.Discount.approver, err = ap.approve(ctx, domain.PermOrderDiscount, l.Discount.ApprovedBy); err != nil {
				return err
			}
		}
	}
	return nil
}

// approvals checks step-ups for one request, resolving each distinct
// approver at most once (FR-STP-5).
type approvals struct {
	actor *Actor
	src   *memoAccess
}

func (s *Service) newApprovals(a *Actor) *approvals {
	return &approvals{actor: a, src: &memoAccess{src: s.access, seen: map[string]*domain.StaffAccess{}}}
}

func (p *approvals) approve(ctx context.Context, perm, approvedBy string) (string, error) {
	return authz.Approver(ctx, p.src, p.actor.Acting, p.actor.OutletID, perm, approvedBy)
}

// memoAccess remembers each staff member's access for one request.
type memoAccess struct {
	src  authz.AccessSource
	seen map[string]*domain.StaffAccess
}

func (m *memoAccess) Access(ctx context.Context, id string) (*domain.StaffAccess, error) {
	if a, ok := m.seen[id]; ok {
		return a, nil
	}
	a, err := m.src.Access(ctx, id)
	if err != nil {
		return nil, err
	}
	m.seen[id] = a
	return a, nil
}

func result(d *Detail, applied bool, err error) (*Result, error) {
	if err != nil {
		return nil, err
	}
	return &Result{Applied: applied, Detail: d}, nil
}

// checkLineIDs refuses a request naming one line ID twice (FR-PLC-2).
func checkLineIDs(lines []LineInput) error {
	seen := make(map[string]bool, len(lines))
	for i, l := range lines {
		if seen[l.ID] {
			return domain.ValidationError(map[string]string{"lines[" + strconv.Itoa(i) + "].id": "is repeated"})
		}
		seen[l.ID] = true
	}
	return nil
}

func (s *Service) Get(ctx context.Context, outletID, orderID string) (*Detail, error) {
	return s.repo.Get(ctx, outletID, orderID)
}

// ListPOS lists the outlet's orders of one shift, by default the open one;
// with no open shift and none named, the list is empty (FR-RD-1).
func (s *Service) ListPOS(ctx context.Context, f *ListFilter) (*Page, error) {
	if f.ShiftID == "" {
		id, err := s.repo.OpenShiftID(ctx, f.OutletID)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return &Page{Orders: []Order{}}, nil
		}
		f.ShiftID = id
	}
	return s.list(ctx, f)
}

// ListDashboard lists the outlet's orders by business date, today by
// default; a range is at most maxRangeDays long (FR-RD-3).
func (s *Service) ListDashboard(ctx context.Context, f *ListFilter) (*Page, error) {
	if f.To == "" {
		today, err := s.repo.BusinessToday(ctx, f.OutletID, s.now())
		if err != nil {
			return nil, err
		}
		f.To = today
	}
	if f.From == "" {
		f.From = f.To
	}
	if err := checkRange(f.From, f.To); err != nil {
		return nil, err
	}
	return s.list(ctx, f)
}

func checkRange(from, to string) error {
	start, err1 := time.Parse(dateLayout, from)
	end, err2 := time.Parse(dateLayout, to)
	if err1 != nil || err2 != nil {
		return domain.ValidationError(map[string]string{"from": "must be a date as YYYY-MM-DD"})
	}
	switch days := int(end.Sub(start).Hours() / 24); {
	case days < 0:
		return domain.ValidationError(map[string]string{"to": "must not be before from"})
	case days > maxRangeDays:
		return domain.ValidationError(map[string]string{
			"to": "must be at most " + strconv.Itoa(maxRangeDays) + " days after from"})
	}
	return nil
}

// list returns one page and the cursor of the next.
func (s *Service) list(ctx context.Context, f *ListFilter) (*Page, error) {
	if f.Limit == 0 {
		f.Limit = defaultPageSize
	}
	// One extra row tells whether another page follows.
	page := *f
	page.Limit = f.Limit + 1
	orders, err := s.repo.List(ctx, &page)
	if err != nil {
		return nil, err
	}
	p := &Page{Orders: orders}
	if len(orders) > f.Limit {
		p.Orders = orders[:f.Limit]
		last := p.Orders[f.Limit-1]
		next := cursor{date: last.BusinessDate, createdAt: last.CreatedAt, id: last.ID}.String()
		p.NextCursor = &next
	}
	return p, nil
}

// cursor is the position after the last order of a page, in listing order.
type cursor struct {
	date      string
	createdAt time.Time
	id        string
}

func (c cursor) String() string {
	raw := c.date + "|" + c.createdAt.UTC().Format(time.RFC3339Nano) + "|" + c.id
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
	return &cursor{date: parts[0], createdAt: at, id: id.String()}, true
}
