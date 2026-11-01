package catalog

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// AC-1: the FR-OUT-2 resolution matrix, both channels.
func TestResolveChannel(t *testing.T) {
	productWin := &window{from: 7 * 60, to: 11 * 60}
	outletWin := &window{from: 6 * 60, to: 10 * 60}
	base := productDefaults{active: true, catActive: true, webVisible: true, posVisible: false,
		basePrice: 20000, window: productWin}
	price := int64(23000)

	tests := []struct {
		name    string
		mutate  func(*productDefaults)
		o       *override
		ch      Channel
		visible bool
		price   int64
		window  *window
	}{
		{name: "no row, web flag on", ch: ChannelWeb, visible: true, price: 20000, window: productWin},
		{name: "no row, pos flag off", ch: ChannelPOS, visible: false, price: 20000, window: productWin},
		{name: "all-null row inherits", o: &override{}, ch: ChannelWeb, visible: true, price: 20000, window: productWin},
		{name: "row hides on web", o: &override{visible: new(false)}, ch: ChannelWeb, visible: false, price: 20000,
			window: productWin},
		{name: "row shows on pos (E-1)", o: &override{visible: new(true)}, ch: ChannelPOS, visible: true,
			price: 20000, window: productWin},
		{name: "price only keeps flag (E-2)", o: &override{price: &price}, ch: ChannelPOS, visible: false,
			price: 23000, window: productWin},
		{name: "price override", o: &override{price: &price}, ch: ChannelWeb, visible: true, price: 23000,
			window: productWin},
		{name: "outlet window replaces", o: &override{window: outletWin}, ch: ChannelWeb, visible: true,
			price: 20000, window: outletWin},
		{name: "outlet window without product window", mutate: func(d *productDefaults) { d.window = nil },
			o: &override{window: outletWin}, ch: ChannelPOS, visible: false, price: 20000, window: outletWin},
		{name: "no window anywhere", mutate: func(d *productDefaults) { d.window = nil }, ch: ChannelWeb,
			visible: true, price: 20000},
		{name: "archived product beats row", mutate: func(d *productDefaults) { d.active = false },
			o: &override{visible: new(true)}, ch: ChannelWeb, visible: false, price: 20000, window: productWin},
		{name: "archived category beats row", mutate: func(d *productDefaults) { d.catActive = false },
			o: &override{visible: new(true)}, ch: ChannelPOS, visible: false, price: 20000, window: productWin},
		{name: "unknown channel", ch: Channel("kiosk"), visible: false, price: 20000, window: productWin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base
			if tt.mutate != nil {
				tt.mutate(&d)
			}
			got := resolveChannel(&d, tt.ch, tt.o)
			if got.visible != tt.visible || got.price != tt.price || got.window != tt.window {
				t.Errorf("got %+v, want visible %v price %d window %v", got, tt.visible, tt.price, tt.window)
			}
		})
	}
}

