package catalog

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/migrations"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Integration tests run against a database named by TEST_DATABASE_URL,
// which they migrate. Every test creates its own uniquely named rows, so
// they can share one database and run in any order.

var testDB *postgres.DB

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	db, err := postgres.Connect(ctx, postgres.Config{DSN: dsn, MaxConns: 5})
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

type env struct {
	t     *testing.T
	ctx   context.Context
	svc   *Service
	owner *domain.StaffAccess
	tag   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if testDB == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return &env{
		t:   t,
		ctx: context.Background(),
		svc: NewService(NewRepository(testDB), 100_000_000),
		owner: &domain.StaffAccess{StaffID: uuid.NewString(), Active: true, Global: domain.AccessScope{
			Roles: []string{domain.RoleOwner}, Permissions: []string{domain.PermCatalogManage, domain.PermCostView},
		}},
		tag: uuid.NewString()[:8],
	}
}

func outletAccess(role string, perms ...string) *domain.StaffAccess {
	return &domain.StaffAccess{StaffID: uuid.NewString(), Active: true, Outlets: map[string]domain.AccessScope{
		uuid.NewString(): {Roles: []string{role}, Permissions: perms},
	}}
}

func (e *env) name(s string) string { return s + " " + e.tag }

func (e *env) category(name string) *Category {
	e.t.Helper()
	c, err := e.svc.CreateCategory(e.ctx, e.owner, &CategoryInput{Name: e.name(name)})
	if err != nil {
		e.t.Fatalf("create category: %v", err)
	}
	return c
}

func (e *env) product(catID, name string, price int64) *Product {
	e.t.Helper()
	cost := int64(7000)
	p, err := e.svc.CreateProduct(e.ctx, e.owner, &ProductInput{
		CategoryID: catID, Name: e.name(name), BasePrice: &price, CostPrice: &cost,
	})
	if err != nil {
		e.t.Fatalf("create product: %v", err)
	}
	return p
}

func (e *env) group(name string, minSel, maxSel int32, modifiers ...string) *ModifierGroup {
	e.t.Helper()
	g, err := e.svc.CreateGroup(e.ctx, e.owner, &GroupInput{Name: e.name(name), MinSelect: minSel, MaxSelect: &maxSel})
	if err != nil {
		e.t.Fatalf("create group: %v", err)
	}
	for i, m := range modifiers {
		mod, err := e.svc.CreateModifier(e.ctx, e.owner, g.ID,
			&ModifierInput{Name: m, PriceDelta: int64(i * 1000), SortOrder: int32(i)})
		if err != nil {
			e.t.Fatalf("create modifier: %v", err)
		}
		g.Modifiers = append(g.Modifiers, *mod)
	}
	return g
}

func (e *env) outlet(tz string, active bool) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	if _, err := testDB.Pool.Exec(e.ctx, `INSERT INTO outlets (id, name, timezone, is_active) VALUES ($1, $2, $3, $4)`,
		id, e.name("Outlet"), tz, active); err != nil {
		e.t.Fatalf("insert outlet: %v", err)
	}
	return id
}

func (e *env) quote(outlet uuid.UUID, ch Channel, lines ...QuoteLine) QuoteResult {
	e.t.Helper()
	res, err := Quote(e.ctx, testDB.Pool, QuoteRequest{OutletID: outlet, Channel: ch, At: time.Now(), Lines: lines})
	if err != nil {
		e.t.Fatalf("quote: %v", err)
	}
	return res
}

func wantCode(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	_ = wantErr(t, err, code)
}

// wantErr is wantCode returning the error, for tests that inspect it.
func wantErr(t *testing.T, err error, code domain.ErrorCode) *domain.Error {
	t.Helper()
	de := domain.AsError(err)
	if de == nil || de.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
	return de
}

