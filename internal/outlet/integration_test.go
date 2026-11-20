package outlet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/auth"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/duniandewon/madkunyah-be-service/internal/shift"
	"github.com/duniandewon/madkunyah-be-service/migrations"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Integration tests run against a database named by TEST_DATABASE_URL,
// which they migrate. Every test creates its own outlets and staff.

var testDB *postgres.DB

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	db, err := postgres.Connect(ctx, postgres.Config{DSN: dsn, MaxConns: 20})
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

// fakeCache records cache bumps.
type fakeCache struct {
	mu     sync.Mutex
	bumped []string
}

func (c *fakeCache) BumpOutletCatalogGeneration(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bumped = append(c.bumped, id)
	return nil
}

func (c *fakeCache) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.bumped {
		if b == id {
			n++
		}
	}
	return n
}

// fakeSessions records devices whose sessions were ended.
type fakeSessions struct {
	mu    sync.Mutex
	ended []string
}

func (s *fakeSessions) EndDeviceSessions(_ context.Context, d auth.DeviceRef, _ auth.RequestMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = append(s.ended, d.ID)
	return nil
}

type noOrders struct{}

func (noOrders) UnfinishedOrders(context.Context, shift.Querier, string, int) ([]shift.OrderRef, int, error) {
	return nil, 0, nil
}

type env struct {
	t        *testing.T
	ctx      context.Context
	access   fakeAccess
	cache    *fakeCache
	sessions *fakeSessions
	svc      *Service
	shifts   *shift.Service
	owner    *domain.StaffAccess
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if testDB == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	e := &env{t: t, ctx: context.Background(), access: fakeAccess{}, cache: &fakeCache{}, sessions: &fakeSessions{}}
	e.svc = NewService(NewRepository(testDB), e.cache, e.sessions)
	e.shifts = shift.NewService(shift.NewRepository(testDB), e.access, noOrders{})
	e.owner = e.staff(&domain.StaffAccess{Global: domain.AccessScope{
		Roles:       []string{domain.RoleOwner},
		Permissions: []string{domain.PermOutletManage, domain.PermShiftOpen, domain.PermShiftClose},
	}})
	return e
}

// staff inserts a staff row for a and registers its access.
func (e *env) staff(a *domain.StaffAccess) *domain.StaffAccess {
	e.t.Helper()
	a.StaffID, a.Active = uuid.NewString(), true
	if _, err := testDB.Pool.Exec(e.ctx, `INSERT INTO staff (id, full_name, email) VALUES ($1, 'Staff', $2)`,
		a.StaffID, a.StaffID+"@test.local"); err != nil {
		e.t.Fatalf("insert staff: %v", err)
	}
	e.access[a.StaffID] = a
	return a
}

// atOutlet is staff holding role at one outlet only.
func (e *env) atOutlet(outletID, role string, perms ...string) *domain.StaffAccess {
	return e.staff(&domain.StaffAccess{Outlets: map[string]domain.AccessScope{
		outletID: {Roles: []string{role}, Permissions: perms}}})
}

func (e *env) create(name string) *Outlet {
	e.t.Helper()
	o, err := e.svc.Create(e.ctx, e.owner, &Fields{Name: Value(name)})
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return o
}

func (e *env) device(outletID string) string {
	e.t.Helper()
	id := uuid.NewString()
	if _, err := testDB.Pool.Exec(e.ctx, `INSERT INTO devices (id, outlet_id, name) VALUES ($1, $2, 'POS')`,
		id, outletID); err != nil {
		e.t.Fatalf("insert device: %v", err)
	}
	return id
}

func (e *env) open(outletID, deviceID string) (*shift.Result, error) {
	return e.shifts.Open(e.ctx, &shift.OpenInput{ID: uuid.NewString(), OutletID: outletID, DeviceID: deviceID,
		Acting: e.owner})
}

func (e *env) close(outletID, deviceID, shiftID string) {
	e.t.Helper()
	if _, err := e.shifts.Close(e.ctx, &shift.CloseInput{ShiftID: shiftID, OutletID: outletID, DeviceID: deviceID,
		Acting: e.owner}); err != nil {
		e.t.Fatalf("close: %v", err)
	}
}

