package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

// memStore is the Redis cache in memory, with switches for an outage.
type memStore struct {
	mu        sync.Mutex
	gen       int64
	outletGen map[string]int64
	docs      map[string][]byte
	down      bool
	bumpFail  bool
}

var errRedisDown = errors.New("redis down")

func newMemStore() *memStore {
	return &memStore{outletGen: map[string]int64{}, docs: map[string][]byte{}}
}

func (m *memStore) CatalogGenerations(_ context.Context, outletID string) (global, outlet int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, 0, errRedisDown
	}
	return m.gen, m.outletGen[outletID], nil
}

func (m *memStore) BumpCatalogGeneration(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down || m.bumpFail {
		return errRedisDown
	}
	m.gen++
	return nil
}

func (m *memStore) BumpOutletCatalogGeneration(_ context.Context, outletID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down || m.bumpFail {
		return errRedisDown
	}
	m.outletGen[outletID]++
	return nil
}

// bumps returns the catalog-wide and the outlet's generation.
func (m *memStore) bumps(outletID string) (global, outlet int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen, m.outletGen[outletID]
}

func (m *memStore) GetCatalogDoc(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, errRedisDown
	}
	b, ok := m.docs[key]
	if !ok {
		return nil, credis.ErrNotFound
	}
	return b, nil
}

func (m *memStore) SetCatalogDoc(_ context.Context, key string, b []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errRedisDown
	}
	m.docs[key] = b
	return nil
}

type oenv struct {
	*env
	store *memStore
}

// newOutletEnv is newEnv with a cache and an owner who exists in staff,
// since settings and sold-out rows reference who changed them.
func newOutletEnv(t *testing.T) *oenv {
	t.Helper()
	e := newEnv(t)
	store := newMemStore()
	e.svc = NewService(NewRepository(testDB), Config{PriceMax: 100_000_000, CacheTTL: time.Minute}, store)
	e.owner.StaffID = e.staffRow("Owner")
	// The seeded owner role carries every outlet permission globally.
	e.owner.Global.Permissions = append(e.owner.Global.Permissions,
		domain.PermAvailabilityManage, domain.PermSoldOutToggle)
	return &oenv{env: e, store: store}
}

func (e *env) staffRow(name string) string {
	e.t.Helper()
	id := uuid.NewString()
	if _, err := testDB.Pool.Exec(e.ctx, `INSERT INTO staff (id, full_name, email) VALUES ($1, $2, $3)`,
		id, e.name(name), id+"@test.local"); err != nil {
		e.t.Fatalf("insert staff: %v", err)
	}
	return id
}

func (e *env) outletID() string { return e.outlet("Asia/Jakarta", true).String() }

func (e *env) device(outletID string) string {
	e.t.Helper()
	id := uuid.NewString()
	if _, err := testDB.Pool.Exec(e.ctx, `INSERT INTO devices (id, outlet_id, name) VALUES ($1, $2, 'POS')`,
		id, outletID); err != nil {
		e.t.Fatalf("insert device: %v", err)
	}
	return id
}

// staffAt is a staff member with one role at one outlet.
func staffAt(staffID, outletID, role string, perms ...string) *domain.StaffAccess {
	return &domain.StaffAccess{StaffID: staffID, Active: true, Outlets: map[string]domain.AccessScope{
		outletID: {Roles: []string{role}, Permissions: perms}}}
}

func set[T any](v T) Nullable[T] { return Nullable[T]{Set: true, Valid: true, V: v} }

func null[T any]() Nullable[T] { return Nullable[T]{Set: true} }

func (e *oenv) menuCategory(outletID, catID string) *MenuCategory {
	e.t.Helper()
	m, err := e.svc.Menu(e.ctx, outletID)
	if err != nil {
		e.t.Fatalf("menu: %v", err)
	}
	for i := range m.Categories {
		if m.Categories[i].ID == catID {
			return &m.Categories[i]
		}
	}
	return nil
}

func (e *oenv) menuProduct(outletID, catID, productID string) *MenuProduct {
	e.t.Helper()
	if c := e.menuCategory(outletID, catID); c != nil {
		for i := range c.Products {
			if c.Products[i].ID == productID {
				return &c.Products[i]
			}
		}
	}
	return nil
}

func (e *oenv) bundle(outletID string) (*Storefront, string) {
	e.t.Helper()
	body, version, err := e.svc.Bundle(e.ctx, outletID)
	if err != nil {
		e.t.Fatalf("bundle: %v", err)
	}
	var sf Storefront
	if err := json.Unmarshal(body, &sf); err != nil {
		e.t.Fatal(err)
	}
	if sf.Version != version {
		e.t.Fatalf("body version %q, header version %q", sf.Version, version)
	}
	return &sf, version
}

func bundleHas(sf *Storefront, productID string) bool {
	for _, c := range sf.Categories {
		for _, p := range c.Products {
			if p.ID == productID {
				return true
			}
		}
	}
	return false
}

func (e *oenv) toggle(outletID string, item SoldOutItem, sold bool, scope string) *SoldOutResult {
	e.t.Helper()
	w, err := NewSoldOutWrite(outletID, &SoldOutInput{Item: item, SoldOut: &sold, Scope: scope}, e.owner.StaffID,
		ViaDashboard)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.svc.SetSoldOut(e.ctx, w)
	if err != nil {
		e.t.Fatalf("set sold out: %v", err)
	}
	return res
}

