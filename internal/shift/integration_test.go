package shift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/duniandewon/madkunyah-be-service/migrations"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Integration tests run against a database named by TEST_DATABASE_URL,
// which they migrate. Every test creates its own outlets, staff and
// devices, so they can share one database and run in any order.

var testDB *postgres.DB

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	db, err := postgres.Connect(ctx, postgres.Config{DSN: dsn, MaxConns: 30})
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

// fakeGuard reports the same unfinished orders for every shift.
type fakeGuard struct {
	orders []OrderRef
	count  int
}

func (g *fakeGuard) UnfinishedOrders(context.Context, Querier, string, int) ([]OrderRef, int, error) {
	return g.orders, g.count, nil
}

// env is one outlet with a device, a cashier and a supervisor.
type env struct {
	t          *testing.T
	ctx        context.Context
	svc        *Service
	guard      *fakeGuard
	access     fakeAccess
	outlet     string
	device     string
	cashier    *domain.StaffAccess
	supervisor *domain.StaffAccess
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if testDB == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	e := &env{t: t, ctx: context.Background(), guard: &fakeGuard{}, access: fakeAccess{}}
	e.svc = NewService(NewRepository(testDB), e.access, e.guard)
	e.outlet = e.newOutlet("Asia/Jakarta", "04:00", true)
	e.device = e.newDevice(e.outlet)
	e.cashier = e.staff("Cashier", e.outlet, domain.RoleCashier)
	e.supervisor = e.staff("Supervisor", e.outlet, domain.RoleSupervisor, domain.PermShiftOpen, domain.PermShiftClose)
	return e
}

func (e *env) newOutlet(tz, cutoff string, active bool) string {
	e.t.Helper()
	id := uuid.NewString()
	_, err := testDB.Pool.Exec(e.ctx,
		`INSERT INTO outlets (id, name, timezone, business_day_cutoff, is_active) VALUES ($1, $2, $3, $4::time, $5)`,
		id, "Outlet "+id[:8], tz, cutoff, active)
	if err != nil {
		e.t.Fatalf("insert outlet: %v", err)
	}
	return id
}

func (e *env) newDevice(outletID string) string {
	e.t.Helper()
	id := uuid.NewString()
	if _, err := testDB.Pool.Exec(e.ctx, `INSERT INTO devices (id, outlet_id, name) VALUES ($1, $2, 'POS')`,
		id, outletID); err != nil {
		e.t.Fatalf("insert device: %v", err)
	}
	return id
}

// staff inserts a staff row and registers their access at one outlet.
func (e *env) staff(name, outletID, role string, perms ...string) *domain.StaffAccess {
	e.t.Helper()
	id := uuid.NewString()
	if _, err := testDB.Pool.Exec(e.ctx, `INSERT INTO staff (id, full_name, email) VALUES ($1, $2, $3)`,
		id, name, id+"@test.local"); err != nil {
		e.t.Fatalf("insert staff: %v", err)
	}
	a := &domain.StaffAccess{StaffID: id, Active: true, Outlets: map[string]domain.AccessScope{
		outletID: {Roles: []string{role}, Permissions: perms}}}
	e.access[id] = a
	return a
}

func (e *env) openAs(acting *domain.StaffAccess, id, approvedBy string) (*Result, error) {
	return e.svc.Open(e.ctx, &OpenInput{ID: id, OutletID: e.outlet, DeviceID: e.device,
		Acting: acting, ApprovedBy: approvedBy})
}

func (e *env) closeAs(acting *domain.StaffAccess, id, approvedBy string) (*Result, error) {
	return e.svc.Close(e.ctx, &CloseInput{ShiftID: id, OutletID: e.outlet, DeviceID: e.device,
		Acting: acting, ApprovedBy: approvedBy})
}

