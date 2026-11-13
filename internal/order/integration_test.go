package order

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/duniandewon/madkunyah-be-service/internal/shift"
	"github.com/duniandewon/madkunyah-be-service/migrations"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Integration tests run against a database named by TEST_DATABASE_URL,
// which they migrate. Every test creates its own outlet, staff, device and
// catalog, so they share one database and run in any order.

var testDB *postgres.DB

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	db, err := postgres.Connect(ctx, postgres.Config{DSN: dsn, MaxConns: 40})
	if err != nil {
		panic(err)
	}
	if err := postgres.SetMigrations(migrations.FS, "."); err != nil {
		panic(err)
	}
	if err := db.Migrate(ctx); err != nil {
		panic(err)
	}
	testDB = db
	code := m.Run()
	db.Close()
	os.Exit(code)
}

type fakeAccess map[string]*domain.StaffAccess

func (f fakeAccess) Access(_ context.Context, id string) (*domain.StaffAccess, error) {
	if a, ok := f[id]; ok {
		return a, nil
	}
	return &domain.StaffAccess{StaffID: id}, nil
}

var orderPerms = []string{domain.PermOrderCreate, domain.PermOrderVoidUnpaid, domain.PermPaymentRecord}

// env is one outlet with a device, staff, a small catalog, and the order
// and shift services.
type env struct {
	t       *testing.T
	ctx     context.Context
	svc     *Service
	shifts  *shift.Service
	access  fakeAccess
	outlet  string
	device  string
	cashier *domain.StaffAccess
	// manager opens and closes shifts.
	manager *domain.StaffAccess
	// catalog
	category, kopi, large, tea, sugarGroup, lessSugar, extraShot uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if testDB == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	e := &env{t: t, ctx: context.Background(), access: fakeAccess{}}
	e.svc = NewService(NewRepository(testDB))
	e.shifts = shift.NewService(shift.NewRepository(testDB), e.access, Guard{})
	e.outlet = e.newOutlet()
	e.device = e.newDevice(e.outlet)
	e.cashier = e.staff("Rina", e.outlet, domain.RoleCashier, orderPerms...)
	e.manager = e.staff("Manager", e.outlet, domain.RoleManager,
		append([]string{domain.PermShiftOpen, domain.PermShiftClose}, orderPerms...)...)
	e.seedCatalog()
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := testDB.Pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", strings.Fields(sql)[0:3], err)
	}
}

func (e *env) newOutlet() string {
	id := uuid.NewString()
	e.exec(`INSERT INTO outlets (id, name, timezone, business_day_cutoff) VALUES ($1, $2, 'Asia/Jakarta', '04:00')`,
		id, "Outlet "+id[:8])
	return id
}

func (e *env) newDevice(outletID string) string {
	id := uuid.NewString()
	e.exec(`INSERT INTO devices (id, outlet_id, name) VALUES ($1, $2, 'POS')`, id, outletID)
	return id
}

func (e *env) staff(name, outletID, role string, perms ...string) *domain.StaffAccess {
	id := uuid.NewString()
	e.exec(`INSERT INTO staff (id, full_name, email) VALUES ($1, $2, $3)`, id, name, id+"@test.local")
	a := &domain.StaffAccess{StaffID: id, Active: true, Outlets: map[string]domain.AccessScope{
		outletID: {Roles: []string{role}, Permissions: perms}}}
	e.access[id] = a
	return a
}

// seedCatalog creates Kopi (20,000, cost 8,000; Regular +0 / Large +5,000;
// optional Sugar group: Less sugar +0, Extra shot +3,000) and Tea (10,000).
func (e *env) seedCatalog() {
	e.category, e.kopi, e.tea = uuid.New(), uuid.New(), uuid.New()
	e.large, e.sugarGroup, e.lessSugar, e.extraShot = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tag := e.category.String()[:8]
	e.exec(`INSERT INTO categories (id, name) VALUES ($1, $2)`, e.category, "Coffee "+tag)
	e.exec(`INSERT INTO products (id, category_id, name, base_price, cost_price) VALUES ($1, $2, 'Es Kopi Susu', 20000, 8000)`,
		e.kopi, e.category)
	e.exec(`INSERT INTO products (id, category_id, name, base_price) VALUES ($1, $2, 'Teh', 10000)`, e.tea, e.category)
	e.exec(`INSERT INTO product_variants (id, product_id, name, price_delta) VALUES ($1, $2, 'Regular', 0)`,
		uuid.New(), e.kopi)
	e.exec(`INSERT INTO product_variants (id, product_id, name, price_delta) VALUES ($1, $2, 'Large', 5000)`,
		e.large, e.kopi)
	e.exec(`INSERT INTO modifier_groups (id, name, min_select, max_select) VALUES ($1, 'Sugar', 0, 2)`, e.sugarGroup)
	e.exec(`INSERT INTO modifiers (id, modifier_group_id, name, price_delta) VALUES ($1, $2, 'Less sugar', 0)`,
		e.lessSugar, e.sugarGroup)
	e.exec(`INSERT INTO modifiers (id, modifier_group_id, name, price_delta) VALUES ($1, $2, 'Extra shot', 3000)`,
		e.extraShot, e.sugarGroup)
	e.exec(`INSERT INTO product_modifier_groups (product_id, modifier_group_id) VALUES ($1, $2)`, e.kopi, e.sugarGroup)
}

// kopi is a Large Kopi with both modifiers: 28,000 a cup.
func (e *env) kopiLine(qty int, f string) LineInput {
	v := e.large
	return LineInput{ID: uuid.NewString(), ProductID: e.kopi, VariantID: &v,
		ModifierIDs: []uuid.UUID{e.lessSugar, e.extraShot}, Quantity: qty, FulfillmentType: f}
}

func (e *env) teaLine(qty int, f string) LineInput {
	return LineInput{ID: uuid.NewString(), ProductID: e.tea, Quantity: qty, FulfillmentType: f}
}

func (e *env) actor(a *domain.StaffAccess) Actor {
	return Actor{OutletID: e.outlet, DeviceID: e.device, Acting: a}
}

func (e *env) openShift() *shift.Result {
	e.t.Helper()
	res, err := e.shifts.Open(e.ctx, &shift.OpenInput{ID: uuid.NewString(), OutletID: e.outlet, DeviceID: e.device,
		Acting: e.manager})
	if err != nil {
		e.t.Fatalf("open shift: %v", err)
	}
	return res
}

func (e *env) closeShift(id string) (*shift.Result, error) {
	return e.shifts.Close(e.ctx, &shift.CloseInput{ShiftID: id, OutletID: e.outlet, DeviceID: e.device,
		Acting: e.manager})
}

func (e *env) placeID(id string, lines ...LineInput) (*Result, error) {
	return e.svc.Place(e.ctx, &PlaceInput{Actor: e.actor(e.cashier), ID: id, Lines: lines})
}