type outletAudit struct {
	Action   string
	OutletID *string
	ActorID  *string
	Delta    *int64
	Before   map[string]any
	After    map[string]any
}

func outletAudits(t *testing.T, entityID, actionPrefix string) []outletAudit {
	t.Helper()
	rows, err := testDB.Pool.Query(context.Background(),
		`SELECT action, outlet_id::text, actor_id::text, value_delta, before, after FROM audit_logs
		 WHERE entity_id = $1 AND action LIKE $2 || '%' ORDER BY created_at`, entityID, actionPrefix)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[outletAudit])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *oenv) outletProduct(outletID, productID string) *OutletProduct {
	e.t.Helper()
	products, err := e.svc.OutletCatalog(e.ctx, e.owner, outletID, true)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := range products {
		if products[i].ID == productID {
			return &products[i]
		}
	}
	e.t.Fatalf("product %s not in outlet catalog", productID)
	return nil
}

// AC-2, E-3: a manager sets visibility and windows, never prices; an owner
// sets all three. A request touching a forbidden field writes nothing.
func TestOutletSettingsPermissions(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Perms").ID, "Kopi", 20000)
	outlet := e.outletID()
	mgr := staffAt(e.staffRow("Mgr"), outlet, domain.RoleManager, domain.PermAvailabilityManage)
	cashier := staffAt(e.staffRow("Cashier"), outlet, domain.RoleCashier, domain.PermSoldOutToggle)

	res, err := e.svc.SetChannelSettings(e.ctx, mgr, outlet, p.ID, ChannelWeb, &ChannelSettingsInput{
		IsVisible: set(false), AvailableFrom: set("07:00"), AvailableTo: set("11:00")})
	if err != nil || res.Visible || res.Window == nil || res.Window.From != "07:00" || res.Price != 20000 {
		t.Fatalf("manager visibility: %v %+v", err, res)
	}
	_, err = e.svc.SetChannelSettings(e.ctx, mgr, outlet, p.ID, ChannelWeb, &ChannelSettingsInput{
		Price: set(int64(23000)), IsVisible: set(true)})
	wantCode(t, err, domain.CodeForbidden)
	if o := e.outletProduct(outlet, p.ID).Channels.Web.Override; o.Price != nil || *o.IsVisible {
		t.Fatalf("forbidden request wrote: %+v", o)
	}
	_, err = e.svc.SetChannelSettings(e.ctx, cashier, outlet, p.ID, ChannelWeb, &ChannelSettingsInput{
		IsVisible: set(true)})
	wantCode(t, err, domain.CodeForbidden)

	res, err = e.svc.SetChannelSettings(e.ctx, e.owner, outlet, p.ID, ChannelWeb, &ChannelSettingsInput{
		Price: set(int64(23000)), IsVisible: set(true), AvailableFrom: set("06:00"), AvailableTo: set("10:00")})
	if err != nil || !res.Visible || res.Price != 23000 || res.Window.To != "10:00" {
		t.Fatalf("owner: %v %+v", err, res)
	}
	// Clearing the owner's price is changing it: a manager cannot reset.
	wantCode(t, e.svc.ResetChannelSettings(e.ctx, mgr, outlet, p.ID, ChannelWeb), domain.CodeForbidden)
}

// AC-4, AC-21: an outlet price audits its effective delta; DELETE returns
// every field to the product default.
func TestOutletSettingsResetAndAudit(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Reset").ID, "Kopi", 20000)
	outlet := e.outletID()

	if _, err := e.svc.SetChannelSettings(e.ctx, e.owner, outlet, p.ID, ChannelWeb, &ChannelSettingsInput{
		Price: set(int64(23000))}); err != nil {
		t.Fatal(err)
	}
	rows := outletAudits(t, p.ID, "outlet_product")
	if len(rows) != 1 || rows[0].Action != "outlet_product.updated" || rows[0].OutletID == nil ||
		*rows[0].OutletID != outlet || rows[0].Delta == nil || *rows[0].Delta != 3000 {
		t.Fatalf("audit = %+v", rows)
	}
	if rows[0].Before["price"] != nil || rows[0].After["price"] != float64(23000) || rows[0].After["channel"] != "web" ||
		len(rows[0].After) != 2 {
		t.Errorf("audit fields = %v -> %v", rows[0].Before, rows[0].After)
	}
	// Same value again: no write, no audit.
	if _, err := e.svc.SetChannelSettings(e.ctx, e.owner, outlet, p.ID, ChannelWeb, &ChannelSettingsInput{
		Price: set(int64(23000))}); err != nil {
		t.Fatal(err)
	}
	if got := len(outletAudits(t, p.ID, "outlet_product")); got != 1 {
		t.Fatalf("no-op wrote audit: %d rows", got)
	}

	if err := e.svc.ResetChannelSettings(e.ctx, e.owner, outlet, p.ID, ChannelWeb); err != nil {
		t.Fatal(err)
	}
	web := e.outletProduct(outlet, p.ID).Channels.Web
	if web.Override != nil || web.Price != 20000 || !web.Visible || web.Window != nil {
		t.Fatalf("after reset: %+v", web)
	}
	rows = outletAudits(t, p.ID, "outlet_product")
	if len(rows) != 2 || rows[1].Action != "outlet_product.reset" || *rows[1].Delta != -3000 {
		t.Fatalf("reset audit = %+v", rows)
	}
	if err := e.svc.ResetChannelSettings(e.ctx, e.owner, outlet, p.ID, ChannelWeb); err != nil {
		t.Fatal(err)
	}
	if got := len(outletAudits(t, p.ID, "outlet_product")); got != 2 {
		t.Fatalf("reset of nothing wrote audit: %d rows", got)
	}
}