func (e *env) audits(outletID, action string) int {
	e.t.Helper()
	var n int
	err := testDB.Pool.QueryRow(e.ctx,
		`SELECT count(*) FROM audit_logs WHERE outlet_id = $1 AND action = $2`, outletID, action).Scan(&n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) auditAfter(outletID, action string) map[string]any {
	e.t.Helper()
	var b []byte
	err := testDB.Pool.QueryRow(e.ctx, `SELECT after FROM audit_logs WHERE outlet_id = $1 AND action = $2
		ORDER BY created_at DESC LIMIT 1`, outletID, action).Scan(&b)
	if err != nil {
		e.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func code(err error) domain.ErrorCode {
	if de := domain.AsError(err); de != nil {
		return de.Code
	}
	return ""
}

type fakeVerifier struct{ p *domain.Principal }

func (f fakeVerifier) Verify(string, ...domain.Audience) (*domain.Principal, error) { return f.p, nil }

type noDenylist struct{}

func (noDenylist) IsTokenDenied(context.Context, string) (bool, error) { return false, nil }

// router mounts the outlet routes as the server does, for one staff member.
func (e *env) router(staffID string) http.Handler {
	h := NewHandler(e.svc)
	p := &domain.Principal{Subject: staffID, Audience: domain.AudienceStaff}
	manage := middleware.RequireGlobalPermission(domain.PermOutletManage)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.Authenticate(fakeVerifier{p}, noDenylist{}, domain.AudienceStaff),
			middleware.LoadStaffAccess(e.access), middleware.MaxBodyBytes(4<<10))
		one := r.With(middleware.RequireOutletRole)
		r.With(manage).Post("/v1/outlets", h.Create)
		one.Get("/v1/outlets/{outletID}", h.Get)
		one.With(manage).Patch("/v1/outlets/{outletID}", h.Update)
		one.With(manage).Post("/v1/outlets/{outletID}/deactivate", h.Deactivate)
		one.With(manage).Post("/v1/outlets/{outletID}/activate", h.Activate)
		r.Get("/v1/staff/me/outlets", h.Mine)
	})
	return r
}

func call(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	return v
}