// AC-6, E-8, E-9: "today" ends at the next business-day cutoff, outlet time.
func TestNextCutoff(t *testing.T) {
	jkt, _ := location("Asia/Jakarta")
	wita, _ := location("Asia/Makassar")
	day := func(loc *time.Location, d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	tests := []struct {
		name string
		at   time.Time
		loc  *time.Location
		want time.Time
	}{
		{"afternoon, next day", day(jkt, 30, 14, 0), jkt, day(jkt, 31, 4, 0)},
		{"before cutoff, same day", day(jkt, 30, 2, 0), jkt, day(jkt, 30, 4, 0)},
		{"at cutoff, next day", day(jkt, 30, 4, 0), jkt, day(jkt, 31, 4, 0)},
		{"just before cutoff", day(jkt, 30, 3, 59), jkt, day(jkt, 30, 4, 0)},
		{"month end", day(jkt, 31, 23, 0), jkt, time.Date(2026, 11, 1, 4, 0, 0, 0, jkt)},
		{"outlet timezone, not server", day(time.UTC, 29, 19, 30), wita, day(wita, 30, 4, 0)},
	}
	for _, tt := range tests {
		if got := nextCutoff(tt.at, tt.loc, 4*60); !got.Equal(tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDecideSoldOut(t *testing.T) {
	jkt, _ := location("Asia/Jakarta")
	now := time.Date(2026, 10, 30, 14, 0, 0, 0, jkt)
	tomorrow := time.Date(2026, 10, 31, 4, 0, 0, 0, jkt).UTC()
	earlier := now.Add(-time.Hour)
	stored := func(sold bool, until *time.Time, at time.Time) *soldOutRow {
		return &soldOutRow{found: true, id: "r", isSoldOut: sold, until: until, changedAt: at}
	}
	tests := []struct {
		name    string
		cur     *soldOutRow
		sold    bool
		scope   string
		at      time.Time
		applied bool
		until   *time.Time
	}{
		{name: "first set today", cur: &soldOutRow{}, sold: true, scope: ScopeToday, at: now, applied: true,
			until: &tomorrow},
		{name: "first set indefinite", cur: &soldOutRow{}, sold: true, scope: ScopeIndefinite, at: now, applied: true},
		{name: "repeat today (E-14)", cur: stored(true, &tomorrow, earlier), sold: true, scope: ScopeToday, at: now},
		{name: "today to indefinite", cur: stored(true, &tomorrow, earlier), sold: true, scope: ScopeIndefinite,
			at: now, applied: true},
		{name: "repeat indefinite", cur: stored(true, nil, earlier), sold: true, scope: ScopeIndefinite, at: now},
		{name: "clear", cur: stored(true, nil, earlier), sold: false, at: now, applied: true},
		{name: "clear never set", cur: &soldOutRow{}, sold: false, at: now},
		{name: "clear already cleared", cur: stored(false, nil, earlier), sold: false, at: now},
		{name: "clear expired", cur: stored(true, &earlier, earlier.Add(-time.Hour)), sold: false, at: now},
		{name: "set again after expiry", cur: stored(true, &earlier, earlier.Add(-time.Hour)), sold: true,
			scope: ScopeToday, at: now, applied: true, until: &tomorrow},
		{name: "older toggle ignored (E-12)", cur: stored(false, nil, now), sold: true, scope: ScopeToday,
			at: earlier},
		{name: "same instant, new state applies", cur: stored(false, nil, now), sold: true,
			scope: ScopeIndefinite, at: now, applied: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &SoldOutWrite{SoldOut: tt.sold, Scope: tt.scope, ChangedAt: tt.at, ActorID: "s", Via: ViaPOS}
			next, applied := decideSoldOut(w, tt.cur, now, jkt, 4*60)
			if applied != tt.applied {
				t.Fatalf("applied = %v, want %v", applied, tt.applied)
			}
			if !applied {
				return
			}
			if next.id != tt.cur.id || next.isSoldOut != tt.sold || !sameTime(next.until, tt.until) {
				t.Errorf("next = %+v, want sold %v until %v", next, tt.sold, tt.until)
			}
		})
	}
}

func menuFixture() *Storefront {
	id := func(s string) string { return s }
	return &Storefront{
		Outlet: StoreOutlet{ID: "o", Name: "Outlet", Timezone: "Asia/Jakarta", BusinessDayCutoff: "04:00"},
		Categories: []StoreCategory{{ID: "c", Name: "Drinks", Products: []StoreProduct{
			{ID: id("latte"), Name: "Latte", Price: 25000,
				Variants: []StoreVariant{{ID: "regular"}, {ID: "large", PriceDelta: 5000}},
				ModifierGroups: []StoreGroup{
					{ID: "milk", Required: true, MinSelect: 1, MaxSelect: 1,
						Modifiers: []StoreModifier{{ID: "dairy"}, {ID: "oat", PriceDelta: 5000}}},
					{ID: "extras", MaxSelect: 2, Modifiers: []StoreModifier{{ID: "shot"}}},
				}},
			{ID: "breakfast", Name: "Nasi Uduk", Price: 15000, Window: &Window{From: "07:00", To: "11:00"},
				Variants: []StoreVariant{}, ModifierGroups: []StoreGroup{}},
		}}},
	}
}

// AC-8, E-10, E-11: sold-out variants and required modifiers make a
// product unorderable; an optional modifier does not.
func TestMenuOrderable(t *testing.T) {
	jkt, _ := location("Asia/Jakarta")
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, jkt)
	later := now.Add(time.Hour)
	expired := now.Add(-time.Minute)
	rule := func(kind, id string, until *time.Time) SoldOutRule {
		r := SoldOutRule{SoldOutUntil: until}
		switch kind {
		case "p":
			r.ProductID = &id
		case "v":
			r.VariantID = &id
		default:
			r.ModifierID = &id
		}
		return r
	}
	tests := []struct {
		name      string
		rules     []SoldOutRule
		at        time.Time
		orderable bool
		soldOut   bool
	}{
		{name: "nothing sold out", orderable: true},
		{name: "product sold out", rules: []SoldOutRule{rule("p", "latte", nil)}, soldOut: true},
		{name: "product sold out, expired", rules: []SoldOutRule{rule("p", "latte", &expired)}, orderable: true},
		{name: "product sold out until later", rules: []SoldOutRule{rule("p", "latte", &later)}, soldOut: true},
		{name: "one variant sold out", rules: []SoldOutRule{rule("v", "large", nil)}, orderable: true},
		{name: "all variants sold out (E-11)", rules: []SoldOutRule{rule("v", "large", nil), rule("v", "regular", nil)}},
		{name: "one required option left", rules: []SoldOutRule{rule("m", "oat", nil)}, orderable: true},
		{name: "required group emptied (E-10)", rules: []SoldOutRule{rule("m", "oat", nil), rule("m", "dairy", nil)}},
		{name: "optional modifier sold out", rules: []SoldOutRule{rule("m", "shot", nil)}, orderable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sf := menuFixture()
			sf.SoldOut = tt.rules
			m, err := sf.menu(now)
			if err != nil {
				t.Fatal(err)
			}
			p := m.Categories[0].Products[0]
			if p.Orderable != tt.orderable || p.SoldOut != tt.soldOut {
				t.Errorf("orderable %v sold_out %v, want %v %v", p.Orderable, p.SoldOut, tt.orderable, tt.soldOut)
			}
		})
	}
}

func TestMenuWindow(t *testing.T) {
	jkt, _ := location("Asia/Jakarta")
	for _, tt := range []struct {
		h, m   int
		within bool
	}{{6, 59, false}, {7, 0, true}, {10, 59, true}, {11, 0, false}} {
		m, err := menuFixture().menu(time.Date(2026, 10, 30, tt.h, tt.m, 0, 0, jkt))
		if err != nil {
			t.Fatal(err)
		}
		p := m.Categories[0].Products[1]
		if p.InWindow != tt.within || p.Orderable != tt.within || p.Window.From != "07:00" {
			t.Errorf("%02d:%02d: in_window %v orderable %v, want %v", tt.h, tt.m, p.InWindow, p.Orderable, tt.within)
		}
	}
}

// AC-18, FR-BND-3: the version depends on content only.
func TestStorefrontVersion(t *testing.T) {
	until := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC)
	sf := menuFixture()
	pid := "latte"
	sf.SoldOut = []SoldOutRule{{ProductID: &pid, SoldOutUntil: &until}}
	v1, _ := storefrontVersion(sf)
	sf.Version = v1
	again, _ := storefrontVersion(sf)
	if v1 != again || len(v1) != 32 {
		t.Fatalf("version not stable: %q vs %q", v1, again)
	}
	sf.Categories[0].Products[0].Price = 26000
	if v, _ := storefrontVersion(sf); v == v1 {
		t.Error("price change kept the version")
	}
}