func auditRows(t *testing.T, entityID string) []auditRow {
	t.Helper()
	rows, err := testDB.Pool.Query(context.Background(),
		`SELECT action, value_delta, before, after FROM audit_logs WHERE entity_id = $1 ORDER BY created_at`, entityID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[auditRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type auditRow struct {
	Action string
	Delta  *int64
	Before map[string]any
	After  map[string]any
}

func ids(products []Product) []string {
	out := make([]string, len(products))
	for i, p := range products {
		out[i] = p.ID
	}
	return out
}

// AC-1, E-1, E-2: archive hides, include_archived shows, restore brings back.
func TestArchiveRestore(t *testing.T) {
	e := newEnv(t)
	cat := e.category("Seasonal")
	p := e.product(cat.ID, "Es Buah", 15000)

	off := false
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	list, _ := e.svc.ListProducts(e.ctx, e.owner, ProductFilter{CategoryID: &cat.ID})
	if len(list) != 0 {
		t.Fatalf("archived product listed: %v", ids(list))
	}
	list, _ = e.svc.ListProducts(e.ctx, e.owner, ProductFilter{CategoryID: &cat.ID, IncludeArchived: true})
	if !slices.Equal(ids(list), []string{p.ID}) {
		t.Fatalf("include_archived = %v", ids(list))
	}
	on := true
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{IsActive: &on}); err != nil {
		t.Fatal(err)
	}
	actions := []string{}
	for _, a := range auditRows(t, p.ID) {
		actions = append(actions, a.Action)
	}
	if !slices.Equal(actions, []string{"product.created", "product.archived", "product.restored"}) {
		t.Errorf("audit actions = %v", actions)
	}

	// Categories: archived hidden by default.
	if _, err := e.svc.UpdateCategory(e.ctx, e.owner, cat.ID, &CategoryPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	cats, _ := e.svc.ListCategories(e.ctx, false)
	for _, c := range cats {
		if c.ID == cat.ID {
			t.Fatal("archived category listed")
		}
	}
	// E-2: a product can be created in an archived category.
	e.product(cat.ID, "Staged", 10000)
}

// AC-2, E-3, E-4: names unique per scope among active rows.
func TestNameTaken(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.CreateCategory(e.ctx, e.owner, &CategoryInput{Name: strings.ToUpper(e.name("Kopi"))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CreateCategory(e.ctx, e.owner, &CategoryInput{Name: "  " + e.name("kopi") + " "})
	wantCode(t, err, domain.CodeNameTaken)

	a, b := e.category("A"), e.category("B")
	first := e.product(a.ID, "Original", 1000)
	price := int64(1000)
	_, err = e.svc.CreateProduct(e.ctx, e.owner, &ProductInput{CategoryID: a.ID, Name: e.name("original"), BasePrice: &price})
	wantCode(t, err, domain.CodeNameTaken)
	e.product(b.ID, "Original", 1000) // E-3: other category

	// E-4: archive, reuse the name, then restoring the first collides.
	off, on := false, true
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, first.ID, &ProductPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	e.product(a.ID, "Original", 2000)
	_, err = e.svc.UpdateProduct(e.ctx, e.owner, first.ID, &ProductPatch{IsActive: &on})
	wantCode(t, err, domain.CodeNameTaken)

	if _, err := e.svc.CreateVariant(e.ctx, e.owner, first.ID, &VariantInput{Name: "Large"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CreateVariant(e.ctx, e.owner, first.ID, &VariantInput{Name: "large"})
	wantCode(t, err, domain.CodeNameTaken)

	g := e.group("Sugar", 0, 1, "Less")
	_, err = e.svc.CreateModifier(e.ctx, e.owner, g.ID, &ModifierInput{Name: "LESS"})
	wantCode(t, err, domain.CodeNameTaken)
	other := e.group("Ice", 0, 1)
	if _, err := e.svc.CreateModifier(e.ctx, e.owner, other.ID, &ModifierInput{Name: "Less"}); err != nil {
		t.Fatalf("same modifier name in another group: %v", err)
	}
}

// AC-3, E-5, E-6: SKUs are unique across products and variants, archived
// included.
func TestSKUTaken(t *testing.T) {
	e := newEnv(t)
	cat := e.category("SKU")
	price := int64(1000)
	sku := "SKU-" + e.tag
	p, err := e.svc.CreateProduct(e.ctx, e.owner, &ProductInput{CategoryID: cat.ID, Name: e.name("One"), BasePrice: &price, SKU: &sku})
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(sku)
	_, err = e.svc.CreateProduct(e.ctx, e.owner, &ProductInput{CategoryID: cat.ID, Name: e.name("Two"), BasePrice: &price, SKU: &lower})
	wantCode(t, err, domain.CodeSKUTaken)

	q := e.product(cat.ID, "Three", 1000)
	_, err = e.svc.CreateVariant(e.ctx, e.owner, q.ID, &VariantInput{Name: "Reg", SKU: &sku})
	wantCode(t, err, domain.CodeSKUTaken)

	vsku := "VAR-" + e.tag
	if _, err := e.svc.CreateVariant(e.ctx, e.owner, q.ID, &VariantInput{Name: "Reg", SKU: &vsku}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.UpdateProduct(e.ctx, e.owner, q.ID, &ProductPatch{SKU: Nullable[string]{Set: true, Valid: true, V: vsku}})
	wantCode(t, err, domain.CodeSKUTaken)
}

// AC-4: out-of-range prices on every price field.
func TestPriceRange(t *testing.T) {
	e := newEnv(t)
	cat := e.category("Prices")
	p := e.product(cat.ID, "Item", 1000)
	g := e.group("G", 0, 1, "M")
	for _, bad := range []int64{-1, 100_000_001} {
		_, err := e.svc.CreateProduct(e.ctx, e.owner, &ProductInput{CategoryID: cat.ID, Name: e.name("X"), BasePrice: &bad})
		wantCode(t, err, domain.CodeInvalidInput)
		_, err = e.svc.CreateProduct(e.ctx, e.owner, &ProductInput{CategoryID: cat.ID, Name: e.name("X"), BasePrice: ptr(int64(1)), CostPrice: &bad})
		wantCode(t, err, domain.CodeInvalidInput)
		_, err = e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{BasePrice: &bad})
		wantCode(t, err, domain.CodeInvalidInput)
		_, err = e.svc.CreateVariant(e.ctx, e.owner, p.ID, &VariantInput{Name: "V", PriceDelta: bad})
		wantCode(t, err, domain.CodeInvalidInput)
		_, err = e.svc.CreateModifier(e.ctx, e.owner, g.ID, &ModifierInput{Name: "N", PriceDelta: bad})
		wantCode(t, err, domain.CodeInvalidInput)
		_, err = e.svc.UpdateModifier(e.ctx, e.owner, g.ID, g.Modifiers[0].ID, &ModifierPatch{PriceDelta: &bad})
		wantCode(t, err, domain.CodeInvalidInput)
	}
	// A string sent for the nullable cost_price is reported on its field.
	var patch ProductPatch
	if err := json.Unmarshal([]byte(`{"cost_price":"5000"}`), &patch); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &patch)
	if de := wantErr(t, err, domain.CodeInvalidInput); de.Fields["cost_price"] == "" {
		t.Errorf("fields = %v", de.Fields)
	}
}

// AC-5, E-7.
func TestLastActiveVariant(t *testing.T) {
	e := newEnv(t)
	p := e.product(e.category("Sizes").ID, "Kopi", 20000)
	reg, err := e.svc.CreateVariant(e.ctx, e.owner, p.ID, &VariantInput{Name: "Regular"})
	if err != nil {
		t.Fatal(err)
	}
	large, err := e.svc.CreateVariant(e.ctx, e.owner, p.ID, &VariantInput{Name: "Large", PriceDelta: 5000})
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := e.svc.UpdateVariant(e.ctx, e.owner, p.ID, large.ID, &VariantPatch{IsActive: &off}); err != nil {
		t.Fatalf("archive non-last variant: %v", err)
	}
	_, err = e.svc.UpdateVariant(e.ctx, e.owner, p.ID, reg.ID, &VariantPatch{IsActive: &off})
	wantCode(t, err, domain.CodeLastActiveVariant)
	// The variant must belong to the product in the URL.
	other := e.product(e.category("Other").ID, "Teh", 1000)
	_, err = e.svc.UpdateVariant(e.ctx, e.owner, other.ID, reg.ID, &VariantPatch{IsActive: &off})
	wantCode(t, err, domain.CodeNotFound)
}

// AC-6, E-8, E-9, E-10.
func TestModifierGroupGuards(t *testing.T) {
	e := newEnv(t)
	cat := e.category("Guards")
	p := e.product(cat.ID, "Kopi", 20000)
	q := e.product(cat.ID, "Teh", 10000)
	temp := e.group("Temp", 1, 1, "Hot", "Iced")
	empty := e.group("Empty", 1, 1)

	// Attaching a required group with no modifiers.
	_, err := e.svc.SetProductGroups(e.ctx, e.owner, p.ID, &AttachInput{ModifierGroupIDs: []string{empty.ID}})
	de := wantErr(t, err, domain.CodeModifierGroupUnsatisfiable)
	if got := de.Details.(map[string][]AffectedProduct)["products"]; len(got) != 1 || got[0].ID != p.ID {
		t.Errorf("affected = %v", got)
	}

	for _, prod := range []*Product{p, q} {
		if _, err := e.svc.SetProductGroups(e.ctx, e.owner, prod.ID, &AttachInput{ModifierGroupIDs: []string{temp.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	// E-10: raising a shared group's min_select past its modifiers.
	three := int32(3)
	_, err = e.svc.UpdateGroup(e.ctx, e.owner, temp.ID, &GroupPatch{MinSelect: &three, MaxSelect: &three})
	de = wantErr(t, err, domain.CodeModifierGroupUnsatisfiable)
	if got := de.Details.(map[string][]AffectedProduct)["products"]; len(got) != 2 {
		t.Errorf("affected = %v", got)
	}
	// E-8: archiving one modifier is fine, archiving the second is not...
	two := int32(2)
	if _, err := e.svc.UpdateGroup(e.ctx, e.owner, temp.ID, &GroupPatch{MinSelect: &two, MaxSelect: &two}); err != nil {
		t.Fatal(err)
	}
	off := false
	_, err = e.svc.UpdateModifier(e.ctx, e.owner, temp.ID, temp.Modifiers[0].ID, &ModifierPatch{IsActive: &off})
	wantCode(t, err, domain.CodeModifierGroupUnsatisfiable)

	// E-9: archiving an attached group.
	_, err = e.svc.UpdateGroup(e.ctx, e.owner, temp.ID, &GroupPatch{IsActive: &off})
	wantCode(t, err, domain.CodeModifierGroupInUse)

	// Detached from both, it archives.
	for _, prod := range []*Product{p, q} {
		if _, err := e.svc.SetProductGroups(e.ctx, e.owner, prod.ID, &AttachInput{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.UpdateGroup(e.ctx, e.owner, temp.ID, &GroupPatch{IsActive: &off}); err != nil {
		t.Fatalf("archive detached group: %v", err)
	}
}

// AC-7: a request containing an archived group changes nothing.
func TestAttachAtomic(t *testing.T) {
	e := newEnv(t)
	p := e.product(e.category("Attach").ID, "Kopi", 20000)
	a := e.group("A", 0, 1, "x")
	b := e.group("B", 0, 1, "y")
	archived := e.group("Archived", 0, 1)
	off := false
	if _, err := e.svc.UpdateGroup(e.ctx, e.owner, archived.ID, &GroupPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.SetProductGroups(e.ctx, e.owner, p.ID, &AttachInput{ModifierGroupIDs: []string{b.ID, a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Fatalf("attached order = %+v", got)
	}
	_, err = e.svc.SetProductGroups(e.ctx, e.owner, p.ID, &AttachInput{ModifierGroupIDs: []string{a.ID, archived.ID}})
	wantCode(t, err, domain.CodeInvalidInput)
	d, err := e.svc.GetProduct(e.ctx, e.owner, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ModifierGroups) != 2 || d.ModifierGroups[0].ID != b.ID {
		t.Errorf("attachments changed: %+v", d.ModifierGroups)
	}
	// Re-sending the same set is a no-op: one groups_changed row only.
	if _, err := e.svc.SetProductGroups(e.ctx, e.owner, p.ID, &AttachInput{ModifierGroupIDs: []string{b.ID, a.ID}}); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range auditRows(t, p.ID) {
		if r.Action == "product.groups_changed" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("groups_changed rows = %d, want 1", n)
	}
}

// AC-8, AC-15, AC-17, E-1.
func TestQuoteAvailability(t *testing.T) {
	e := newEnv(t)
	cat := e.category("Quote")
	p := e.product(cat.ID, "POS Only", 12000)
	off := false
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{IsWebVisible: &off}); err != nil {
		t.Fatal(err)
	}
	outlet := e.outlet("Asia/Jakarta", true)
	line := QuoteLine{ProductID: uuid.MustParse(p.ID)}

	web := e.quote(outlet, ChannelWeb, line)
	if !slices.Equal(kinds(web.Lines[0].Problems), []ProblemKind{ProblemProductUnavailable}) {
		t.Errorf("web problems = %v", kinds(web.Lines[0].Problems))
	}
	pos := e.quote(outlet, ChannelPOS, line)
	if !pos.Valid() || pos.Lines[0].Snapshot.UnitPrice != 12000 || *pos.Lines[0].Snapshot.UnitCost != 7000 {
		t.Fatalf("pos = %+v", pos.Lines[0])
	}

	if _, err := e.svc.UpdateCategory(e.ctx, e.owner, cat.ID, &CategoryPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	pos = e.quote(outlet, ChannelPOS, line)
	if !slices.Equal(kinds(pos.Lines[0].Problems), []ProblemKind{ProblemProductUnavailable}) {
		t.Errorf("archived category problems = %v", kinds(pos.Lines[0].Problems))
	}
	d, _ := e.svc.GetProduct(e.ctx, e.owner, p.ID)
	if !d.IsActive {
		t.Error("archiving the category archived the product")
	}

	_, err := Quote(e.ctx, testDB.Pool, QuoteRequest{OutletID: e.outlet("Asia/Jakarta", false), Channel: ChannelPOS,
		At: time.Now(), Lines: []QuoteLine{line}})
	if err != ErrOutletUnavailable {
		t.Errorf("inactive outlet err = %v", err)
	}
	_, err = Quote(e.ctx, testDB.Pool, QuoteRequest{OutletID: uuid.New(), Channel: ChannelPOS, At: time.Now()})
	if err != ErrOutletUnavailable {
		t.Errorf("unknown outlet err = %v", err)
	}
}

// AC-13 end to end, and E-15: each outlet evaluates the window in its own
// timezone.
func TestQuotePricingAndWindow(t *testing.T) {
	e := newEnv(t)
	p := e.product(e.category("Price").ID, "Kopi", 20000)
	large, err := e.svc.CreateVariant(e.ctx, e.owner, p.ID, &VariantInput{Name: "Large", PriceDelta: 5000})
	if err != nil {
		t.Fatal(err)
	}
	g := e.group("Extras", 0, 2, "Shot", "Syrup", "Boba") // deltas 0, 1000, 2000
	if _, err := e.svc.SetProductGroups(e.ctx, e.owner, p.ID, &AttachInput{ModifierGroupIDs: []string{g.ID}}); err != nil {
		t.Fatal(err)
	}
	from, to := "07:00", "11:00"
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{
		AvailableFrom: Nullable[string]{Set: true, Valid: true, V: from},
		AvailableTo:   Nullable[string]{Set: true, Valid: true, V: to},
	}); err != nil {
		t.Fatal(err)
	}
	line := QuoteLine{ProductID: uuid.MustParse(p.ID), VariantID: ptr(uuid.MustParse(large.ID)),
		ModifierIDs: []uuid.UUID{uuid.MustParse(g.Modifiers[1].ID), uuid.MustParse(g.Modifiers[2].ID)}}

	jakarta, makassar := e.outlet("Asia/Jakarta", true), e.outlet("Asia/Makassar", true)
	at := time.Date(2026, 10, 29, 3, 30, 0, 0, time.UTC) // 10:30 WIB, 11:30 WITA
	res, err := Quote(e.ctx, testDB.Pool, QuoteRequest{OutletID: jakarta, Channel: ChannelWeb, At: at, Lines: []QuoteLine{line}})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Lines[0].Snapshot
	if s == nil || s.UnitPrice != 20000+5000+1000+2000 || *s.VariantName != "Large" || s.ProductName != p.Name {
		t.Fatalf("jakarta line = %+v", res.Lines[0])
	}
	res, err = Quote(e.ctx, testDB.Pool, QuoteRequest{OutletID: makassar, Channel: ChannelWeb, At: at, Lines: []QuoteLine{line}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kinds(res.Lines[0].Problems), []ProblemKind{ProblemOutsideWindow}) {
		t.Errorf("makassar problems = %v", kinds(res.Lines[0].Problems))
	}
}

// AC-16, E-16: Quote inside a transaction sees its uncommitted price.
func TestQuoteInTransaction(t *testing.T) {
	e := newEnv(t)
	p := e.product(e.category("Tx").ID, "Kopi", 20000)
	outlet := e.outlet("Asia/Jakarta", true)
	errDone := errorString("rollback")
	err := testDB.InTx(e.ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `UPDATE products SET base_price = 23000 WHERE id = $1`, p.ID); err != nil {
			return err
		}
		res, err := Quote(e.ctx, tx, QuoteRequest{OutletID: outlet, Channel: ChannelPOS, At: time.Now(),
			Lines: []QuoteLine{{ProductID: uuid.MustParse(p.ID)}}})
		if err != nil {
			return err
		}
		if got := res.Lines[0].Snapshot.UnitPrice; got != 23000 {
			t.Errorf("unit price in tx = %d, want 23000", got)
		}
		return errDone
	})
	if err != errDone {
		t.Fatal(err)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// AC-9: cost_price present for an owner, absent (not null) otherwise, on
// list and detail.
func TestCostVisibility(t *testing.T) {
	e := newEnv(t)
	cat := e.category("Cost")
	p := e.product(cat.ID, "Kopi", 20000)
	callers := map[string]*domain.StaffAccess{
		"owner":      e.owner,
		"manager":    outletAccess(domain.RoleManager, "staff.manage"),
		"supervisor": outletAccess(domain.RoleSupervisor, "shift.open"),
		"cashier":    outletAccess(domain.RoleCashier, "order.create"),
	}
	for role, actor := range callers {
		d, err := e.svc.GetProduct(e.ctx, actor, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		list, err := e.svc.ListProducts(e.ctx, actor, ProductFilter{CategoryID: &cat.ID})
		if err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]any{"detail": d, "list": list[0]} {
			b, _ := json.Marshal(v)
			has := strings.Contains(string(b), `"cost_price"`)
			if has != (role == "owner") {
				t.Errorf("%s %s: cost_price present = %v: %s", role, name, has, b)
			}
		}
	}
	manager := callers["manager"]
	cost := int64(1)
	_, err := e.svc.UpdateProduct(e.ctx, manager, p.ID, &ProductPatch{CostPrice: Nullable[int64]{Set: true, Valid: true, V: cost}})
	wantCode(t, err, domain.CodeForbidden)
}

// AC-18, AC-19.
func TestPriceAudit(t *testing.T) {
	e := newEnv(t)
	p := e.product(e.category("Audit").ID, "Kopi", 20000)
	price := int64(22000)
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{BasePrice: &price}); err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, p.ID)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	r := rows[1]
	if r.Action != "product.updated" || r.Delta == nil || *r.Delta != 2000 {
		t.Errorf("row = %+v", r)
	}
	if len(r.Before) != 1 || len(r.After) != 1 || r.Before["base_price"] != float64(20000) || r.After["base_price"] != float64(22000) {
		t.Errorf("before/after = %v / %v", r.Before, r.After)
	}
	if rows[0].Delta != nil {
		t.Errorf("created row has value_delta %d", *rows[0].Delta)
	}

	// AC-19: same values again, and an empty patch, write nothing.
	name := " " + p.Name + " "
	for _, patch := range []*ProductPatch{{BasePrice: &price, Name: &name}, {}} {
		if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, patch); err != nil {
			t.Fatal(err)
		}
	}
	// A failed write leaves no row.
	bad := int64(-5)
	_, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{BasePrice: &bad})
	wantCode(t, err, domain.CodeInvalidInput)
	other := e.product(e.category("Audit2").ID, "Teh", 1000)
	_, err = e.svc.UpdateProduct(e.ctx, e.owner, other.ID, &ProductPatch{CategoryID: &p.CategoryID, Name: &p.Name, BasePrice: &price})
	wantCode(t, err, domain.CodeNameTaken)
	if n := len(auditRows(t, p.ID)); n != 2 {
		t.Errorf("audit rows after no-ops = %d, want 2", n)
	}
	if n := len(auditRows(t, other.ID)); n != 1 {
		t.Errorf("failed write audited: %d rows", n)
	}
}

// Window validation on write (E-14).
func TestWindowValidation(t *testing.T) {
	e := newEnv(t)
	cat := e.category("Window")
	price := int64(1000)
	from, same := "07:00", "07:00"
	for _, in := range []ProductInput{
		{AvailableFrom: &from},
		{AvailableFrom: &from, AvailableTo: &same},
		{AvailableFrom: ptr("7:00"), AvailableTo: ptr("11:00")},
	} {
		in.CategoryID, in.Name, in.BasePrice = cat.ID, e.name("W"), &price
		_, err := e.svc.CreateProduct(e.ctx, e.owner, &in)
		wantCode(t, err, domain.CodeInvalidInput)
	}
	p, err := e.svc.CreateProduct(e.ctx, e.owner, &ProductInput{CategoryID: cat.ID, Name: e.name("Night"),
		BasePrice: &price, AvailableFrom: ptr("22:00"), AvailableTo: ptr("02:00")})
	if err != nil {
		t.Fatalf("wrapping window: %v", err)
	}
	// Clearing one end of an existing window is rejected; clearing both works.
	_, err = e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{AvailableTo: Nullable[string]{Set: true}})
	wantCode(t, err, domain.CodeInvalidInput)
	got, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{
		AvailableFrom: Nullable[string]{Set: true}, AvailableTo: Nullable[string]{Set: true}})
	if err != nil || got.AvailableFrom != nil || got.AvailableTo != nil {
		t.Fatalf("clear window: %v %+v", err, got)
	}
}

// NFR-PERF-1: a 20-line quote, logged for a rough p95 read.
func TestQuoteLatency(t *testing.T) {
	e := newEnv(t)
	cat := e.category("Perf")
	outlet := e.outlet("Asia/Jakarta", true)
	g := e.group("Temp", 1, 1, "Hot", "Iced")
	lines := make([]QuoteLine, 20)
	for i := range lines {
		p := e.product(cat.ID, "P"+uuid.NewString()[:4], 10000)
		if _, err := e.svc.SetProductGroups(e.ctx, e.owner, p.ID, &AttachInput{ModifierGroupIDs: []string{g.ID}}); err != nil {
			t.Fatal(err)
		}
		lines[i] = QuoteLine{ProductID: uuid.MustParse(p.ID), ModifierIDs: []uuid.UUID{uuid.MustParse(g.Modifiers[0].ID)}}
	}
	durations := make([]time.Duration, 50)
	for i := range durations {
		start := time.Now()
		res := e.quote(outlet, ChannelPOS, lines...)
		durations[i] = time.Since(start)
		if !res.Valid() {
			t.Fatalf("invalid quote: %+v", res.Lines[0].Problems)
		}
	}
	slices.Sort(durations)
	t.Logf("20-line quote p50 %v p95 %v", durations[25], durations[47])
}

// A product restore racing an archive of its required group's last
// modifier: the restore waits on the group lock, then sees the archive and
// is refused, instead of both committing.
func TestRestoreRacesModifierArchive(t *testing.T) {
	e := newEnv(t)
	p := e.product(e.category("Race").ID, "Kopi", 20000)
	g := e.group("Temp", 1, 1, "Hot")
	if _, err := e.svc.SetProductGroups(e.ctx, e.owner, p.ID, &AttachInput{ModifierGroupIDs: []string{g.ID}}); err != nil {
		t.Fatal(err)
	}
	off, on := false, true
	if _, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}

	tx, err := testDB.Pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, lockGroup, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `UPDATE modifiers SET is_active = FALSE WHERE id = $1`, g.Modifiers[0].ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := e.svc.UpdateProduct(e.ctx, e.owner, p.ID, &ProductPatch{IsActive: &on})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("restore did not wait for the group lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	wantCode(t, <-done, domain.CodeModifierGroupUnsatisfiable)
}