// FR-OUT-8 and validation.
func TestOutletSettingsValidation(t *testing.T) {
	e := newOutletEnv(t)
	cat := e.category("Validate")
	p := e.product(cat.ID, "Kopi", 20000)
	outlet := e.outletID()
	put := func(outletID, productID string, in ChannelSettingsInput) error {
		_, err := e.svc.SetChannelSettings(e.ctx, e.owner, outletID, productID, ChannelPOS, &in)
		return err
	}
	wantCode(t, put(outlet, p.ID, ChannelSettingsInput{}), domain.CodeInvalidInput)
	wantCode(t, put(outlet, p.ID, ChannelSettingsInput{Price: set(int64(-1))}), domain.CodeInvalidInput)
	wantCode(t, put(outlet, p.ID, ChannelSettingsInput{AvailableFrom: set("07:00")}), domain.CodeInvalidInput)
	wantCode(t, put(outlet, p.ID, ChannelSettingsInput{AvailableFrom: set("7am"), AvailableTo: set("11:00")}),
		domain.CodeInvalidInput)
	wantCode(t, put(outlet, uuid.NewString(), ChannelSettingsInput{IsVisible: set(true)}), domain.CodeNotFound)
	wantCode(t, put(uuid.NewString(), p.ID, ChannelSettingsInput{IsVisible: set(true)}), domain.CodeNotFound)
	wantCode(t, put(e.outlet("Asia/Jakarta", false).String(), p.ID, ChannelSettingsInput{IsVisible: set(true)}),
		domain.CodeNotFound)

	// Windows merge with the stored row; one end alone moves that end.
	if err := put(outlet, p.ID, ChannelSettingsInput{AvailableFrom: set("07:00"), AvailableTo: set("11:00")}); err != nil {
		t.Fatal(err)
	}
	if err := put(outlet, p.ID, ChannelSettingsInput{AvailableTo: set("12:00")}); err != nil {
		t.Fatal(err)
	}
	if w := e.outletProduct(outlet, p.ID).Channels.POS.Window; w.From != "07:00" || w.To != "12:00" {
		t.Fatalf("merged window = %+v", w)
	}
	wantCode(t, put(outlet, p.ID, ChannelSettingsInput{AvailableTo: null[string]()}), domain.CodeInvalidInput)

	// An archived product's settings stay editable, ready for its restore.
	off := false
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	if err := put(outlet, p.ID, ChannelSettingsInput{Price: set(int64(18000))}); err != nil {
		t.Fatalf("archived product: %v", err)
	}
}

