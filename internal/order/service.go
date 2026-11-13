package order

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

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
	Get(ctx context.Context, outletID, orderID string) (*Detail, error)
	List(ctx context.Context, f *ListFilter) ([]Order, error)
	OpenShiftID(ctx context.Context, outletID string) (string, error)
	BusinessToday(ctx context.Context, outletID string, at time.Time) (string, error)
}

type Service struct {
	repo repository
	now  func() time.Time
}

func NewService(repo repository) *Service {
	return &Service{repo: repo, now: time.Now}
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
	d, applied, err := s.repo.Place(ctx, &placeWrite{write: write{at: s.at(), actorID: in.Acting.StaffID}, in: in})
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
	d, applied, err := s.repo.AddLines(ctx, &addWrite{write: write{at: s.at(), actorID: in.Acting.StaffID}, in: in})
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