// place places an order of lines, or a Tea, and fails the test on error.
func (e *env) place(lines ...LineInput) *Result {
	e.t.Helper()
	if len(lines) == 0 {
		lines = []LineInput{e.teaLine(1, FulfillmentDineIn)}
	}
	res, err := e.placeID(uuid.NewString(), lines...)
	if err != nil {
		e.t.Fatalf("place: %v", err)
	}
	return res
}

func (e *env) add(orderID string, lines ...LineInput) (*Result, error) {
	return e.svc.AddLines(e.ctx, &AddLinesInput{Actor: e.actor(e.cashier), OrderID: orderID, Lines: lines})
}

func (e *env) void(orderID, lineID, reason string) (*Result, error) {
	return e.svc.VoidLine(e.ctx, &VoidInput{Actor: e.actor(e.cashier), OrderID: orderID, LineID: lineID, Reason: reason})
}

func (e *env) cancel(orderID, reason string) (*Result, error) {
	return e.svc.Cancel(e.ctx, &CancelInput{Actor: e.actor(e.cashier), OrderID: orderID, Reason: reason})
}

func (e *env) pay(orderID, paymentID string, amount int64) (*Result, error) {
	return e.svc.Pay(e.ctx, &PayInput{Actor: e.actor(e.cashier), OrderID: orderID, PaymentID: paymentID,
		Method: MethodQRISStatic, Amount: amount})
}

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := testDB.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func wantCode(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	_ = wantErr(t, err, code)
}

// wantErr is wantCode for callers that go on to inspect the error.
func wantErr(t *testing.T, err error, code domain.ErrorCode) *domain.Error {
	t.Helper()
	de := domain.AsError(err)
	if de == nil || de.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
	return de
}

type outboxRow struct {
	EventType string
	Payload   map[string]any
}