// AC-1: create with only a name takes the defaults and is listed.
func TestCreate(t *testing.T) {
	e := newEnv(t)
	rec := call(e.router(e.owner.StaffID), http.MethodPost, "/v1/outlets", `{"name": "  Kemang  "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	o := decode(t, rec)
	id, _ := o["id"].(string)
	if o["name"] != "Kemang" || o["timezone"] != "Asia/Jakarta" || o["business_day_cutoff"] != "04:00" ||
		o["order_prefix"] != "POS" || o["is_active"] != true || o["address"] != nil || o["open_shift_id"] != nil {
		t.Fatalf("outlet = %v", o)
	}
	if n := e.audits(id, "outlet.created"); n != 1 {
		t.Fatalf("outlet.created rows = %d", n)
	}
	if a := e.auditAfter(id, "outlet.created"); a["name"] != "Kemang" || a["order_prefix"] != "POS" {
		t.Fatalf("audit after = %v", a)
	}
	mine := call(e.router(e.owner.StaffID), http.MethodGet, "/v1/staff/me/outlets", "")
	if !strings.Contains(mine.Body.String(), id) {
		t.Fatalf("own list misses the outlet: %s", mine.Body)
	}
	var active bool
	if err := testDB.Pool.QueryRow(e.ctx, `SELECT is_active FROM outlets WHERE id = $1`, id).Scan(&active); err != nil ||
		!active {
		t.Fatalf("stored active = %v, %v", active, err)
	}
}

// AC-2 and AC-11: who may write and read.
func TestAccess(t *testing.T) {
	e := newEnv(t)
	mine, other := e.create("Mine"), e.create("Other")
	manager := e.atOutlet(mine.ID, domain.RoleManager, domain.PermStaffManage)
	cashier := e.atOutlet(mine.ID, domain.RoleCashier)
	m := e.router(manager.StaffID)
	for _, tt := range []struct {
		name, method, path string
		want               int
	}{
		{"manager create", http.MethodPost, "/v1/outlets", http.StatusForbidden},
		{"manager patch own", http.MethodPatch, "/v1/outlets/" + mine.ID, http.StatusForbidden},
		{"manager patch other", http.MethodPatch, "/v1/outlets/" + other.ID, http.StatusNotFound},
		{"manager deactivate own", http.MethodPost, "/v1/outlets/" + mine.ID + "/deactivate", http.StatusForbidden},
		{"manager read other", http.MethodGet, "/v1/outlets/" + other.ID, http.StatusNotFound},
	} {
		if rec := call(m, tt.method, tt.path, `{"name": "X"}`); rec.Code != tt.want {
			t.Errorf("%s: %d %s", tt.name, rec.Code, rec.Body)
		}
	}
	rec := call(e.router(cashier.StaffID), http.MethodGet, "/v1/outlets/"+mine.ID, "")
	if rec.Code != http.StatusOK || decode(t, rec)["id"] != mine.ID {
		t.Fatalf("cashier read: %d %s", rec.Code, rec.Body)
	}
	own := e.router(e.owner.StaffID)
	for _, path := range []string{"/v1/outlets/" + uuid.NewString(), "/v1/outlets/not-a-uuid"} {
		if rec := call(own, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("owner read %s: %d", path, rec.Code)
		}
		if rec := call(own, http.MethodPatch, path, `{"name": "X"}`); rec.Code != http.StatusNotFound {
			t.Errorf("owner patch %s: %d", path, rec.Code)
		}
	}
	// FR-OTL-5: the manager lists only their outlet; inactive outlets are
	// hidden from them.
	list := decode(t, call(m, http.MethodGet, "/v1/staff/me/outlets", ""))["outlets"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != mine.ID {
		t.Fatalf("manager list = %v", list)
	}
	if _, err := e.svc.Deactivate(e.ctx, e.owner, mine.ID, auth.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if list := decode(t, call(m, http.MethodGet, "/v1/staff/me/outlets", ""))["outlets"].([]any); len(list) != 0 {
		t.Fatalf("manager sees an inactive outlet: %v", list)
	}
	if !strings.Contains(call(own, http.MethodGet, "/v1/staff/me/outlets", "").Body.String(), mine.ID) {
		t.Fatal("owner does not see the inactive outlet")
	}
}

// AC-3 over HTTP: refused bodies write nothing.
func TestValidation(t *testing.T) {
	e := newEnv(t)
	o := e.create("Valid")
	r := e.router(e.owner.StaffID)
	for _, tt := range []struct{ body, field string }{
		{`{"name": null}`, "name"},
		{`{"name": 5}`, "name"},
		{`{"timezone": "Local"}`, "timezone"},
		{`{"business_day_cutoff": "25:00"}`, "business_day_cutoff"},
		{`{"order_prefix": "WEB"}`, "order_prefix"},
		{`{"phone": "` + strings.Repeat("1", 31) + `"}`, "phone"},
	} {
		rec := call(r, http.MethodPatch, "/v1/outlets/"+o.ID, tt.body)
		fields, _ := decode(t, rec)["error"].(map[string]any)["fields"].(map[string]any)
		if rec.Code != http.StatusBadRequest || fields[tt.field] == nil {
			t.Errorf("%s: %d %s", tt.body, rec.Code, rec.Body)
		}
	}
	if rec := call(r, http.MethodPatch, "/v1/outlets/"+o.ID, `{"nmae": "x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := call(r, http.MethodPost, "/v1/outlets", `{"address": "x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("create without name: %d", rec.Code)
	}
	big := `{"name": "` + strings.Repeat("a", 5000) + `"}`
	if rec := call(r, http.MethodPatch, "/v1/outlets/"+o.ID, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: %d", rec.Code)
	}
	if n := e.audits(o.ID, "outlet.updated"); n != 0 {
		t.Fatalf("outlet.updated rows = %d", n)
	}
}

// AC-4, E-1, E-2: changed fields only, no-ops write nothing, cached
// fields drop the cache.
func TestUpdate(t *testing.T) {
	e := newEnv(t)
	o := e.create("Before")
	r := e.router(e.owner.StaffID)
	rec := call(r, http.MethodPatch, "/v1/outlets/"+o.ID, `{"name": "After", "address": "Jl. 1", "phone": "021"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if got := decode(t, rec); got["name"] != "After" || got["address"] != "Jl. 1" || got["phone"] != "021" {
		t.Fatalf("outlet = %v", got)
	}
	a := e.auditAfter(o.ID, "outlet.updated")
	if len(a) != 3 || a["name"] != "After" {
		t.Fatalf("audit after = %v", a)
	}
	if e.cache.count(o.ID) != 1 {
		t.Fatalf("bumps = %d", e.cache.count(o.ID))
	}
	// Address and phone are not in the cached documents.
	if rec := call(r, http.MethodPatch, "/v1/outlets/"+o.ID, `{"address": null}`); rec.Code != http.StatusOK ||
		decode(t, rec)["address"] != nil {
		t.Fatalf("clear address: %d %s", rec.Code, rec.Body)
	}
	if e.cache.count(o.ID) != 1 {
		t.Fatalf("address change bumped the cache")
	}
	for _, body := range []string{`{}`, `{"order_prefix": "pos", "name": " After "}`, `{"phone": "021"}`} {
		if rec := call(r, http.MethodPatch, "/v1/outlets/"+o.ID, body); rec.Code != http.StatusOK {
			t.Fatalf("no-op %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if n := e.audits(o.ID, "outlet.updated"); n != 2 {
		t.Fatalf("outlet.updated rows = %d, want 2", n)
	}
	if e.cache.count(o.ID) != 1 {
		t.Fatalf("no-op bumped the cache")
	}
}

// AC-5, AC-6, E-3, E-4: day settings lock while a shift is open; the
// prefix does not, and the open shift keeps its sequence.
func TestShiftOpen(t *testing.T) {
	e := newEnv(t)
	o := e.create("Shifted")
	dev := e.device(o.ID)
	s, err := e.open(o.ID, dev)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*Fields{
		{Timezone: Value("Asia/Makassar")},
		{BusinessDayCutoff: Value("05:00")},
		{Name: Value("Renamed"), Timezone: Value("Asia/Makassar")},
	} {
		_, err := e.svc.Update(e.ctx, e.owner, o.ID, f)
		de := domain.AsError(err)
		if de == nil || de.Code != domain.CodeOutletShiftOpen || de.Details.(map[string]string)["shift_id"] != s.ID {
			t.Fatalf("update %+v: %v", f, err)
		}
	}
	if got, _ := e.svc.Get(e.ctx, o.ID); got.Name != "Shifted" || got.OpenShiftID == nil || *got.OpenShiftID != s.ID {
		t.Fatalf("outlet = %+v", got)
	}
	if _, err := e.svc.Update(e.ctx, e.owner, o.ID, &Fields{Timezone: Value("Asia/Jakarta")}); err != nil {
		t.Fatalf("same timezone: %v", err)
	}
	if _, err := e.svc.Update(e.ctx, e.owner, o.ID, &Fields{OrderPrefix: Value("KMG")}); err != nil {
		t.Fatalf("prefix: %v", err)
	}
	if got := e.prefixes(s.ID); got != "POS,WEB" {
		t.Fatalf("open shift sequences = %s", got)
	}
	e.close(o.ID, dev, s.ID)
	if _, err := e.svc.Update(e.ctx, e.owner, o.ID, &Fields{Timezone: Value("Asia/Makassar")}); err != nil {
		t.Fatalf("after close: %v", err)
	}
	next, err := e.open(o.ID, dev)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.prefixes(next.ID); got != "KMG,WEB" {
		t.Fatalf("next shift sequences = %s", got)
	}
}

func (e *env) prefixes(shiftID string) string {
	e.t.Helper()
	var s string
	err := testDB.Pool.QueryRow(e.ctx,
		`SELECT string_agg(prefix, ',' ORDER BY prefix) FROM order_sequences WHERE shift_id = $1`, shiftID).Scan(&s)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// AC-7, AC-8, AC-9, E-5: deactivation needs a closed shift, revokes
// devices and keeps roles; activation does not bring devices back.
func TestDeactivate(t *testing.T) {
	e := newEnv(t)
	o := e.create("Closing")
	dev := e.device(o.ID)
	manager := e.atOutlet(o.ID, domain.RoleManager)
	if _, err := testDB.Pool.Exec(e.ctx, `INSERT INTO staff_outlet_roles (staff_id, outlet_id, role_id)
		SELECT $1, $2, id FROM roles WHERE code = 'manager'`, manager.StaffID, o.ID); err != nil {
		t.Fatal(err)
	}
	s, err := e.open(o.ID, dev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Deactivate(e.ctx, e.owner, o.ID, auth.RequestMeta{}); code(err) != domain.CodeOutletShiftOpen {
		t.Fatalf("deactivate with a shift open: %v", err)
	}
	e.close(o.ID, dev, s.ID)
	r := e.router(e.owner.StaffID)
	rec := call(r, http.MethodPost, "/v1/outlets/"+o.ID+"/deactivate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("deactivate: %d %s", rec.Code, rec.Body)
	}
	if got := decode(t, rec); got["applied"] != true || got["is_active"] != false {
		t.Fatalf("result = %v", got)
	}
	var devActive bool
	var roles int
	if err := testDB.Pool.QueryRow(e.ctx, `SELECT is_active FROM devices WHERE id = $1`, dev).Scan(&devActive); err != nil {
		t.Fatal(err)
	}
	if err := testDB.Pool.QueryRow(e.ctx, `SELECT count(*) FROM staff_outlet_roles WHERE outlet_id = $1`, o.ID).
		Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if devActive || roles != 1 || len(e.sessions.ended) != 1 || e.sessions.ended[0] != dev {
		t.Fatalf("device active %v, roles %d, sessions ended %v", devActive, roles, e.sessions.ended)
	}
	if e.audits(o.ID, "device.revoked") != 1 || e.auditAfter(o.ID, "outlet.deactivated")["devices_revoked"] != 1.0 {
		t.Fatal("deactivation audit rows")
	}
	if _, err := e.open(o.ID, e.device(o.ID)); code(err) != domain.CodeNotFound {
		t.Fatalf("shift open at an inactive outlet: %v", err)
	}
	if rec := call(r, http.MethodPost, "/v1/outlets/"+o.ID+"/deactivate", ""); decode(t, rec)["applied"] != false {
		t.Fatalf("repeat deactivate: %s", rec.Body)
	}
	if rec := call(r, http.MethodPost, "/v1/outlets/"+o.ID+"/activate", ""); decode(t, rec)["applied"] != true {
		t.Fatalf("activate: %s", rec.Body)
	}
	if rec := call(r, http.MethodPost, "/v1/outlets/"+o.ID+"/activate", ""); decode(t, rec)["applied"] != false {
		t.Fatalf("repeat activate: %s", rec.Body)
	}
	if err := testDB.Pool.QueryRow(e.ctx, `SELECT is_active FROM devices WHERE id = $1`, dev).Scan(&devActive); err != nil ||
		devActive {
		t.Fatalf("revoked device came back: %v %v", devActive, err)
	}
	if e.audits(o.ID, "outlet.deactivated") != 1 || e.audits(o.ID, "outlet.activated") != 1 {
		t.Fatal("repeats wrote audit rows")
	}
	if e.cache.count(o.ID) != 2 {
		t.Fatalf("bumps = %d, want 2", e.cache.count(o.ID))
	}
}

// AC-10, E-6: a shift open racing a deactivation; exactly one wins.
func TestOpenRacesDeactivate(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 20; i++ {
		o := e.create("Race")
		dev := e.device(o.ID)
		var wg sync.WaitGroup
		var openErr, deactErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, openErr = e.open(o.ID, dev) }()
		go func() { defer wg.Done(); _, deactErr = e.svc.Deactivate(e.ctx, e.owner, o.ID, auth.RequestMeta{}) }()
		wg.Wait()
		switch {
		case openErr == nil && code(deactErr) == domain.CodeOutletShiftOpen:
		case deactErr == nil && code(openErr) == domain.CodeNotFound:
		default:
			t.Fatalf("run %d: open %v, deactivate %v", i, openErr, deactErr)
		}
	}
}

// A zone Go accepts must also be one Postgres knows; the repository checks
// Postgres directly on create and on a timezone change.
func TestZoneUnknownToPostgres(t *testing.T) {
	e := newEnv(t)
	repo := NewRepository(testDB)
	o := &Outlet{ID: uuid.NewString(), Name: "Zone", Timezone: "Mars/Olympus", BusinessDayCutoff: "04:00",
		OrderPrefix: "POS", IsActive: true}
	de := domain.AsError(repo.Create(e.ctx, e.owner.StaffID, o))
	if de == nil || de.Code != domain.CodeInvalidInput || de.Fields["timezone"] == "" {
		t.Fatalf("create: %v", de)
	}
	if _, err := e.svc.Get(e.ctx, o.ID); code(err) != domain.CodeNotFound {
		t.Fatalf("outlet was stored: %v", err)
	}
	good := e.create("Zone ok")
	_, _, err := repo.Update(e.ctx, e.owner.StaffID, good.ID, func(cur *Outlet, _ string) (*Outlet, error) {
		next := *cur
		next.Timezone = "Mars/Olympus"
		return &next, nil
	})
	if de := domain.AsError(err); de == nil || de.Fields["timezone"] == "" {
		t.Fatalf("update: %v", err)
	}
	if got, _ := e.svc.Get(e.ctx, good.ID); got.Timezone != "Asia/Jakarta" || e.audits(good.ID, "outlet.updated") != 0 {
		t.Fatalf("outlet changed: %+v", got)
	}
}