// E-1, E-2, E-5, AC-12: overrides reach the menu, the bundle and Quote at
// their outlet and channel only.
func TestOverridesResolve(t *testing.T) {
	e := newOutletEnv(t)
	cat := e.category("Resolve")
	a, b := e.outletID(), e.outletID()
	off := false

	hidden := e.product(cat.ID, "Hidden", 15000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, hidden.ID, &ProductPatch{IsWebVisible: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetChannelSettings(e.ctx, e.owner, a, hidden.ID, ChannelWeb,
		&ChannelSettingsInput{IsVisible: set(true)}); err != nil {
		t.Fatal(err)
	}
	if e.menuProduct(a, cat.ID, hidden.ID) == nil || e.menuProduct(b, cat.ID, hidden.ID) != nil {
		t.Error("E-1: web override should show the product at outlet A only")
	}

	posOnly := e.product(cat.ID, "POS Only", 10000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, posOnly.ID, &ProductPatch{IsWebVisible: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetChannelSettings(e.ctx, e.owner, a, posOnly.ID, ChannelWeb,
		&ChannelSettingsInput{Price: set(int64(9000))}); err != nil {
		t.Fatal(err)
	}
	if e.menuProduct(a, cat.ID, posOnly.ID) != nil {
		t.Error("E-2: a price-only row must not make a POS-only product visible on web")
	}

	coffee := e.product(cat.ID, "Kopi", 20000)
	large, err := e.svc.CreateVariant(e.ctx, e.owner, coffee.ID, &VariantInput{Name: "Large", PriceDelta: 5000})
	if err != nil {
		t.Fatal(err)
	}
	g := e.group("Extras", 0, 3, "None", "Shot", "Syrup")
	if _, err := e.svc.SetProductGroups(e.ctx, e.owner, coffee.ID, &AttachInput{ModifierGroupIDs: []string{g.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetChannelSettings(e.ctx, e.owner, a, coffee.ID, ChannelWeb,
		&ChannelSettingsInput{Price: set(int64(23000))}); err != nil {
		t.Fatal(err)
	}
	line := QuoteLine{ProductID: uuid.MustParse(coffee.ID), VariantID: ptr(uuid.MustParse(large.ID)),
		ModifierIDs: []uuid.UUID{uuid.MustParse(g.Modifiers[1].ID), uuid.MustParse(g.Modifiers[2].ID)}}
	for _, tt := range []struct {
		outlet string
		ch     Channel
		want   int64
	}{{a, ChannelWeb, 23000 + 5000 + 1000 + 2000}, {a, ChannelPOS, 20000 + 8000}, {b, ChannelWeb, 20000 + 8000}} {
		res := e.quote(uuid.MustParse(tt.outlet), tt.ch, line)
		if s := res.Lines[0].Snapshot; s == nil || s.UnitPrice != tt.want || !res.Valid() {
			t.Errorf("%s at %s: %+v, want %d", tt.ch, tt.outlet, res.Lines[0], tt.want)
		}
	}
	if p := e.menuProduct(a, cat.ID, coffee.ID); p == nil || p.Price != 23000 {
		t.Errorf("menu price at A = %+v", p)
	}
	webOnly := e.product(cat.ID, "Web Only", 30000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, webOnly.ID, &ProductPatch{IsPOSVisible: &off}); err != nil {
		t.Fatal(err)
	}
	if sf, _ := e.bundle(a); !bundleHas(sf, posOnly.ID) || bundleHas(sf, webOnly.ID) {
		t.Error("bundle should hold POS-only items and no web-only ones")
	}
}

// AC-5, E-6: products and outlets are opt-out; nothing needs setting up.
func TestNewProductAppearsEverywhere(t *testing.T) {
	e := newOutletEnv(t)
	outlet := e.outletID()
	cat := e.category("New")
	p := e.product(cat.ID, "Baru", 12000)
	if e.menuProduct(outlet, cat.ID, p.ID) == nil {
		t.Fatal("new product missing from an existing outlet's menu")
	}
	if e.menuProduct(e.outletID(), cat.ID, p.ID) == nil {
		t.Fatal("product missing from a new outlet's menu")
	}
}

// AC-15, AC-16, FR-MENU-3, FR-MENU-5: the menu holds exactly the web-visible
// items, without cost, SKUs or override metadata.
func TestMenuContents(t *testing.T) {
	e := newOutletEnv(t)
	outlet := e.outletID()
	off := false
	cat := e.category("Menu")
	web := e.product(cat.ID, "Web", 10000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, web.ID, &ProductPatch{SKU: set("SKU-" + e.tag)}); err != nil {
		t.Fatal(err)
	}
	posOnly := e.product(cat.ID, "POS", 10000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, posOnly.ID, &ProductPatch{IsWebVisible: &off}); err != nil {
		t.Fatal(err)
	}
	archived := e.product(cat.ID, "Archived", 10000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, archived.ID, &ProductPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	gone := e.category("Gone")
	e.product(gone.ID, "In archived category", 10000)
	if _, err := e.svc.UpdateCategory(e.ctx, e.owner, gone.ID, &CategoryPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	empty := e.category("Only POS")
	hiddenOnWeb := e.product(empty.ID, "Kasir", 10000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, hiddenOnWeb.ID, &ProductPatch{IsWebVisible: &off}); err != nil {
		t.Fatal(err)
	}

	c := e.menuCategory(outlet, cat.ID)
	if c == nil || len(c.Products) != 1 || c.Products[0].ID != web.ID {
		t.Fatalf("menu category = %+v", c)
	}
	if e.menuCategory(outlet, gone.ID) != nil || e.menuCategory(outlet, empty.ID) != nil {
		t.Error("archived and empty categories must be absent")
	}
	m, _ := e.svc.Menu(e.ctx, outlet)
	body, _ := json.Marshal(m)
	for _, banned := range []string{"cost_price", "sku", "SKU-" + e.tag, "override", "updated_by", e.owner.StaffID} {
		if strings.Contains(string(body), banned) {
			t.Errorf("menu contains %q", banned)
		}
	}
	bundle, _, err := e.svc.Bundle(e.ctx, outlet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bundle), "cost_price") {
		t.Error("bundle contains cost_price")
	}
}

// AC-6: "today" expires at the next business-day cutoff in outlet time;
// "indefinite" never does.
func TestSoldOutScopes(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Scope").ID, "Croissant", 18000)
	q := e.product(e.category("Scope2").ID, "Roti", 8000)
	outlet := e.outletID()
	jkt, _ := location("Asia/Jakarta")
	item := SoldOutItem{ProductID: &p.ID}

	e.svc.now = func() time.Time { return time.Date(2026, 10, 30, 14, 0, 0, 0, jkt) }
	res := e.toggle(outlet, item, true, ScopeToday)
	if want := time.Date(2026, 10, 31, 4, 0, 0, 0, jkt); !res.Applied || !res.SoldOut || !res.SoldOutUntil.Equal(want) {
		t.Fatalf("after cutoff: %+v, want until %v", res, want)
	}
	e.svc.now = func() time.Time { return time.Date(2026, 10, 31, 2, 0, 0, 0, jkt) }
	res = e.toggle(outlet, SoldOutItem{ProductID: &q.ID}, true, "")
	if want := time.Date(2026, 10, 31, 4, 0, 0, 0, jkt); !res.SoldOutUntil.Equal(want) {
		t.Fatalf("before cutoff: until %v, want %v", res.SoldOutUntil, want)
	}
	e.svc.now = func() time.Time { return time.Date(2026, 10, 31, 4, 0, 0, 0, jkt) }
	if items, _ := e.svc.ListSoldOut(e.ctx, outlet); len(items) != 0 {
		t.Fatalf("expired items still listed: %+v", items)
	}
	if mp := e.menuProduct(outlet, p.CategoryID, p.ID); mp.SoldOut || !mp.Orderable {
		t.Fatalf("expired item still sold out on the menu: %+v", mp)
	}

	res = e.toggle(outlet, item, true, ScopeIndefinite)
	if res.SoldOutUntil != nil || !res.Applied {
		t.Fatalf("indefinite: %+v", res)
	}
	e.svc.now = func() time.Time { return time.Date(2027, 1, 1, 12, 0, 0, 0, jkt) }
	items, err := e.svc.ListSoldOut(e.ctx, outlet)
	if err != nil || len(items) != 1 || *items[0].Item.ProductID != p.ID || items[0].ChangedBy.ID != e.owner.StaffID ||
		items[0].ChangedVia != ViaDashboard || items[0].Name != p.Name {
		t.Fatalf("indefinite list: %v %+v", err, items)
	}
}

// AC-7, AC-11, NFR-PERF-4: a POS toggle reaches the cached web menu of its
// outlet on the next request, and no other outlet.
func TestPOSSoldOutReachesMenu(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Reach").ID, "Croissant", 18000)
	a, b := e.outletID(), e.outletID()
	if e.menuProduct(a, p.CategoryID, p.ID).SoldOut {
		t.Fatal("sold out before any toggle")
	}
	sold := true
	w, _ := NewSoldOutWrite(a, &SoldOutInput{Item: SoldOutItem{ProductID: &p.ID}, SoldOut: &sold},
		e.staffRow("Cashier"), ViaPOS)
	device := e.device(a)
	w.DeviceID, w.ChangedAt = &device, time.Now()
	if _, err := e.svc.SetSoldOut(e.ctx, w); err != nil {
		t.Fatal(err)
	}
	if mp := e.menuProduct(a, p.CategoryID, p.ID); !mp.SoldOut || mp.Orderable {
		t.Errorf("outlet A after toggle: %+v", mp)
	}
	if e.menuProduct(b, p.CategoryID, p.ID).SoldOut {
		t.Error("outlet B shows A's sold out")
	}
}

// AC-10, E-12, E-13, E-14: late and repeated toggles change nothing; a
// future device time is clamped.
func TestSoldOutOrdering(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Order").ID, "Croissant", 18000)
	outlet := e.outletID()
	device := e.device(outlet)
	cashier := e.staffRow("Cashier")
	now := time.Now().Truncate(time.Microsecond)
	e.svc.now = func() time.Time { return now }
	posToggle := func(sold bool, at time.Time) *SoldOutResult {
		t.Helper()
		w, _ := NewSoldOutWrite(outlet, &SoldOutInput{Item: SoldOutItem{ProductID: &p.ID}, SoldOut: &sold,
			Scope: ScopeIndefinite}, cashier, ViaPOS)
		w.DeviceID, w.ChangedAt = &device, at
		res, err := e.svc.SetSoldOut(e.ctx, w)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := posToggle(true, now.Add(time.Hour)); !res.Applied || !res.ChangedAt.Equal(now) {
		t.Fatalf("E-13 future time: %+v", res)
	}
	if res := posToggle(true, now); res.Applied {
		t.Fatalf("E-14 repeat applied: %+v", res)
	}
	if got := len(outletAudits(t, p.ID, "sold_out")); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
	// HQ clears at 10:05, the POS's 10:00 toggle arrives at 10:10.
	e.toggle(outlet, SoldOutItem{ProductID: &p.ID}, false, "")
	res := posToggle(true, now.Add(-5*time.Minute))
	if res.Applied || res.SoldOut || *res.ChangedVia != ViaDashboard {
		t.Fatalf("E-12 late toggle: %+v", res)
	}
	rows := outletAudits(t, p.ID, "sold_out")
	if len(rows) != 2 || rows[1].Action != "sold_out.cleared" {
		t.Fatalf("audit = %+v", rows)
	}
	first := rows[0]
	if first.Action != "sold_out.set" || *first.ActorID != cashier || *first.OutletID != outlet ||
		first.After["device_id"] != device || first.After["changed_at"] == nil {
		t.Errorf("POS audit row = %+v", first)
	}
}

// FR-SO-7, E-17: unknown or archived targets are not found.
func TestSoldOutTargets(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Targets").ID, "Kopi", 20000)
	v1, _ := e.svc.CreateVariant(e.ctx, e.owner, p.ID, &VariantInput{Name: "Regular"})
	v2, _ := e.svc.CreateVariant(e.ctx, e.owner, p.ID, &VariantInput{Name: "Large"})
	off := false
	if _, err := e.svc.UpdateVariant(e.ctx, e.owner, p.ID, v2.ID, &VariantPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	outlet := e.outletID()
	sold := true
	try := func(outletID string, item SoldOutItem) error {
		w, err := NewSoldOutWrite(outletID, &SoldOutInput{Item: item, SoldOut: &sold}, e.owner.StaffID, ViaDashboard)
		if err != nil {
			return err
		}
		_, err = e.svc.SetSoldOut(e.ctx, w)
		return err
	}
	unknown := uuid.NewString()
	wantCode(t, try(outlet, SoldOutItem{VariantID: &v2.ID}), domain.CodeNotFound)
	wantCode(t, try(outlet, SoldOutItem{ModifierID: &unknown}), domain.CodeNotFound)
	wantCode(t, try(outlet, SoldOutItem{ProductID: &p.ID, VariantID: &v1.ID}), domain.CodeInvalidInput)
	wantCode(t, try(e.outlet("Asia/Jakarta", false).String(), SoldOutItem{ProductID: &p.ID}), domain.CodeNotFound)
	if err := try(outlet, SoldOutItem{VariantID: &v1.ID}); err != nil {
		t.Fatal(err)
	}
	// The only active variant is sold out (E-11).
	if mp := e.menuProduct(outlet, p.CategoryID, p.ID); mp.Orderable || mp.SoldOut {
		t.Errorf("menu = %+v", mp)
	}
}

// AC-13, E-15, E-18: Quote reports each sold-out kind, evaluated at At.
func TestQuoteSoldOut(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("QSold").ID, "Kopi", 20000)
	v, _ := e.svc.CreateVariant(e.ctx, e.owner, p.ID, &VariantInput{Name: "Regular"})
	g := e.group("Temp", 1, 1, "Hot", "Iced")
	if _, err := e.svc.SetProductGroups(e.ctx, e.owner, p.ID, &AttachInput{ModifierGroupIDs: []string{g.ID}}); err != nil {
		t.Fatal(err)
	}
	outlet := e.outletID()
	line := QuoteLine{ProductID: uuid.MustParse(p.ID), VariantID: ptr(uuid.MustParse(v.ID)),
		ModifierIDs: []uuid.UUID{uuid.MustParse(g.Modifiers[0].ID)}}
	quoteAt := func(at time.Time) []ProblemKind {
		res, err := Quote(e.ctx, testDB.Pool, QuoteRequest{OutletID: uuid.MustParse(outlet), Channel: ChannelWeb,
			At: at, Lines: []QuoteLine{line}})
		if err != nil {
			t.Fatal(err)
		}
		return kinds(res.Lines[0].Problems)
	}
	now := time.Now()
	for _, tt := range []struct {
		item SoldOutItem
		want ProblemKind
	}{
		{SoldOutItem{ProductID: &p.ID}, ProblemProductSoldOut},
		{SoldOutItem{VariantID: &v.ID}, ProblemVariantSoldOut},
		{SoldOutItem{ModifierID: &g.Modifiers[0].ID}, ProblemModifierSoldOut},
	} {
		res := e.toggle(outlet, tt.item, true, ScopeToday)
		if got := quoteAt(now); !slices.Equal(got, []ProblemKind{tt.want}) {
			t.Errorf("%s: problems %v", tt.want, got)
		}
		if got := quoteAt(*res.SoldOutUntil); len(got) != 0 {
			t.Errorf("%s at until (E-18): problems %v", tt.want, got)
		}
		e.toggle(outlet, tt.item, false, "")
	}
}

// AC-14, FR-OQ-4: with the cache stale, Quote still prices from Postgres.
func TestQuoteIgnoresCache(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Stale").ID, "Kopi", 20000)
	outlet := e.outletID()
	if mp := e.menuProduct(outlet, p.CategoryID, p.ID); mp.Price != 20000 {
		t.Fatalf("menu price %d", mp.Price)
	}
	// A write that skipped the generation bump leaves the cache stale.
	if _, err := testDB.Pool.Exec(e.ctx, `UPDATE products SET base_price = 25000 WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if mp := e.menuProduct(outlet, p.CategoryID, p.ID); mp.Price != 20000 {
		t.Fatalf("expected the stale cached price, got %d", mp.Price)
	}
	res := e.quote(uuid.MustParse(outlet), ChannelWeb, QuoteLine{ProductID: uuid.MustParse(p.ID)})
	if res.Lines[0].Snapshot.UnitPrice != 25000 {
		t.Fatalf("quote price %d, want 25000", res.Lines[0].Snapshot.UnitPrice)
	}
}

// AC-18, E-19, FR-CACHE-4: the bundle version ignores the clock and writes
// that do not touch its content.
func TestBundleVersionStable(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Bundle").ID, "Kopi", 20000)
	a, b := e.outletID(), e.outletID()
	e.toggle(a, SoldOutItem{ProductID: &p.ID}, true, ScopeToday)
	_, v1 := e.bundle(a)
	e.svc.now = func() time.Time { return time.Now().Add(30 * time.Hour) }
	_, gen := e.store.bumps(b)
	e.toggle(b, SoldOutItem{ProductID: &p.ID}, true, ScopeIndefinite)
	if _, after := e.store.bumps(b); after != gen+1 {
		t.Fatal("toggle did not bump the outlet generation")
	}
	sf, v2 := e.bundle(a)
	if v1 != v2 {
		t.Fatalf("version changed without a content change: %s -> %s", v1, v2)
	}
	if sf.Outlet.BusinessDayCutoff != "04:00" || sf.Outlet.Timezone != "Asia/Jakarta" || len(sf.SoldOut) == 0 {
		t.Errorf("bundle outlet/sold-out = %+v / %+v", sf.Outlet, sf.SoldOut)
	}
	price := int64(21000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{BasePrice: &price}); err != nil {
		t.Fatal(err)
	}
	if _, v3 := e.bundle(a); v3 == v1 {
		t.Fatal("price change kept the version")
	}
}

// Outlet-scoped writes bump only that outlet's generation, and only when
// they change something; the other outlet keeps its cached menu.
func TestOutletWritesInvalidateOnlyOnChange(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Scope").ID, "Kopi", 20000)
	a, b := e.outletID(), e.outletID()
	global, _ := e.store.bumps(a)
	want := func(step string, wantA int64) {
		t.Helper()
		g, ga := e.store.bumps(a)
		_, gb := e.store.bumps(b)
		if g != global || ga != wantA || gb != 0 {
			t.Fatalf("%s: global %d->%d, outlet a %d (want %d), outlet b %d (want 0)", step, global, g, ga, wantA, gb)
		}
	}
	put := func() {
		t.Helper()
		if _, err := e.svc.SetChannelSettings(e.ctx, e.owner, a, p.ID, ChannelWeb,
			&ChannelSettingsInput{Price: set(int64(25000))}); err != nil {
			t.Fatal(err)
		}
	}
	reset := func() {
		t.Helper()
		if err := e.svc.ResetChannelSettings(e.ctx, e.owner, a, p.ID, ChannelWeb); err != nil {
			t.Fatal(err)
		}
	}

	e.menuProduct(b, p.CategoryID, p.ID)
	put()
	want("price override", 1)
	put()
	want("repeated override", 1)
	reset()
	want("reset", 2)
	reset()
	want("repeated reset", 2)
	e.toggle(a, SoldOutItem{ProductID: &p.ID}, true, ScopeIndefinite)
	want("sold out", 3)
	e.toggle(a, SoldOutItem{ProductID: &p.ID}, true, ScopeIndefinite)
	want("repeated sold out", 3)

	key := fmt.Sprintf("catalog:%s:%d.0:%s", docMenu, global, b)
	e.store.mu.Lock()
	_, cached := e.store.docs[key]
	e.store.mu.Unlock()
	if !cached {
		t.Fatal("outlet b's cached menu was dropped by writes to outlet a")
	}
}

// AC-22, E-22, E-23: with Redis down the menu and bundle come from
// Postgres; a failed bump does not fail the write.
func TestRedisDown(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("Down").ID, "Kopi", 20000)
	outlet := e.outletID()
	e.store.down = true
	price := int64(22000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{BasePrice: &price}); err != nil {
		t.Fatalf("write with redis down: %v", err)
	}
	if mp := e.menuProduct(outlet, p.CategoryID, p.ID); mp == nil || mp.Price != 22000 {
		t.Fatalf("menu with redis down: %+v", mp)
	}
	if sf, _ := e.bundle(outlet); !bundleHas(sf, p.ID) {
		t.Fatal("bundle with redis down is missing the product")
	}
	e.store.down = false
	e.store.bumpFail = true
	e.toggle(outlet, SoldOutItem{ProductID: &p.ID}, true, ScopeIndefinite)
	if items, _ := e.svc.ListSoldOut(e.ctx, outlet); len(items) != 1 {
		t.Fatal("toggle with a failed bump was not saved")
	}
}

type fakeVerifier struct{ p *domain.Principal }

func (f fakeVerifier) Verify(string, ...domain.Audience) (*domain.Principal, error) { return f.p, nil }

type noDenylist struct{}

func (noDenylist) IsTokenDenied(context.Context, string) (bool, error) { return false, nil }

type fakeAccess map[string]*domain.StaffAccess

func (f fakeAccess) Access(_ context.Context, id string) (*domain.StaffAccess, error) {
	if a, ok := f[id]; ok {
		return a, nil
	}
	return &domain.StaffAccess{StaffID: id}, nil
}

// router mounts the public menu and the POS routes as the server does, for
// a device at outletID.
func (e *oenv) router(outletID, deviceID string, staff fakeAccess) http.Handler {
	h := NewHandler(e.svc)
	r := chi.NewRouter()
	r.Get("/v1/outlets/{outletID}/menu", h.Menu)
	p := &domain.Principal{Subject: deviceID, Audience: domain.AudiencePOS, OutletID: outletID, DeviceID: deviceID}
	pos := r.With(middleware.Authenticate(fakeVerifier{p}, noDenylist{}, domain.AudiencePOS))
	pos.Get("/v1/pos/catalog", h.Bundle)
	pos.With(middleware.RequireActingStaff(staff)).Put("/v1/pos/sold-out", h.POSSetSoldOut)
	return r
}

func call(h http.Handler, method, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// AC-9, AC-16: the POS sold-out route needs an acting staff member with
// the permission at the device's outlet, and names them and the device.
func TestPOSSoldOutRoute(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("POS route").ID, "Kopi", 20000)
	outlet, elsewhere := e.outletID(), e.outletID()
	device := e.device(outlet)
	cashier, stranger, noPerm := e.staffRow("Cashier"), e.staffRow("Stranger"), e.staffRow("NoPerm")
	h := e.router(outlet, device, fakeAccess{
		cashier:  staffAt(cashier, outlet, domain.RoleCashier, domain.PermSoldOutToggle),
		stranger: staffAt(stranger, elsewhere, domain.RoleCashier, domain.PermSoldOutToggle),
		noPerm:   staffAt(noPerm, outlet, "viewer"),
	})
	body := `{"item":{"product_id":"` + p.ID + `"},"sold_out":true,"changed_at":"` +
		time.Now().UTC().Format(time.RFC3339) + `"}`
	for _, tt := range []struct {
		name  string
		staff string
		want  int
	}{{"no header", "", 400}, {"no role at outlet", stranger, 403}, {"no permission", noPerm, 403}} {
		headers := map[string]string{}
		if tt.staff != "" {
			headers["X-Acting-Staff-Id"] = tt.staff
		}
		if rec := call(h, http.MethodPut, "/v1/pos/sold-out", headers, body); rec.Code != tt.want {
			t.Errorf("%s: %d %s", tt.name, rec.Code, rec.Body)
		}
	}
	rec := call(h, http.MethodPut, "/v1/pos/sold-out", map[string]string{"X-Acting-Staff-Id": cashier}, body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"applied":true`) ||
		strings.Contains(rec.Body.String(), "cost_price") {
		t.Fatalf("cashier: %d %s", rec.Code, rec.Body)
	}
	rows := outletAudits(t, p.ID, "sold_out")
	if len(rows) != 1 || *rows[0].ActorID != cashier || rows[0].After["device_id"] != device {
		t.Fatalf("audit = %+v", rows)
	}
	noChangedAt := `{"item":{"product_id":"` + p.ID + `"},"sold_out":false}`
	if rec := call(h, http.MethodPut, "/v1/pos/sold-out", map[string]string{"X-Acting-Staff-Id": cashier},
		noChangedAt); rec.Code != 400 {
		t.Errorf("missing changed_at: %d", rec.Code)
	}
}

// AC-17, AC-19, AC-20: ETags give 304 until visible content changes; every
// bundle fetch is recorded on the device.
func TestETagsAndDeviceRecord(t *testing.T) {
	e := newOutletEnv(t)
	p := e.product(e.category("ETag").ID, "Kopi", 20000)
	outlet := e.outletID()
	device := e.device(outlet)
	h := e.router(outlet, device, fakeAccess{})
	deviceState := func() (*string, *time.Time) {
		var v *string
		var at *time.Time
		if err := testDB.Pool.QueryRow(e.ctx, `SELECT catalog_version, catalog_fetched_at FROM devices WHERE id = $1`,
			device).Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		return v, at
	}

	for _, path := range []string{"/v1/outlets/" + outlet + "/menu", "/v1/pos/catalog"} {
		first := call(h, http.MethodGet, path, nil, "")
		tag := first.Header().Get("ETag")
		if first.Code != 200 || tag == "" || first.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d etag %q", path, first.Code, tag)
		}
		if again := call(h, http.MethodGet, path, map[string]string{"If-None-Match": tag}, ""); again.Code != 304 ||
			again.Body.Len() != 0 {
			t.Fatalf("%s revalidate: %d", path, again.Code)
		}
		if path == "/v1/pos/catalog" {
			v, at := deviceState()
			if v == nil || `"`+*v+`"` != tag || at == nil {
				t.Fatalf("device after 304: %v %v", v, at)
			}
		}
	}

	_, before := deviceState()
	menuTag := call(h, http.MethodGet, "/v1/outlets/"+outlet+"/menu", nil, "").Header().Get("ETag")
	bundleTag := call(h, http.MethodGet, "/v1/pos/catalog", nil, "").Header().Get("ETag")
	price := int64(21000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{BasePrice: &price}); err != nil {
		t.Fatal(err)
	}
	for path, tag := range map[string]string{"/v1/outlets/" + outlet + "/menu": menuTag, "/v1/pos/catalog": bundleTag} {
		rec := call(h, http.MethodGet, path, map[string]string{"If-None-Match": tag}, "")
		if rec.Code != 200 || rec.Header().Get("ETag") == tag {
			t.Errorf("%s after price change: %d, etag %q", path, rec.Code, rec.Header().Get("ETag"))
		}
	}
	if v, after := deviceState(); !after.After(*before) || `"`+*v+`"` == bundleTag {
		t.Errorf("device not updated on 200: %v %v", v, after)
	}

	for _, id := range []string{e.outlet("Asia/Jakarta", false).String(), uuid.NewString(), "nope"} {
		if rec := call(h, http.MethodGet, "/v1/outlets/"+id+"/menu", nil, ""); rec.Code != 404 {
			t.Errorf("menu for %s: %d", id, rec.Code)
		}
	}
}