func outbox(t *testing.T, orderID string) []outboxRow {
	t.Helper()
	rows, err := testDB.Pool.Query(context.Background(),
		`SELECT event_type, payload FROM outbox_events WHERE aggregate_type = 'order' AND aggregate_id = $1
		 ORDER BY created_at, event_type`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[outboxRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type auditRow struct {
	Action     string
	OutletID   string
	ActorID    string
	ApprovedBy string
	ValueDelta int64
	Reason     string
	After      map[string]any
}

func audits(t *testing.T, orderID string) []auditRow {
	t.Helper()
	rows, err := testDB.Pool.Query(context.Background(),
		`SELECT action, outlet_id::text, actor_id::text, approved_by::text, value_delta, reason, after
		 FROM audit_logs WHERE entity_type = 'order' AND entity_id = $1 ORDER BY created_at`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[auditRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func events(t *testing.T, orderID string) int {
	return count(t, `SELECT count(*) FROM order_status_events WHERE order_id = $1`, orderID)
}

func sequence(t *testing.T, shiftID string) int {
	return count(t, `SELECT last_value FROM order_sequences WHERE shift_id = $1 AND prefix = 'POS'`, shiftID)
}

// AC-1, AC-2, AC-8, E-9: placement stores the Quote snapshot, numbers the
// order in its shift, and a retry changes nothing.
func TestPlace(t *testing.T) {
	e := newEnv(t)
	sh := e.openShift()
	note := "no ice"
	kopi := e.kopiLine(2, FulfillmentDineIn)
	kopi.Note = &note
	teaIn, teaOut := e.teaLine(1, FulfillmentDineIn), e.teaLine(3, FulfillmentTakeaway)
	id := uuid.NewString()
	table := "12"

	res, err := e.svc.Place(e.ctx, &PlaceInput{Actor: e.actor(e.cashier), ID: id, TableNumber: &table,
		Lines: []LineInput{kopi, teaIn, teaOut}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.ID != id || res.Status != StatusPlaced || res.OrderNumber != "POS-001" ||
		res.Channel != ChannelPOS || res.ShiftID != sh.ID || res.BusinessDate != sh.BusinessDate ||
		res.OutletID != e.outlet || *res.DeviceID != e.device || *res.TableNumber != "12" ||
		res.FulfillmentSummary != FulfillmentMixed || res.PlacedAt == nil || res.PaidAt != nil ||
		*res.CreatedBy != (StaffRef{ID: e.cashier.StaffID, Name: "Rina"}) {
		t.Fatalf("placed = %+v", res.Order)
	}
	if want := int64(2*28000 + 10000 + 3*10000); res.Subtotal != want || res.GrandTotal != want {
		t.Fatalf("totals = %d / %d, want %d", res.Subtotal, res.GrandTotal, want)
	}
	if len(res.Lines) != 3 || res.Lines[0].ID != kopi.ID || res.Lines[1].ID != teaIn.ID || res.Lines[2].ID != teaOut.ID {
		t.Fatalf("lines = %+v", res.Lines)
	}
	l := res.Lines[0]
	if l.ProductName != "Es Kopi Susu" || *l.VariantName != "Large" || l.CategoryID != e.category.String() ||
		!strings.HasPrefix(l.CategoryName, "Coffee") || l.UnitPrice != 28000 || l.LineTotal != 56000 ||
		l.Quantity != 2 || *l.Note != "no ice" || l.Status != LineActive || l.CreatedBy.ID != e.cashier.StaffID ||
		len(l.Modifiers) != 2 || l.Modifiers[0].Name != "Extra shot" || l.Modifiers[0].PriceDelta != 3000 {
		t.Fatalf("kopi line = %+v", l)
	}
	// AC-8: the same product twice, kept as two lines.
	if res.Lines[1].ProductID != res.Lines[2].ProductID || res.Lines[1].FulfillmentType == res.Lines[2].FulfillmentType {
		t.Fatalf("tea lines = %+v", res.Lines[1:])
	}
	if c := count(t, `SELECT unit_cost FROM order_lines WHERE id = $1`, kopi.ID); c != 8000 {
		t.Fatalf("unit_cost = %d", c)
	}
	if len(res.Events) != 1 || res.Events[0].From != nil || res.Events[0].To != StatusPlaced ||
		res.Events[0].Staff.ID != e.cashier.StaffID || len(res.Payments) != 0 {
		t.Fatalf("events = %+v, payments = %+v", res.Events, res.Payments)
	}
	ob := outbox(t, id)
	if len(ob) != 1 || ob[0].EventType != "order.placed" || ob[0].Payload["v"] != float64(1) ||
		ob[0].Payload["order_number"] != "POS-001" || ob[0].Payload["grand_total"] != float64(res.GrandTotal) ||
		ob[0].Payload["shift_id"] != sh.ID || ob[0].Payload["status"] != StatusPlaced {
		t.Fatalf("outbox = %+v", ob)
	}

	// AC-2: a retry, even with different lines (E-4), returns the stored order.
	again, err := e.placeID(id, e.teaLine(9, FulfillmentDineIn))
	if err != nil || again.Applied || again.OrderNumber != "POS-001" || len(again.Lines) != 3 {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if n := count(t, `SELECT count(*) FROM orders WHERE outlet_id = $1`, e.outlet); n != 1 ||
		events(t, id) != 1 || len(outbox(t, id)) != 1 || sequence(t, sh.ID) != 1 {
		t.Fatalf("retry wrote: %d orders, %d events, sequence %d", n, events(t, id), sequence(t, sh.ID))
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE entity_id = $1`, id); n != 0 {
		t.Fatalf("placement audited: %d rows", n)
	}

	// E-12: the same ID at another outlet.
	other := newEnv(t)
	other.openShift()
	_, err = other.placeID(id, other.teaLine(1, FulfillmentDineIn))
	wantCode(t, err, domain.CodeConflict)
}

// AC-3, AC-4, E-5: refused placements write nothing and burn no number.
func TestPlaceRefused(t *testing.T) {
	e := newEnv(t)
	_, err := e.placeID(uuid.NewString(), e.teaLine(1, FulfillmentDineIn))
	wantCode(t, err, domain.CodeNoOpenShift)

	sh := e.openShift()
	e.place()
	e.exec(`INSERT INTO outlet_sold_out (id, outlet_id, product_id, is_sold_out, changed_at, changed_by, changed_via)
	        VALUES ($1, $2, $3, true, now(), $4, 'dashboard')`, uuid.New(), e.outlet, e.tea, e.manager.StaffID)
	kopi, tea := e.kopiLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn)
	id := uuid.NewString()
	_, err = e.placeID(id, kopi, tea)
	d, ok := wantErr(t, err, domain.CodeLinesUnavailable).Details.(LinesUnavailable)
	if !ok || len(d.Lines) != 1 || d.Lines[0].ID != tea.ID || d.Lines[0].Kind != "product_sold_out" ||
		d.Lines[0].TargetID != nil {
		t.Fatalf("details = %+v", d)
	}
	// A problem naming its target: a variant of another product.
	bad := e.teaLine(1, FulfillmentDineIn)
	bad.ProductID = e.kopi
	_, err = e.placeID(id, bad)
	d = wantErr(t, err, domain.CodeLinesUnavailable).Details.(LinesUnavailable)
	if len(d.Lines) != 1 || d.Lines[0].Kind != "variant_required" {
		t.Fatalf("details = %+v", d)
	}
	if n := count(t, `SELECT count(*) FROM orders WHERE id = $1`, id); n != 0 || sequence(t, sh.ID) != 1 {
		t.Fatalf("refused placement wrote: %d orders, sequence %d", n, sequence(t, sh.ID))
	}

	e.exec(`UPDATE outlet_sold_out SET is_sold_out = false WHERE outlet_id = $1`, e.outlet)
	if res := e.place(kopi, tea); res.OrderNumber != "POS-002" {
		t.Fatalf("next number = %s", res.OrderNumber)
	}
}

// AC-7, E-10, E-13: a price change reaches only lines added afterwards.
func TestPriceChange(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	res := e.place(e.teaLine(1, FulfillmentDineIn))
	e.exec(`UPDATE products SET base_price = 12000 WHERE id = $1`, e.tea)
	res, err := e.add(res.ID, e.teaLine(1, FulfillmentTakeaway))
	if err != nil {
		t.Fatal(err)
	}
	if res.Lines[0].UnitPrice != 10000 || res.Lines[1].UnitPrice != 12000 || res.GrandTotal != 22000 ||
		res.FulfillmentSummary != FulfillmentMixed || res.OrderNumber != "POS-001" {
		t.Fatalf("after price change = %+v", res)
	}
}

// AC-9: concurrent placements get distinct, gapless numbers; concurrent
// placements of one ID make one order.
func TestConcurrentPlacement(t *testing.T) {
	e := newEnv(t)
	sh := e.openShift()
	const n = 20
	numbers := make(chan string, n)
	errs := make(chan error, 2*n)
	for range n {
		go func() {
			res, err := e.placeID(uuid.NewString(), e.teaLine(1, FulfillmentDineIn))
			if err != nil {
				errs <- err
				numbers <- ""
			} else {
				numbers <- res.OrderNumber
			}
		}()
	}
	seen := map[string]bool{}
	for range n {
		seen[<-numbers] = true
	}
	for i := 1; i <= n; i++ {
		if !seen[shift.FormatNumber("POS", i)] {
			t.Fatalf("numbers = %v", seen)
		}
	}

	id := uuid.NewString()
	applied := make(chan bool, n)
	for range n {
		go func() {
			res, err := e.placeID(id, e.teaLine(1, FulfillmentDineIn))
			if err != nil {
				errs <- err
				applied <- false
				return
			}
			applied <- res.Applied
		}()
	}
	created := 0
	for range n {
		if <-applied {
			created++
		}
	}
	close(errs)
	for err := range errs {
		t.Errorf("place: %v", err)
	}
	if created != 1 || count(t, `SELECT count(*) FROM orders WHERE id = $1`, id) != 1 || sequence(t, sh.ID) != n+1 {
		t.Fatalf("same ID: %d applied, sequence %d", created, sequence(t, sh.ID))
	}
}

// AC-10, E-14: add lines, retry, and a mix of new and known IDs.
func TestAddLines(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	o := e.place()
	other := e.place()
	kopi := e.kopiLine(1, FulfillmentTakeaway)

	res, err := e.svc.AddLines(e.ctx, &AddLinesInput{Actor: e.actor(e.manager), OrderID: o.ID, Lines: []LineInput{kopi}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || len(res.Lines) != 2 || res.GrandTotal != 38000 || res.FulfillmentSummary != FulfillmentMixed ||
		res.Lines[1].CreatedBy.ID != e.manager.StaffID || res.Lines[0].CreatedBy.ID != e.cashier.StaffID {
		t.Fatalf("added = %+v", res)
	}
	ob := outbox(t, o.ID)
	if len(ob) != 2 || ob[1].EventType != "order.updated" || ob[1].Payload["grand_total"] != float64(38000) ||
		fmt.Sprint(ob[1].Payload["line_ids"]) != "["+kopi.ID+"]" {
		t.Fatalf("outbox = %+v", ob)
	}

	again, err := e.add(o.ID, kopi)
	if err != nil || again.Applied || len(again.Lines) != 2 {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	_, err = e.add(o.ID, kopi, e.teaLine(1, FulfillmentDineIn))
	wantCode(t, err, domain.CodeConflict)
	_, err = e.add(other.ID, kopi)
	wantCode(t, err, domain.CodeConflict)
	if len(outbox(t, o.ID)) != 2 || count(t, `SELECT count(*) FROM order_lines WHERE order_id = $1`, o.ID) != 2 {
		t.Fatal("rejected adds wrote rows")
	}

	// The 100-line cap counts lines already on the order.
	many := make([]LineInput, 98)
	for i := range many {
		many[i] = e.teaLine(1, FulfillmentDineIn)
	}
	if _, err := e.add(o.ID, many...); err != nil {
		t.Fatal(err)
	}
	_, err = e.add(o.ID, e.teaLine(1, FulfillmentDineIn))
	wantCode(t, err, domain.CodeInvalidInput)

	_, err = e.add(uuid.NewString(), e.teaLine(1, FulfillmentDineIn))
	wantCode(t, err, domain.CodeNotFound)
}

// AC-11, AC-12, E-15, E-16, E-21: voiding a line.
func TestVoidLine(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	kopi, tea := e.kopiLine(2, FulfillmentDineIn), e.teaLine(1, FulfillmentTakeaway)
	o := e.place(kopi, tea)
	other := e.place()

	_, err := e.void(o.ID, other.Lines[0].ID, "wrong order")
	wantCode(t, err, domain.CodeNotFound)

	res, err := e.void(o.ID, kopi.ID, "customer changed mind")
	if err != nil {
		t.Fatal(err)
	}
	l := res.Lines[0]
	if !res.Applied || l.Status != LineVoided || *l.VoidReason != "customer changed mind" || l.VoidedAt == nil ||
		l.VoidedBy.ID != e.cashier.StaffID || res.Subtotal != 10000 || res.GrandTotal != 10000 ||
		res.FulfillmentSummary != FulfillmentTakeaway || len(res.Lines) != 2 {
		t.Fatalf("voided = %+v", res)
	}
	rows := audits(t, o.ID)
	if len(rows) != 1 || rows[0].Action != "order.line_voided" || rows[0].ValueDelta != -56000 ||
		rows[0].Reason != "customer changed mind" || rows[0].ActorID != e.cashier.StaffID ||
		rows[0].ApprovedBy != e.cashier.StaffID || rows[0].OutletID != e.outlet ||
		rows[0].After["line_id"] != kopi.ID || rows[0].After["quantity"] != float64(2) ||
		rows[0].After["order_number"] != o.OrderNumber || rows[0].After["product_name"] != "Es Kopi Susu" {
		t.Fatalf("audit = %+v", rows)
	}
	if ob := outbox(t, o.ID); len(ob) != 2 || ob[1].EventType != "order.updated" {
		t.Fatalf("outbox = %+v", ob)
	}

	again, err := e.void(o.ID, kopi.ID, "again")
	if err != nil || again.Applied || len(audits(t, o.ID)) != 1 || len(outbox(t, o.ID)) != 2 {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	_, err = e.void(o.ID, tea.ID, "last one")
	wantCode(t, err, domain.CodeLastActiveLine)
}

// AC-13, E-18: cancelling an open bill.
func TestCancel(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	kopi, tea := e.kopiLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn)
	o := e.place(kopi, tea)
	if _, err := e.void(o.ID, tea.ID, "not wanted"); err != nil {
		t.Fatal(err)
	}

	res, err := e.cancel(o.ID, "customer left")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Status != StatusCancelled || res.GrandTotal != 28000 || len(res.Lines) != 2 ||
		res.Lines[0].Status != LineActive {
		t.Fatalf("cancelled = %+v", res)
	}
	ev := res.Events[len(res.Events)-1]
	if len(res.Events) != 2 || *ev.From != StatusPlaced || ev.To != StatusCancelled || *ev.Reason != "customer left" {
		t.Fatalf("events = %+v", res.Events)
	}
	rows := audits(t, o.ID)
	if len(rows) != 2 || rows[1].Action != "order.cancelled" || rows[1].ValueDelta != -28000 ||
		rows[1].Reason != "customer left" || rows[1].After["active_lines"] != float64(1) ||
		rows[1].After["order_number"] != o.OrderNumber {
		t.Fatalf("audit = %+v", rows)
	}
	if ob := outbox(t, o.ID); len(ob) != 3 || ob[2].EventType != "order.cancelled" || ob[2].Payload["status"] != "cancelled" {
		t.Fatalf("outbox = %+v", ob)
	}

	again, err := e.cancel(o.ID, "again")
	if err != nil || again.Applied || len(audits(t, o.ID)) != 2 || events(t, o.ID) != 2 {
		t.Fatalf("retry = %+v, %v", again, err)
	}
}

// AC-15, AC-16, AC-17, E-22..E-27: recording the payment.
func TestPay(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	o := e.place(e.kopiLine(1, FulfillmentDineIn))
	pid := uuid.NewString()

	_, err := e.pay(o.ID, pid, 27999)
	if d := wantErr(t, err, domain.CodeAmountMismatch).Details.(map[string]int64); d["grand_total"] != 28000 {
		t.Fatalf("details = %v", d)
	}
	if count(t, `SELECT count(*) FROM payments WHERE order_id = $1`, o.ID) != 0 || events(t, o.ID) != 1 {
		t.Fatal("mismatch wrote rows")
	}

	ref := "RRN123"
	res, err := e.svc.Pay(e.ctx, &PayInput{Actor: e.actor(e.cashier), OrderID: o.ID, PaymentID: pid,
		Method: MethodCardEDC, Amount: 28000, ExternalRef: &ref})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Payments
	if !res.Applied || res.Status != StatusCompleted || res.PaidAt == nil || len(p) != 1 || p[0].ID != pid ||
		p[0].Status != PaymentSettled || p[0].Method != MethodCardEDC || p[0].Amount != 28000 ||
		*p[0].ExternalRef != "RRN123" || p[0].RecordedBy.ID != e.cashier.StaffID || !p[0].SettledAt.Equal(*res.PaidAt) {
		t.Fatalf("paid = %+v", res)
	}
	if ev := res.Events[1]; *ev.From != StatusPlaced || ev.To != StatusCompleted {
		t.Fatalf("events = %+v", res.Events)
	}
	if ob := outbox(t, o.ID); len(ob) != 2 || ob[1].EventType != "order.paid" || ob[1].Payload["status"] != "completed" {
		t.Fatalf("outbox = %+v", ob)
	}

	again, err := e.pay(o.ID, pid, 1)
	if err != nil || again.Applied || len(again.Payments) != 1 {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	_, err = e.pay(o.ID, uuid.NewString(), 28000)
	wantCode(t, err, domain.CodeOrderAlreadyPaid)
	o2 := e.place()
	_, err = e.pay(o2.ID, pid, o2.GrandTotal)
	wantCode(t, err, domain.CodeConflict)

	// E-27: no reference; E-11: a zero total is paid with zero.
	e.exec(`UPDATE products SET base_price = 0 WHERE id = $1`, e.tea)
	free := e.place()
	res, err = e.pay(free.ID, uuid.NewString(), 0)
	if err != nil || res.Payments[0].ExternalRef != nil || res.Status != StatusCompleted {
		t.Fatalf("free order = %+v, %v", res, err)
	}

	// Two concurrent payments with different IDs leave one.
	race := e.place()
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := e.pay(race.ID, uuid.NewString(), race.GrandTotal)
			results <- err
		}()
	}
	failed := 0
	for range 2 {
		if err := <-results; err != nil {
			wantCode(t, err, domain.CodeOrderAlreadyPaid)
			failed++
		}
	}
	if failed != 1 || count(t, `SELECT count(*) FROM payments WHERE order_id = $1`, race.ID) != 1 {
		t.Fatalf("racing payments: %d failed", failed)
	}
}

// AC-18, E-26, E-28: completed and cancelled orders cannot change.
func TestOrderNotOpen(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	paid := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
	if _, err := e.pay(paid.ID, uuid.NewString(), paid.GrandTotal); err != nil {
		t.Fatal(err)
	}
	cancelled := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
	if _, err := e.cancel(cancelled.ID, "test"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		o      *Result
		status string
		err    func(o *Result) error
	}{
		{"add to completed", paid, StatusCompleted, func(o *Result) error {
			_, err := e.add(o.ID, e.teaLine(1, FulfillmentDineIn))
			return err
		}},
		{"void on completed", paid, StatusCompleted, func(o *Result) error {
			_, err := e.void(o.ID, o.Lines[0].ID, "after payment")
			return err
		}},
		{"cancel completed", paid, StatusCompleted, func(o *Result) error {
			_, err := e.cancel(o.ID, "after payment")
			return err
		}},
		{"add to cancelled", cancelled, StatusCancelled, func(o *Result) error {
			_, err := e.add(o.ID, e.teaLine(1, FulfillmentDineIn))
			return err
		}},
		{"void on cancelled", cancelled, StatusCancelled, func(o *Result) error {
			_, err := e.void(o.ID, o.Lines[0].ID, "after cancel")
			return err
		}},
		{"pay cancelled", cancelled, StatusCancelled, func(o *Result) error {
			_, err := e.pay(o.ID, uuid.NewString(), o.GrandTotal)
			return err
		}},
	} {
		de := domain.AsError(tt.err(tt.o))
		if de == nil || de.Code != domain.CodeOrderNotOpen || de.Details.(map[string]string)["status"] != tt.status {
			t.Errorf("%s: %v", tt.name, de)
		}
	}
}

// AC-19, E-20: an add racing a payment ends in one of two outcomes, never
// a completed order paid for less than its total.
func TestAddRacesPayment(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	for range 10 {
		o := e.place()
		addErr, payErr := make(chan error, 1), make(chan error, 1)
		go func() {
			_, err := e.add(o.ID, e.teaLine(1, FulfillmentDineIn))
			addErr <- err
		}()
		go func() {
			_, err := e.pay(o.ID, uuid.NewString(), o.GrandTotal)
			payErr <- err
		}()
		a, p := <-addErr, <-payErr
		final, err := e.svc.Get(e.ctx, e.outlet, o.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case a == nil && p != nil:
			wantCode(t, p, domain.CodeAmountMismatch)
			if final.Status != StatusPlaced || final.GrandTotal != 20000 {
				t.Fatalf("add won: %+v", final.Order)
			}
		case p == nil && a != nil:
			wantCode(t, a, domain.CodeOrderNotOpen)
			if final.Status != StatusCompleted || final.Payments[0].Amount != final.GrandTotal {
				t.Fatalf("payment won: %+v", final)
			}
		default:
			t.Fatalf("add %v, pay %v", a, p)
		}
	}
}

// AC-20, E-29, E-30: open bills block a close until paid or cancelled; the
// guard lists every unfinished status.
func TestCloseGuard(t *testing.T) {
	e := newEnv(t)
	sh := e.openShift()
	o := e.place()
	_, err := e.closeShift(sh.ID)
	d := wantErr(t, err, domain.CodeShiftUnfinishedOrder).Details.(shift.UnfinishedOrders)
	if d.Count != 1 || len(d.Orders) != 1 || d.Orders[0] != (shift.OrderRef{ID: o.ID, OrderNumber: "POS-001",
		Status: StatusPlaced, Channel: ChannelPOS}) {
		t.Fatalf("details = %+v", d)
	}
	if _, err := e.pay(o.ID, uuid.NewString(), o.GrandTotal); err != nil {
		t.Fatal(err)
	}
	if res, err := e.closeShift(sh.ID); err != nil || !res.Applied {
		t.Fatalf("close after payment: %+v, %v", res, err)
	}

	sh = e.openShift()
	o = e.place()
	if _, err := e.closeShift(sh.ID); err == nil {
		t.Fatal("close with an open bill succeeded")
	}
	if _, err := e.cancel(o.ID, "left"); err != nil {
		t.Fatal(err)
	}
	if res, err := e.closeShift(sh.ID); err != nil || !res.Applied {
		t.Fatalf("close after cancel: %+v, %v", res, err)
	}

	// One order in every status, oldest first; the guard lists exactly the
	// unfinished ones.
	sh = e.openShift()
	var want []string
	for _, s := range allStatuses {
		o := e.place()
		e.exec(`UPDATE orders SET status = $2 WHERE id = $1`, o.ID, s)
		if s != StatusCompleted && s != StatusCancelled && s != StatusExpired && s != StatusRefunded {
			want = append(want, s)
		}
	}
	refs, n, err := Guard{}.UnfinishedOrders(e.ctx, testDB.Pool, sh.ID, 3)
	if err != nil || n != len(want) || len(refs) != 3 || refs[0].Status != want[0] || refs[2].Status != want[2] {
		t.Fatalf("guard = %+v, %d, %v (want %v)", refs, n, err, want)
	}
}

// gatedRunner commits only once the gate opens, after signalling that the
// transaction's work is done.
type gatedRunner struct {
	entered, gate chan struct{}
}

func (g gatedRunner) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return testDB.InTx(ctx, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		close(g.entered)
		<-g.gate
		return nil
	})
}

// AC-21, E-8: a close waits behind an uncommitted placement, then lists
// the new order.
func TestCloseWaitsForPlacement(t *testing.T) {
	e := newEnv(t)
	sh := e.openShift()
	g := gatedRunner{entered: make(chan struct{}), gate: make(chan struct{})}
	e.svc = &Service{repo: &Repository{tx: g, pool: testDB.Pool}, now: time.Now}

	placed := make(chan *Result, 1)
	go func() {
		res, err := e.placeID(uuid.NewString(), e.teaLine(1, FulfillmentDineIn))
		if err != nil {
			t.Errorf("place: %v", err)
		}
		placed <- res
	}()
	<-g.entered
	closed := make(chan error, 1)
	go func() {
		_, err := e.closeShift(sh.ID)
		closed <- err
	}()
	select {
	case err := <-closed:
		t.Fatalf("close did not wait: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(g.gate)
	o := <-placed
	select {
	case err := <-closed:
		d := wantErr(t, err, domain.CodeShiftUnfinishedOrder).Details.(shift.UnfinishedOrders)
		if d.Count != 1 || d.Orders[0].ID != o.ID {
			t.Fatalf("details = %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close still blocked")
	}
}

// uncertainRunner reports every commit as uncertain, after committing or
// after rolling back.
type uncertainRunner struct{ commit bool }

func (u uncertainRunner) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if u.commit {
		if err := testDB.InTx(ctx, fn); err != nil {
			return err
		}
		return fmt.Errorf("commit transaction: %w", postgres.ErrCommitUncertain)
	}
	return testDB.InTx(ctx, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return fmt.Errorf("commit transaction: %w", postgres.ErrCommitUncertain)
	})
}

// AC-26: an uncertain commit is 503 order_unavailable, and the retry
// returns the order whether or not the first attempt landed.
func TestCommitUncertain(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	normal := e.svc
	for _, committed := range []bool{true, false} {
		e.svc = &Service{repo: &Repository{tx: uncertainRunner{commit: committed}, pool: testDB.Pool}, now: time.Now}
		id := uuid.NewString()
		body := placeBody(id, `{"id":"`+uuid.NewString()+`","product_id":"`+e.tea.String()+
			`","quantity":1,"fulfillment_type":"dine_in"}`)
		rec := call(e.posRouter(e.outlet, true), "POST", "/v1/pos/orders", e.cashier.StaffID, body)
		if rec.Code != 503 || errorOf(t, rec)["code"] != "order_unavailable" {
			t.Fatalf("committed=%v: %d %s", committed, rec.Code, rec.Body)
		}
		e.svc = normal
		rec = call(e.posRouter(e.outlet, true), "POST", "/v1/pos/orders", e.cashier.StaffID, body)
		want := map[bool]int{true: 200, false: 201}[committed]
		if rec.Code != want || decodeBody(t, rec)["id"] != id {
			t.Fatalf("retry, committed=%v: %d %s", committed, rec.Code, rec.Body)
		}
	}
	if n := count(t, `SELECT count(*) FROM orders WHERE outlet_id = $1`, e.outlet); n != 2 {
		t.Fatalf("%d orders", n)
	}
}

// --- HTTP -----------------------------------------------------------------

type fakeVerifier struct{ p *domain.Principal }

func (f fakeVerifier) Verify(string, ...domain.Audience) (*domain.Principal, error) { return f.p, nil }

type noDenylist struct{}

func (noDenylist) IsTokenDenied(context.Context, string) (bool, error) { return false, nil }

type fakeDevices bool

func (f fakeDevices) DeviceActive(context.Context, string, string) (bool, error) { return bool(f), nil }

// posRouter mounts the POS order routes as the server does, for this
// env's device at outletID.
func (e *env) posRouter(outletID string, active bool) http.Handler {
	h := NewHandler(e.svc)
	p := &domain.Principal{Subject: e.device, Audience: domain.AudiencePOS, OutletID: outletID, DeviceID: e.device}
	r := chi.NewRouter()
	r.Route("/v1/pos", func(r chi.Router) {
		r.Use(middleware.Authenticate(fakeVerifier{p}, noDenylist{}, domain.AudiencePOS),
			middleware.RequireActiveDevice(fakeDevices(active)), middleware.RequireActingStaff(e.access))
		lines := r.With(middleware.MaxBodyBytes(64 << 10))
		small := r.With(middleware.MaxBodyBytes(4 << 10))
		r.Get("/orders", h.POSList)
		r.Get("/orders/{orderID}", h.POSGet)
		lines.Post("/orders", h.POSPlace)
		lines.Post("/orders/{orderID}/lines", h.POSAddLines)
		small.Post("/orders/{orderID}/lines/{lineID}/void", h.POSVoidLine)
		small.Post("/orders/{orderID}/cancel", h.POSCancel)
		small.Post("/orders/{orderID}/payments", h.POSPay)
	})
	return r
}

// dashRouter mounts the dashboard order routes as the server does.
func (e *env) dashRouter(staffID string) http.Handler {
	h := NewHandler(e.svc)
	p := &domain.Principal{Subject: staffID, Audience: domain.AudienceStaff}
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.Authenticate(fakeVerifier{p}, noDenylist{}, domain.AudienceStaff),
			middleware.LoadStaffAccess(e.access), middleware.RequireOutletRole)
		r.Get("/v1/outlets/{outletID}/orders", h.List)
		r.Get("/v1/outlets/{outletID}/orders/{orderID}", h.Get)
	})
	return r
}

func call(h http.Handler, method, path, actingStaff, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	if actingStaff != "" {
		req.Header.Set("X-Acting-Staff-Id", actingStaff)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	return v
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	e, _ := decodeBody(t, rec)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error body: %d %s", rec.Code, rec.Body)
	}
	return e
}

func placeBody(id string, lines ...string) string {
	return `{"id":"` + id + `","lines":[` + strings.Join(lines, ",") + `]}`
}

// AC-14 over HTTP: a void or cancel without a reason writes nothing.
func TestReasonRequired(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	o := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
	h := e.posRouter(e.outlet, true)
	staff := e.cashier.StaffID
	for _, body := range []string{`{}`, `{"reason":""}`, `{"reason":"   "}`, `{"reason":"ab"}`,
		`{"reason":"` + strings.Repeat("x", 281) + `"}`} {
		if rec := call(h, "POST", "/v1/pos/orders/"+o.ID+"/cancel", staff, body); rec.Code != 400 {
			t.Errorf("cancel %s: %d %s", body, rec.Code, rec.Body)
		}
		if rec := call(h, "POST", "/v1/pos/orders/"+o.ID+"/lines/"+o.Lines[0].ID+"/void", staff, body); rec.Code != 400 {
			t.Errorf("void %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	got, _ := e.svc.Get(e.ctx, e.outlet, o.ID)
	if got.Status != StatusPlaced || got.Lines[0].Status != LineActive || len(audits(t, o.ID)) != 0 ||
		len(outbox(t, o.ID)) != 1 {
		t.Fatalf("rejected requests changed the order: %+v", got.Order)
	}
	rec := call(h, "POST", "/v1/pos/orders/"+o.ID+"/cancel", staff, `{"reason":"  left  "}`)
	if rec.Code != 200 || decodeBody(t, rec)["status"] != "cancelled" || audits(t, o.ID)[0].Reason != "left" {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
}

// AC-5: placement input validation.
func TestPlaceValidation(t *testing.T) {
	e := newEnv(t)
	h := e.posRouter(e.outlet, true)
	tea := e.tea.String()
	line := func(fields string) string {
		return `{"id":"` + uuid.NewString() + `","product_id":"` + tea + `","quantity":1,"fulfillment_type":"dine_in"` +
			fields + `}`
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = line("")
	}
	dup := uuid.NewString()
	dupLine := `{"id":"` + dup + `","product_id":"` + tea + `","quantity":1,"fulfillment_type":"dine_in"}`
	id := uuid.NewString()
	for _, tt := range []struct{ name, body, field string }{
		{"no lines", placeBody(id), "lines"},
		{"101 lines", placeBody(id, many...), "lines"},
		{"quantity 0", placeBody(id, strings.Replace(line(""), `"quantity":1`, `"quantity":0`, 1)), "lines[0].quantity"},
		{"quantity 1000", placeBody(id, strings.Replace(line(""), `"quantity":1`, `"quantity":1000`, 1)), "lines[0].quantity"},
		{"unknown fulfillment", placeBody(id, line(""), strings.Replace(line(""), "dine_in", "delivery", 1)),
			"lines[1].fulfillment_type"},
		{"duplicate line IDs", placeBody(id, dupLine, dupLine), "lines[1].id"},
		{"over-long note", placeBody(id, line(`,"note":"`+strings.Repeat("n", 281)+`"`)), "lines[0].note"},
		{"over-long order note", `{"id":"` + id + `","note":"` + strings.Repeat("n", 281) + `","lines":[` + line("") + `]}`, "note"},
		{"long table number", `{"id":"` + id + `","table_number":"` + strings.Repeat("1", 21) + `","lines":[` + line("") + `]}`,
			"table_number"},
		{"bad modifier", placeBody(id, line(`,"modifier_ids":["x"]`)), "lines[0].modifier_ids[0]"},
		{"missing id", `{"lines":[` + line("") + `]}`, "id"},
	} {
		rec := call(h, "POST", "/v1/pos/orders", e.cashier.StaffID, tt.body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s", tt.name, rec.Code, rec.Body)
			continue
		}
		if fields, _ := errorOf(t, rec)["fields"].(map[string]any); fields[tt.field] == nil {
			t.Errorf("%s: fields = %v", tt.name, fields)
		}
	}
	// A price from the client is refused, not ignored (FR-TOT-6).
	if rec := call(h, "POST", "/v1/pos/orders", e.cashier.StaffID, placeBody(id, line(`,"unit_price":1`))); rec.Code != 400 {
		t.Fatalf("client price: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, `SELECT count(*) FROM orders WHERE outlet_id = $1`, e.outlet); n != 0 {
		t.Fatalf("%d orders written", n)
	}
}

// AC-22, E-31: the POS lists its open shift's orders.
func TestPOSList(t *testing.T) {
	e := newEnv(t)
	h := e.posRouter(e.outlet, true)
	list := func(query string) []string {
		t.Helper()
		rec := call(h, "GET", "/v1/pos/orders"+query, e.cashier.StaffID, "")
		if rec.Code != 200 {
			t.Fatalf("list %s: %d %s", query, rec.Code, rec.Body)
		}
		var ids []string
		for _, o := range decodeBody(t, rec)["orders"].([]any) {
			m := o.(map[string]any)
			if m["lines"] != nil || m["payments"] != nil {
				t.Fatalf("list returned children: %v", m)
			}
			ids = append(ids, m["id"].(string))
		}
		return ids
	}
	if got := list("?status=placed"); len(got) != 0 {
		t.Fatalf("no shift: %v", got)
	}
	first := e.openShift()
	old := e.place()
	if _, err := e.pay(old.ID, uuid.NewString(), old.GrandTotal); err != nil {
		t.Fatal(err)
	}
	if _, err := e.closeShift(first.ID); err != nil {
		t.Fatal(err)
	}
	e.openShift()
	a, b, c := e.place(), e.place(), e.place()
	if _, err := e.cancel(b.ID, "left"); err != nil {
		t.Fatal(err)
	}
	if got := list("?status=placed"); fmt.Sprint(got) != fmt.Sprint([]string{c.ID, a.ID}) {
		t.Fatalf("open bills = %v", got)
	}
	if got := list(""); fmt.Sprint(got) != fmt.Sprint([]string{c.ID, b.ID, a.ID}) {
		t.Fatalf("all = %v", got)
	}
	if got := list("?limit=2"); len(got) != 2 {
		t.Fatalf("page = %v", got)
	}
	if got := list("?shift_id=" + first.ID); fmt.Sprint(got) != fmt.Sprint([]string{old.ID}) {
		t.Fatalf("earlier shift = %v", got)
	}
	for _, q := range []string{"?status=draft", "?shift_id=x", "?limit=0", "?cursor=x", "?status=nope"} {
		if rec := call(h, "GET", "/v1/pos/orders"+q, e.cashier.StaffID, ""); rec.Code != 400 {
			t.Errorf("list %s: %d", q, rec.Code)
		}
	}
}

// AC-23: the dashboard list filters by date, status, shift and number.
func TestDashboardList(t *testing.T) {
	e := newEnv(t)
	sh := e.openShift()
	o1, o2 := e.place(), e.place()
	if _, err := e.pay(o1.ID, uuid.NewString(), o1.GrandTotal); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE orders SET business_date = '2026-01-05' WHERE id = $1`, o1.ID)
	e.exec(`UPDATE orders SET business_date = '2026-01-06' WHERE id = $1`, o2.ID)
	h := e.dashRouter(e.cashier.StaffID)
	path := "/v1/outlets/" + e.outlet + "/orders"
	list := func(query string) ([]string, any) {
		t.Helper()
		rec := call(h, "GET", path+query, "", "")
		if rec.Code != 200 {
			t.Fatalf("list %s: %d %s", query, rec.Code, rec.Body)
		}
		var ids []string
		body := decodeBody(t, rec)
		for _, o := range body["orders"].([]any) {
			ids = append(ids, o.(map[string]any)["id"].(string))
		}
		return ids, body["next_cursor"]
	}
	same := func(got []string, want ...string) bool { return fmt.Sprint(got) == fmt.Sprint(want) }
	r := "?from=2026-01-01&to=2026-01-31"

	if got, _ := list(r); !same(got, o2.ID, o1.ID) {
		t.Fatalf("range = %v", got)
	}
	if got, _ := list(""); len(got) != 0 {
		t.Fatalf("today = %v", got)
	}
	if got, _ := list("?from=2026-01-06&to=2026-01-06"); !same(got, o2.ID) {
		t.Fatalf("one day = %v", got)
	}
	if got, _ := list(r + "&status=completed"); !same(got, o1.ID) {
		t.Fatalf("completed = %v", got)
	}
	if got, _ := list(r + "&shift_id=" + sh.ID + "&order_number=POS-002"); !same(got, o2.ID) {
		t.Fatalf("number = %v", got)
	}
	if got, _ := list(r + "&order_number=POS-00"); len(got) != 0 {
		t.Fatalf("partial number = %v", got)
	}
	if got, _ := list(r + "&shift_id=" + uuid.NewString()); len(got) != 0 {
		t.Fatalf("other shift = %v", got)
	}
	got, next := list(r + "&limit=1")
	if !same(got, o2.ID) || next == nil {
		t.Fatalf("page 1 = %v, %v", got, next)
	}
	if got, next := list(r + "&limit=1&cursor=" + next.(string)); !same(got, o1.ID) || next != nil {
		t.Fatalf("page 2 = %v, %v", got, next)
	}
	for _, q := range []string{"?from=2026-01-01&to=2026-04-05", "?from=2026-02-01&to=2026-01-01", "?from=bad",
		"?status=paid", "?limit=201"} {
		if rec := call(h, "GET", path+q, "", ""); rec.Code != 400 {
			t.Errorf("list %s: %d %s", q, rec.Code, rec.Body)
		}
	}
}

// AC-24: a single read has everything but unit_cost.
func TestRead(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	kopi, tea := e.kopiLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn)
	o := e.place(kopi, tea)
	if _, err := e.void(o.ID, tea.ID, "not wanted"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pay(o.ID, uuid.NewString(), 28000); err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]http.Handler{
		"pos":       e.posRouter(e.outlet, true),
		"dashboard": e.dashRouter(e.cashier.StaffID),
	} {
		path := "/v1/pos/orders/" + o.ID
		if name == "dashboard" {
			path = "/v1/outlets/" + e.outlet + "/orders/" + o.ID
		}
		rec := call(h, "GET", path, e.cashier.StaffID, "")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "unit_cost") {
			t.Fatalf("%s read: %d %s", name, rec.Code, rec.Body)
		}
		b := decodeBody(t, rec)
		lines := b["lines"].([]any)
		if len(lines) != 2 || lines[1].(map[string]any)["status"] != "voided" ||
			len(lines[0].(map[string]any)["modifiers"].([]any)) != 2 || len(b["payments"].([]any)) != 1 ||
			len(b["events"].([]any)) != 2 || b["status"] != "completed" {
			t.Fatalf("%s read = %s", name, rec.Body)
		}
	}
}

// AC-25: permissions, outlets and devices over HTTP.
func TestAccess(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	o := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
	h := e.posRouter(e.outlet, true)
	viewer := e.staff("Viewer", e.outlet, domain.RoleCashier).StaffID
	stranger := e.staff("Stranger", e.newOutlet(), domain.RoleManager, orderPerms...).StaffID
	newLine := `{"id":"` + uuid.NewString() + `","product_id":"` + e.tea.String() + `","quantity":1,"fulfillment_type":"dine_in"}`
	routes := []struct{ method, path, body string }{
		{"POST", "/v1/pos/orders", placeBody(uuid.NewString(), newLine)},
		{"POST", "/v1/pos/orders/" + o.ID + "/lines", `{"lines":[` + newLine + `]}`},
		{"POST", "/v1/pos/orders/" + o.ID + "/lines/" + o.Lines[0].ID + "/void", `{"reason":"test"}`},
		{"POST", "/v1/pos/orders/" + o.ID + "/cancel", `{"reason":"test"}`},
		{"POST", "/v1/pos/orders/" + o.ID + "/payments", `{"id":"` + uuid.NewString() + `","method":"qris_static","amount":20000}`},
	}
	for _, r := range routes {
		if rec := call(h, r.method, r.path, viewer, r.body); rec.Code != 403 {
			t.Errorf("%s without permission: %d %s", r.path, rec.Code, rec.Body)
		}
		if rec := call(h, r.method, r.path, stranger, r.body); rec.Code != 403 {
			t.Errorf("%s with no role at the outlet: %d %s", r.path, rec.Code, rec.Body)
		}
	}
	if rec := call(h, "GET", "/v1/pos/orders/"+o.ID, viewer, ""); rec.Code != 200 {
		t.Fatalf("read by any role: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/pos/orders", stranger, ""); rec.Code != 403 {
		t.Fatalf("list with no role at the outlet: %d", rec.Code)
	}

	// Another outlet's device sees this order as unknown.
	other := newEnv(t)
	other.openShift()
	oh := other.posRouter(other.outlet, true)
	for _, r := range append(routes[1:], struct{ method, path, body string }{"GET", "/v1/pos/orders/" + o.ID, ""}) {
		if rec := call(oh, r.method, r.path, other.cashier.StaffID, r.body); rec.Code != 404 {
			t.Errorf("%s %s from another outlet: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if rec := call(e.posRouter(e.outlet, false), "GET", "/v1/pos/orders", e.cashier.StaffID, ""); rec.Code != 401 ||
		errorOf(t, rec)["code"] != "device_revoked" {
		t.Fatalf("revoked device: %d %s", rec.Code, rec.Body)
	}
	dash := e.dashRouter(stranger)
	for _, p := range []string{"/v1/outlets/" + e.outlet + "/orders", "/v1/outlets/" + e.outlet + "/orders/" + o.ID} {
		if rec := call(dash, "GET", p, "", ""); rec.Code != 404 {
			t.Errorf("dashboard %s with no role: %d", p, rec.Code)
		}
	}
	if rec := call(e.dashRouter(viewer), "GET", "/v1/outlets/"+other.outlet+"/orders/"+o.ID, "", ""); rec.Code != 404 {
		t.Fatalf("order under the wrong outlet: %d", rec.Code)
	}
}

// A prefix change while a shift is open reaches the next shift only: the
// open shift keeps numbering under the prefix it opened with.
func TestPrefixChangeMidShift(t *testing.T) {
	e := newEnv(t)
	sh := e.openShift()
	first := e.place()
	e.exec(`UPDATE outlets SET order_prefix = 'KSR' WHERE id = $1`, e.outlet)
	second := e.place()
	if second.OrderNumber != "POS-002" {
		t.Fatalf("mid-shift number = %s", second.OrderNumber)
	}
	for _, o := range []*Result{first, second} {
		if _, err := e.pay(o.ID, uuid.NewString(), o.GrandTotal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.closeShift(sh.ID); err != nil {
		t.Fatal(err)
	}
	e.openShift()
	if o := e.place(); o.OrderNumber != "KSR-001" {
		t.Fatalf("next shift number = %s", o.OrderNumber)
	}
}