func TestETagMatches(t *testing.T) {
	tag := `"abc"`
	for header, want := range map[string]bool{
		`"abc"`: true, `W/"abc"`: true, `"x", "abc"`: true, `*`: true, `"abd"`: false, ``: false, `abc`: false,
	} {
		if got := etagMatches(header, tag); got != want {
			t.Errorf("%q: got %v", header, got)
		}
	}
}

func TestSoldOutItemKind(t *testing.T) {
	id := uuid.NewString()
	bad := "nope"
	tests := []struct {
		item SoldOutItem
		ok   bool
	}{
		{SoldOutItem{ProductID: &id}, true},
		{SoldOutItem{VariantID: &id}, true},
		{SoldOutItem{ModifierID: &id}, true},
		{SoldOutItem{}, false},
		{SoldOutItem{ProductID: &id, VariantID: &id}, false}, // E-17
		{SoldOutItem{ProductID: &bad}, false},
	}
	for i, tt := range tests {
		if _, _, ok := tt.item.kind(); ok != tt.ok {
			t.Errorf("case %d: ok = %v", i, ok)
		}
	}
}

// AC-13 (unit): each sold-out problem from a matching input, none once the
// row has expired at At.
func TestResolveSoldOut(t *testing.T) {
	at := newFixture(t).at(9, 0)
	until := at.Add(time.Hour)
	tests := []struct {
		name   string
		mutate func(*fixture)
		at     time.Time
		want   []ProblemKind
	}{
		{name: "product", mutate: func(f *fixture) { f.data.products[f.coffee].soldOut = soldOut{set: true} },
			at: at, want: []ProblemKind{ProblemProductSoldOut}},
		{name: "variant", mutate: func(f *fixture) { f.data.variants[f.coffee][0].soldOut = soldOut{set: true} },
			at: at, want: []ProblemKind{ProblemVariantSoldOut}},
		{name: "modifier", mutate: func(f *fixture) { f.data.modifiers[f.shot].soldOut = soldOut{set: true} },
			at: at, want: []ProblemKind{ProblemModifierSoldOut}},
		{name: "required modifier, no count problem",
			mutate: func(f *fixture) { f.data.modifiers[f.hot].soldOut = soldOut{set: true} },
			at:     at, want: []ProblemKind{ProblemModifierSoldOut}},
		{name: "expired at At (E-18)", mutate: func(f *fixture) {
			f.data.products[f.coffee].soldOut = soldOut{set: true, until: &until}
		}, at: until},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.mutate(f)
			line := QuoteLine{ProductID: f.coffee, VariantID: ptr(f.regular), ModifierIDs: []uuid.UUID{f.hot, f.shot}}
			res := f.data.resolve(line, ChannelPOS, tt.at, f.loc, Record)
			if !slices.Equal(kinds(res.Problems), tt.want) {
				t.Errorf("problems = %v, want %v", kinds(res.Problems), tt.want)
			}
		})
	}
}