// open opens a new shift as the supervisor.
func (e *env) open() *Result {
	e.t.Helper()
	res, err := e.openAs(e.supervisor, uuid.NewString(), "")
	if err != nil {
		e.t.Fatalf("open shift: %v", err)
	}
	return res
}

func (e *env) close(id string) *Result {
	e.t.Helper()
	res, err := e.closeAs(e.supervisor, id, "")
	if err != nil {
		e.t.Fatalf("close shift: %v", err)
	}
	return res
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

type auditRow struct {
	Action     string
	OutletID   *string
	ActorID    *string
	ApprovedBy *string
	After      map[string]any
}

func audits(t *testing.T, shiftID string) []auditRow {
	t.Helper()
	rows, err := testDB.Pool.Query(context.Background(),
		`SELECT action, outlet_id::text, actor_id::text, approved_by::text, after FROM audit_logs
		 WHERE entity_type = 'shift' AND entity_id = $1 ORDER BY created_at`, shiftID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[auditRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func countShifts(t *testing.T, outletID string) int {
	t.Helper()
	var n int
	if err := testDB.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM shifts WHERE outlet_id = $1`, outletID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AC-2, AC-3, AC-5: an open creates the shift and its sequences; a retry
// with the same ID changes nothing; a closed shift's ID cannot reopen.
func TestOpen(t *testing.T) {
	e := newEnv(t)
	// 02:30 in Jakarta on the 12th, before the 04:00 cutoff.
	now := time.Date(2026, 11, 11, 19, 30, 0, 0, time.UTC)
	e.svc.now = func() time.Time { return now }
	id := uuid.NewString()

	res, err := e.openAs(e.supervisor, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.ID != id || res.Status != StatusOpen || res.OutletID != e.outlet ||
		res.DeviceID != e.device || res.BusinessDate != "2026-11-11" || !res.OpenedAt.Equal(now) ||
		res.OpenedBy != (StaffRef{ID: e.supervisor.StaffID, Name: "Supervisor"}) ||
		res.ClosedAt != nil || res.ClosedBy != nil || res.ClosedDeviceID != nil {
		t.Fatalf("opened = %+v", res.Shift)
	}
	want := []Sequence{{Prefix: "POS", LastValue: 0}, {Prefix: "WEB", LastValue: 0}}
	if fmt.Sprint(res.Sequences) != fmt.Sprint(want) {
		t.Fatalf("sequences = %v", res.Sequences)
	}

	again, err := e.openAs(e.supervisor, id, "")
	if err != nil || again.Applied || again.ID != id || again.Status != StatusOpen {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if n := countShifts(t, e.outlet); n != 1 {
		t.Fatalf("%d shifts after retry", n)
	}
	rows := audits(t, id)
	if len(rows) != 1 || rows[0].Action != "shift.opened" || *rows[0].OutletID != e.outlet ||
		*rows[0].ActorID != e.supervisor.StaffID || rows[0].After["business_date"] != "2026-11-11" ||
		rows[0].After["device_id"] != e.device {
		t.Fatalf("audit = %+v", rows)
	}

	e.close(id)
	_, err = e.openAs(e.supervisor, id, "")
	wantCode(t, err, domain.CodeShiftClosed)
}

// AC-4: one open shift per outlet, by check and by index.
func TestOpenConflicts(t *testing.T) {
	e := newEnv(t)
	first := e.open()

	_, err := e.openAs(e.supervisor, uuid.NewString(), "")
	de := wantErr(t, err, domain.CodeShiftAlreadyOpen)
	info, ok := de.Details.(OpenShiftInfo)
	if !ok || info.ID != first.ID || info.OpenedBy.ID != e.supervisor.StaffID ||
		info.BusinessDate != first.BusinessDate || !info.OpenedAt.Equal(first.OpenedAt) {
		t.Fatalf("details = %+v", de.Details)
	}

	// The ID of a shift at another outlet.
	other := newEnv(t)
	_, err = other.openAs(other.supervisor, first.ID, "")
	wantCode(t, err, domain.CodeConflict)

	// An inactive outlet.
	closed := newEnv(t)
	closed.outlet = closed.newOutlet("Asia/Jakarta", "04:00", false)
	closed.device = closed.newDevice(closed.outlet)
	closed.supervisor = closed.staff("Supervisor", closed.outlet, domain.RoleSupervisor, domain.PermShiftOpen)
	_, err = closed.openAs(closed.supervisor, uuid.NewString(), "")
	wantCode(t, err, domain.CodeNotFound)
}

// AC-4: racing opens with different IDs produce exactly one shift, and
// racing retries of one ID produce exactly one applied open.
func TestOpenRaces(t *testing.T) {
	const n = 12
	race := func(e *env, id func() string) (applied, already, repeats int) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := e.openAs(e.supervisor, id(), "")
				mu.Lock()
				defer mu.Unlock()
				switch de := domain.AsError(err); {
				case err == nil && res.Applied:
					applied++
				case err == nil:
					repeats++
				case de != nil && de.Code == domain.CodeShiftAlreadyOpen:
					already++
				default:
					e.t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		return applied, already, repeats
	}

	e := newEnv(t)
	if applied, already, _ := race(e, uuid.NewString); applied != 1 || already != n-1 {
		t.Fatalf("different IDs: %d applied, %d already open", applied, already)
	}
	if got := countShifts(t, e.outlet); got != 1 {
		t.Fatalf("%d shifts", got)
	}

	e = newEnv(t)
	id := uuid.NewString()
	if applied, _, repeats := race(e, func() string { return id }); applied != 1 || repeats != n-1 {
		t.Fatalf("same ID: %d applied, %d repeats", applied, repeats)
	}
	if rows := audits(t, id); len(rows) != 1 {
		t.Fatalf("%d audit rows", len(rows))
	}
}

// AC-6, AC-7, AC-8: a cashier needs an approver who holds the permission
// at the outlet; the audit row names both.
func TestStepUp(t *testing.T) {
	e := newEnv(t)
	elsewhere := e.staff("Elsewhere", e.newOutlet("Asia/Jakarta", "04:00", true), domain.RoleSupervisor,
		domain.PermShiftOpen, domain.PermShiftClose)
	deactivated := e.staff("Gone", e.outlet, domain.RoleSupervisor, domain.PermShiftOpen, domain.PermShiftClose)
	deactivated.Active = false
	fellow := e.staff("Fellow", e.outlet, domain.RoleCashier)

	id := uuid.NewString()
	for name, approver := range map[string]string{
		"none": "", "a cashier": fellow.StaffID, "deactivated": deactivated.StaffID,
		"no role at the outlet": elsewhere.StaffID, "unknown": uuid.NewString(),
	} {
		_, err := e.openAs(e.cashier, id, approver)
		if !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("open, approver %s: %v", name, err)
		}
	}
	if n := countShifts(t, e.outlet); n != 0 {
		t.Fatalf("%d shifts written by forbidden opens", n)
	}

	if _, err := e.openAs(e.cashier, id, e.supervisor.StaffID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.closeAs(e.cashier, id, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("close without approver: %v", err)
	}
	if _, err := e.closeAs(e.cashier, id, e.supervisor.StaffID); err != nil {
		t.Fatal(err)
	}
	rows := audits(t, id)
	if len(rows) != 2 {
		t.Fatalf("audit = %+v", rows)
	}
	for _, r := range rows {
		if *r.ActorID != e.cashier.StaffID || *r.ApprovedBy != e.supervisor.StaffID {
			t.Errorf("%s: actor %s, approved by %s", r.Action, *r.ActorID, *r.ApprovedBy)
		}
	}

	// A holder acting alone is their own approver, whatever they send.
	own := e.open()
	if rows := audits(t, own.ID); *rows[0].ActorID != e.supervisor.StaffID ||
		*rows[0].ApprovedBy != e.supervisor.StaffID {
		t.Fatalf("audit = %+v", rows)
	}
}

// AC-9, FR-SEQ-4, FR-SEQ-6: numbers count up per prefix and restart with
// each shift.
func TestNextNumber(t *testing.T) {
	e := newEnv(t)
	if _, err := Current(e.ctx, testDB.Pool, e.outlet); !errors.Is(err, ErrNoOpenShift) {
		t.Fatalf("no shift: %v", err)
	}
	s := e.open()

	next := func(shiftID, prefix string) string {
		t.Helper()
		var number string
		err := testDB.InTx(e.ctx, func(tx pgx.Tx) error {
			cur, err := Current(e.ctx, tx, e.outlet)
			if err != nil {
				return err
			}
			if cur.ID != shiftID || cur.BusinessDate != s.BusinessDate {
				t.Fatalf("current = %+v", cur)
			}
			_, number, err = NextNumber(e.ctx, tx, cur.ID, prefix)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return number
	}
	got := []string{next(s.ID, "POS"), next(s.ID, "POS"), next(s.ID, "POS"), next(s.ID, "WEB")}
	if fmt.Sprint(got) != "[POS-001 POS-002 POS-003 WEB-001]" {
		t.Fatalf("numbers = %v", got)
	}
	if _, _, err := NextNumber(e.ctx, testDB.Pool, s.ID, "POS9"); !errors.Is(err, ErrUnknownSequence) {
		t.Fatalf("unknown prefix: %v", err)
	}

	closed := e.close(s.ID)
	if fmt.Sprint(closed.Sequences) != fmt.Sprint([]Sequence{{"POS", 3}, {"WEB", 1}}) {
		t.Fatalf("sequences = %v", closed.Sequences)
	}
	if _, _, err := NextNumber(e.ctx, testDB.Pool, s.ID, "POS"); !errors.Is(err, ErrShiftClosed) {
		t.Fatalf("closed shift: %v", err)
	}
	if _, _, err := NextNumber(e.ctx, testDB.Pool, uuid.NewString(), "POS"); !errors.Is(err, ErrUnknownSequence) {
		t.Fatalf("unknown shift: %v", err)
	}

	s2 := e.open()
	s = s2
	if n := next(s2.ID, "POS"); n != "POS-001" {
		t.Fatalf("new shift starts at %s", n)
	}
}

// AC-10: a rollback returns the number, and concurrent allocations are
// distinct and gapless.
func TestNextNumberAtomic(t *testing.T) {
	e := newEnv(t)
	s := e.open()

	rollback := errors.New("order failed")
	err := testDB.InTx(e.ctx, func(tx pgx.Tx) error {
		if seq, _, err := NextNumber(e.ctx, tx, s.ID, "POS"); err != nil || seq != 1 {
			t.Fatalf("first = %d, %v", seq, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}

	const n = 20
	seqs := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := testDB.InTx(e.ctx, func(tx pgx.Tx) error {
				cur, err := Current(e.ctx, tx, e.outlet)
				if err != nil {
					return err
				}
				seqs[i], _, err = NextNumber(e.ctx, tx, cur.ID, "POS")
				return err
			})
			if err != nil {
				t.Errorf("allocate: %v", err)
			}
		}()
	}
	wg.Wait()
	sort.Ints(seqs)
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("sequences = %v", seqs)
		}
	}
}

// AC-11, AC-13, AC-15: unfinished orders block a close and write nothing;
// a close is idempotent; another device at the outlet may close.
func TestClose(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	blocking := []OrderRef{{ID: uuid.NewString(), OrderNumber: "POS-007", Status: "ready", Channel: "pos"}}
	e.guard.orders, e.guard.count = blocking, 61

	_, err := e.closeAs(e.supervisor, s.ID, "")
	de := wantErr(t, err, domain.CodeShiftUnfinishedOrder)
	d, ok := de.Details.(UnfinishedOrders)
	if !ok || d.Count != 61 || len(d.Orders) != 1 || d.Orders[0] != blocking[0] {
		t.Fatalf("details = %+v", de.Details)
	}
	if cur, err := e.svc.Current(e.ctx, e.outlet); err != nil || cur == nil || cur.ID != s.ID {
		t.Fatalf("blocked close changed the shift: %+v, %v", cur, err)
	}
	if rows := audits(t, s.ID); len(rows) != 1 {
		t.Fatalf("blocked close audited: %+v", rows)
	}

	// The guard caps what it lists, but a long answer is trimmed anyway.
	e.guard.orders = make([]OrderRef, 70)
	_, err = e.closeAs(e.supervisor, s.ID, "")
	if d := wantErr(t, err, domain.CodeShiftUnfinishedOrder).Details.(UnfinishedOrders); len(d.Orders) != 50 {
		t.Fatalf("%d orders listed", len(d.Orders))
	}

	e.guard.orders, e.guard.count = nil, 0
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	e.svc.now = func() time.Time { return now }
	deviceB := e.newDevice(e.outlet)
	e.device = deviceB
	res, err := e.closeAs(e.supervisor, s.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Status != StatusClosed || !res.ClosedAt.Equal(now) ||
		*res.ClosedBy != (StaffRef{ID: e.supervisor.StaffID, Name: "Supervisor"}) ||
		*res.ClosedDeviceID != deviceB || res.DeviceID == deviceB {
		t.Fatalf("closed = %+v", res.Shift)
	}

	again, err := e.closeAs(e.supervisor, s.ID, "")
	if err != nil || again.Applied || again.Status != StatusClosed || !again.ClosedAt.Equal(now) {
		t.Fatalf("second close = %+v, %v", again, err)
	}
	rows := audits(t, s.ID)
	if len(rows) != 2 || rows[1].Action != "shift.closed" || rows[1].After["closed_device_id"] != deviceB {
		t.Fatalf("audit = %+v", rows)
	}
	if seqs, _ := json.Marshal(rows[1].After["sequences"]); string(seqs) !=
		`[{"last_value":0,"prefix":"POS"},{"last_value":0,"prefix":"WEB"}]` {
		t.Fatalf("audited sequences = %s", seqs)
	}
	if cur, err := e.svc.Current(e.ctx, e.outlet); err != nil || cur != nil {
		t.Fatalf("current after close = %+v, %v", cur, err)
	}

	// A shift at another outlet is not found.
	other := newEnv(t)
	theirs := other.open()
	_, err = e.closeAs(e.supervisor, theirs.ID, "")
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.closeAs(e.supervisor, uuid.NewString(), "")
	wantCode(t, err, domain.CodeNotFound)
}

// AC-12: a close waits for a transaction holding the shift through
// Current, then sees what it committed.
func TestCloseWaitsForPlacement(t *testing.T) {
	e := newEnv(t)
	s := e.open()

	tx, err := testDB.Pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := Current(e.ctx, tx, e.outlet); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NextNumber(e.ctx, tx, s.ID, "POS"); err != nil {
		t.Fatal(err)
	}

	done := make(chan *Result, 1)
	go func() {
		res, err := e.closeAs(e.supervisor, s.ID, "")
		if err != nil {
			t.Errorf("close: %v", err)
		}
		done <- res
	}()
	select {
	case <-done:
		t.Fatal("close did not wait for the placement")
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res == nil || !res.Applied || res.Sequences[0] != (Sequence{"POS", 1}) {
			t.Fatalf("close after placement = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close still blocked after the placement committed")
	}
}

type fakeVerifier struct{ p *domain.Principal }

func (f fakeVerifier) Verify(string, ...domain.Audience) (*domain.Principal, error) { return f.p, nil }

type noDenylist struct{}

func (noDenylist) IsTokenDenied(context.Context, string) (bool, error) { return false, nil }

type fakeDevices map[string]bool

func (f fakeDevices) DeviceActive(_ context.Context, deviceID, _ string) (bool, error) {
	return f[deviceID], nil
}

// posRouter mounts the POS shift routes as the server does, for a device.
func (e *env) posRouter(outletID, deviceID string, active bool) http.Handler {
	h := NewHandler(e.svc)
	p := &domain.Principal{Subject: deviceID, Audience: domain.AudiencePOS, OutletID: outletID, DeviceID: deviceID}
	r := chi.NewRouter()
	r.Route("/v1/pos", func(r chi.Router) {
		r.Use(middleware.Authenticate(fakeVerifier{p}, noDenylist{}, domain.AudiencePOS),
			middleware.RequireActiveDevice(fakeDevices{deviceID: active}),
			middleware.MaxBodyBytes(4<<10), middleware.RequireActingStaff(e.access))
		r.Get("/shifts/current", h.POSCurrent)
		r.Post("/shifts", h.POSOpen)
		r.Post("/shifts/{shiftID}/close", h.POSClose)
	})
	return r
}

// dashRouter mounts the dashboard shift routes as the server does, for a
// signed-in staff member.
func (e *env) dashRouter(staffID string) http.Handler {
	h := NewHandler(e.svc)
	p := &domain.Principal{Subject: staffID, Audience: domain.AudienceStaff}
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.Authenticate(fakeVerifier{p}, noDenylist{}, domain.AudienceStaff),
			middleware.LoadStaffAccess(e.access), middleware.RequireOutletRole)
		r.Get("/v1/outlets/{outletID}/shifts", h.List)
		r.Get("/v1/outlets/{outletID}/shifts/{shiftID}", h.Get)
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

// AC-16, AC-18 and the POS error contract over HTTP.
func TestPOSRoutes(t *testing.T) {
	e := newEnv(t)
	h := e.posRouter(e.outlet, e.device, true)
	sup, cashier := e.supervisor.StaffID, e.cashier.StaffID
	stranger := e.staff("Stranger", e.newOutlet("Asia/Jakarta", "04:00", true), domain.RoleSupervisor,
		domain.PermShiftOpen).StaffID
	id := uuid.NewString()
	openBody := `{"id":"` + id + `"}`

	if rec := call(h, "GET", "/v1/pos/shifts/current", cashier, ""); rec.Code != 200 ||
		strings.TrimSpace(rec.Body.String()) != `{"shift":null}` {
		t.Fatalf("current, none open: %d %s", rec.Code, rec.Body)
	}
	for _, tt := range []struct {
		name, staff, body string
		want              int
	}{
		{"no acting staff", "", openBody, 400},
		{"no role at the outlet", stranger, openBody, 403},
		{"cashier without approver", cashier, openBody, 403},
		{"missing id", sup, `{}`, 400},
		{"malformed id", sup, `{"id":"nope"}`, 400},
		{"malformed approver", sup, `{"id":"` + id + `","approved_by":"x"}`, 400},
		{"unknown field", sup, `{"id":"` + id + `","opened_at":"2026-01-01T00:00:00Z"}`, 400},
		{"oversized body", sup, `{"id":"` + id + `","approved_by":"` + strings.Repeat("a", 5000) + `"}`, 413},
	} {
		if rec := call(h, "POST", "/v1/pos/shifts", tt.staff, tt.body); rec.Code != tt.want {
			t.Errorf("open, %s: %d %s", tt.name, rec.Code, rec.Body)
		}
	}

	rec := call(h, "POST", "/v1/pos/shifts", sup, openBody)
	if rec.Code != 201 {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	if b := decodeBody(t, rec); b["applied"] != true || b["id"] != id || b["status"] != "open" ||
		b["closed_at"] != nil || b["opened_by"].(map[string]any)["name"] != "Supervisor" {
		t.Fatalf("open body = %s", rec.Body)
	}
	if rec := call(h, "POST", "/v1/pos/shifts", sup, strings.ToUpper(openBody[:8])+openBody[8:]); rec.Code != 200 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "POST", "/v1/pos/shifts", sup, `{"id":"`+uuid.NewString()+`"}`)
	if rec.Code != 409 || errorOf(t, rec)["code"] != "shift_already_open" ||
		errorOf(t, rec)["details"].(map[string]any)["id"] != id {
		t.Fatalf("second open: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "GET", "/v1/pos/shifts/current", cashier, "")
	if rec.Code != 200 || decodeBody(t, rec)["shift"].(map[string]any)["id"] != id {
		t.Fatalf("current: %d %s", rec.Code, rec.Body)
	}

	// Another outlet's device cannot see or close this shift.
	other := newEnv(t)
	oh := other.posRouter(other.outlet, other.device, true)
	if rec := call(oh, "POST", "/v1/pos/shifts/"+id+"/close", other.supervisor.StaffID, ""); rec.Code != 404 {
		t.Fatalf("close from another outlet: %d %s", rec.Code, rec.Body)
	}
	revoked := e.posRouter(e.outlet, e.device, false)
	if rec := call(revoked, "GET", "/v1/pos/shifts/current", sup, ""); rec.Code != 401 ||
		errorOf(t, rec)["code"] != "device_revoked" {
		t.Fatalf("revoked device: %d %s", rec.Code, rec.Body)
	}

	e.guard.orders = []OrderRef{{ID: uuid.NewString(), OrderNumber: "WEB-002", Status: "pending_payment", Channel: "web"}}
	e.guard.count = 1
	rec = call(h, "POST", "/v1/pos/shifts/"+id+"/close", sup, "")
	if rec.Code != 409 || errorOf(t, rec)["code"] != "shift_has_unfinished_orders" {
		t.Fatalf("blocked close: %d %s", rec.Code, rec.Body)
	}
	details := errorOf(t, rec)["details"].(map[string]any)
	if details["count"] != float64(1) || details["orders"].([]any)[0].(map[string]any)["order_number"] != "WEB-002" {
		t.Fatalf("blocked close details = %v", details)
	}
	e.guard.orders, e.guard.count = nil, 0

	for _, tt := range []struct {
		name, path, staff, body string
		want                    int
	}{
		{"cashier without approver", id, cashier, "", 403},
		{"malformed shift id", "nope", sup, "", 404},
		{"unknown shift", uuid.NewString(), sup, "", 404},
		{"malformed approver", id, sup, `{"approved_by":"x"}`, 400},
	} {
		if rec := call(h, "POST", "/v1/pos/shifts/"+tt.path+"/close", tt.staff, tt.body); rec.Code != tt.want {
			t.Errorf("close, %s: %d %s", tt.name, rec.Code, rec.Body)
		}
	}
	rec = call(h, "POST", "/v1/pos/shifts/"+id+"/close", cashier, `{"approved_by":"`+sup+`"}`)
	if rec.Code != 200 || decodeBody(t, rec)["applied"] != true || decodeBody(t, rec)["status"] != "closed" {
		t.Fatalf("close: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "POST", "/v1/pos/shifts/"+id+"/close", sup, "")
	if rec.Code != 200 || decodeBody(t, rec)["applied"] != false {
		t.Fatalf("second close: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "POST", "/v1/pos/shifts", sup, openBody)
	if rec.Code != 409 || errorOf(t, rec)["code"] != "shift_closed" {
		t.Fatalf("reopen: %d %s", rec.Code, rec.Body)
	}
}

// AC-14, AC-17: the dashboard lists and reads shifts, and nothing more.
func TestDashboardRoutes(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC) // 10:00 in Jakarta
	var ids []string
	for day := range 3 {
		e.svc.now = func() time.Time { return base.AddDate(0, 0, day) }
		s := e.open()
		ids = append(ids, s.ID)
		if day < 2 {
			e.close(s.ID)
		}
	}
	h := e.dashRouter(e.cashier.StaffID)
	path := "/v1/outlets/" + e.outlet + "/shifts"

	list := func(query string) ([]string, any) {
		t.Helper()
		rec := call(h, "GET", path+query, "", "")
		if rec.Code != 200 {
			t.Fatalf("list %s: %d %s", query, rec.Code, rec.Body)
		}
		body := decodeBody(t, rec)
		var got []string
		for _, s := range body["shifts"].([]any) {
			got = append(got, s.(map[string]any)["id"].(string))
		}
		return got, body["next_cursor"]
	}
	same := func(got []string, want ...string) bool { return fmt.Sprint(got) == fmt.Sprint(want) }

	if got, next := list(""); !same(got, ids[2], ids[1], ids[0]) || next != nil {
		t.Fatalf("all = %v, next %v", got, next)
	}
	if got, _ := list("?from=2026-11-10&to=2026-11-10"); !same(got, ids[1]) {
		t.Fatalf("one day = %v", got)
	}
	if got, _ := list("?from=2026-11-10"); !same(got, ids[2], ids[1]) {
		t.Fatalf("from = %v", got)
	}
	if got, _ := list("?to=2026-11-10"); !same(got, ids[1], ids[0]) {
		t.Fatalf("to = %v", got)
	}
	if got, _ := list("?status=open"); !same(got, ids[2]) {
		t.Fatalf("open = %v", got)
	}
	if got, _ := list("?status=closed&from=2026-11-09&to=2026-11-11"); !same(got, ids[1], ids[0]) {
		t.Fatalf("closed = %v", got)
	}
	got, next := list("?limit=2")
	if !same(got, ids[2], ids[1]) || next == nil {
		t.Fatalf("page 1 = %v, next %v", got, next)
	}
	if got, next := list("?limit=2&cursor=" + next.(string)); !same(got, ids[0]) || next != nil {
		t.Fatalf("page 2 = %v, next %v", got, next)
	}
	for _, q := range []string{"?from=2026-08-01&to=2026-11-02", "?from=bad", "?status=paused", "?limit=500", "?cursor=x"} {
		if rec := call(h, "GET", path+q, "", ""); rec.Code != 400 {
			t.Errorf("list %s: %d %s", q, rec.Code, rec.Body)
		}
	}

	rec := call(h, "GET", path+"/"+ids[0], "", "")
	if b := decodeBody(t, rec); rec.Code != 200 || b["id"] != ids[0] || b["status"] != "closed" ||
		b["business_date"] != "2026-11-09" || len(b["sequences"].([]any)) != 2 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", path+"/"+uuid.NewString(), "", ""); rec.Code != 404 {
		t.Fatalf("unknown shift: %d", rec.Code)
	}

	// A shift is only visible under its own outlet, to staff with a role
	// there.
	other := newEnv(t)
	theirs := other.open()
	if rec := call(h, "GET", path+"/"+theirs.ID, "", ""); rec.Code != 404 {
		t.Fatalf("another outlet's shift: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/outlets/"+other.outlet+"/shifts", "", ""); rec.Code != 404 {
		t.Fatalf("no role at the outlet: %d", rec.Code)
	}

	// No dashboard route opens or closes a shift (FR-SH-8).
	sup := e.dashRouter(e.supervisor.StaffID)
	for _, p := range []string{path, path + "/" + ids[2] + "/close"} {
		if rec := call(sup, "POST", p, "", `{}`); rec.Code != 404 && rec.Code != 405 {
			t.Errorf("POST %s: %d", p, rec.Code)
		}
	}
	if cur, err := e.svc.Current(e.ctx, e.outlet); err != nil || cur == nil || cur.ID != ids[2] {
		t.Fatalf("shift changed: %+v, %v", cur, err)
	}
}
