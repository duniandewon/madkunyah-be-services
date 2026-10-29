package catalog

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fixture: a coffee with sizes, a required temperature group (1..1) and an
// optional extras group (0..2), plus a breakfast item with a window.
type fixture struct {
	data                             *quoteData
	coffee, breakfast, plain         uuid.UUID
	regular, large, retired, other   uuid.UUID
	temp, extras, archivedGroup      uuid.UUID
	hot, iced, shot, syrup, oldExtra uuid.UUID
	foreign, fromArchived            uuid.UUID
	loc                              *time.Location
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	loc, err := location("Asia/Jakarta")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{loc: loc}
	ids := []*uuid.UUID{&f.coffee, &f.breakfast, &f.plain, &f.regular, &f.large, &f.retired, &f.other,
		&f.temp, &f.extras, &f.archivedGroup, &f.hot, &f.iced, &f.shot, &f.syrup, &f.oldExtra,
		&f.foreign, &f.fromArchived}
	for _, id := range ids {
		*id = uuid.New()
	}
	cat := uuid.New()
	cost := int64(8000)
	product := func(id uuid.UUID, name string, price int64) *quoteProduct {
		return &quoteProduct{id: id, categoryID: cat, name: name, categoryName: "Drinks",
			basePrice: price, active: true, catActive: true, webVisible: true, posVisible: true}
	}
	f.data = &quoteData{
		products: map[uuid.UUID]*quoteProduct{
			f.coffee:    product(f.coffee, "Kopi Susu", 20000),
			f.breakfast: product(f.breakfast, "Nasi Uduk", 15000),
			f.plain:     product(f.plain, "Air Mineral", 5000),
		},
		variants: map[uuid.UUID][]quoteVariant{
			f.coffee: {
				{id: f.regular, productID: f.coffee, name: "Regular", active: true},
				{id: f.large, productID: f.coffee, name: "Large", priceDelta: 5000, active: true},
				{id: f.retired, productID: f.coffee, name: "Jumbo", priceDelta: 9000},
			},
			f.breakfast: {{id: f.other, productID: f.breakfast, name: "Porsi", active: true}},
		},
		groups: map[uuid.UUID][]quoteGroup{
			f.coffee: {
				{id: f.temp, minSelect: 1, maxSelect: 1, active: true},
				{id: f.extras, minSelect: 0, maxSelect: 2, active: true},
				{id: f.archivedGroup, minSelect: 1, maxSelect: 1},
			},
		},
		modifiers: map[uuid.UUID]*quoteModifier{
			f.hot:          {id: f.hot, groupID: f.temp, name: "Hot", active: true},
			f.iced:         {id: f.iced, groupID: f.temp, name: "Iced", priceDelta: 1000, active: true},
			f.shot:         {id: f.shot, groupID: f.extras, name: "Extra Shot", priceDelta: 4000, active: true},
			f.syrup:        {id: f.syrup, groupID: f.extras, name: "Syrup", priceDelta: 3000, active: true},
			f.oldExtra:     {id: f.oldExtra, groupID: f.extras, name: "Boba", priceDelta: 5000},
			f.foreign:      {id: f.foreign, groupID: uuid.New(), name: "Sambal", active: true},
			f.fromArchived: {id: f.fromArchived, groupID: f.archivedGroup, name: "Old", active: true},
		},
	}
	f.data.products[f.coffee].costPrice = &cost
	f.data.products[f.breakfast].window = &window{from: 7 * 60, to: 11 * 60}
	return f
}

func (f *fixture) at(h, m int) time.Time {
	return time.Date(2026, 10, 29, h, m, 0, 0, f.loc)
}

func ptr[T any](v T) *T { return &v }

func kinds(ps []Problem) []ProblemKind {
	out := make([]ProblemKind, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Kind)
	}
	return out
}

// AC-12, E-17, E-18: each problem kind from exactly one matching input.
func TestResolveProblems(t *testing.T) {
	type lineFn func(f *fixture) QuoteLine
	coffee := func(variant func(f *fixture) *uuid.UUID, mods ...func(f *fixture) uuid.UUID) lineFn {
		return func(f *fixture) QuoteLine {
			l := QuoteLine{ProductID: f.coffee, VariantID: variant(f)}
			for _, m := range mods {
				l.ModifierIDs = append(l.ModifierIDs, m(f))
			}
			return l
		}
	}
	regular := func(f *fixture) *uuid.UUID { return ptr(f.regular) }
	hot := func(f *fixture) uuid.UUID { return f.hot }
	valid := coffee(regular, hot)

	tests := []struct {
		name   string
		line   lineFn
		ch     Channel
		at     func(f *fixture) time.Time
		mutate func(f *fixture)
		want   []ProblemKind
	}{
		{name: "valid", line: valid},
		{name: "valid without variants", line: func(f *fixture) QuoteLine { return QuoteLine{ProductID: f.plain} }},
		{name: "unknown product", line: func(*fixture) QuoteLine { return QuoteLine{ProductID: uuid.New()} },
			want: []ProblemKind{ProblemProductUnavailable}},
		{name: "archived product", line: valid,
			mutate: func(f *fixture) { f.data.products[f.coffee].active = false },
			want:   []ProblemKind{ProblemProductUnavailable}},
		{name: "archived category", line: valid,
			mutate: func(f *fixture) { f.data.products[f.coffee].catActive = false },
			want:   []ProblemKind{ProblemProductUnavailable}},
		{name: "hidden on web", line: valid, ch: ChannelWeb,
			mutate: func(f *fixture) { f.data.products[f.coffee].webVisible = false },
			want:   []ProblemKind{ProblemProductUnavailable}},
		{name: "hidden on pos", line: valid, ch: ChannelPOS,
			mutate: func(f *fixture) { f.data.products[f.coffee].posVisible = false },
			want:   []ProblemKind{ProblemProductUnavailable}},
		{name: "outside window",
			line: func(f *fixture) QuoteLine { return QuoteLine{ProductID: f.breakfast, VariantID: ptr(f.other)} },
			at:   func(f *fixture) time.Time { return f.at(11, 0) },
			want: []ProblemKind{ProblemOutsideWindow}},
		{name: "inside window",
			line: func(f *fixture) QuoteLine { return QuoteLine{ProductID: f.breakfast, VariantID: ptr(f.other)} },
			at:   func(f *fixture) time.Time { return f.at(10, 59) }},
		{name: "variant required", line: coffee(func(*fixture) *uuid.UUID { return nil }, hot),
			want: []ProblemKind{ProblemVariantRequired}},
		{name: "archived variant", line: coffee(func(f *fixture) *uuid.UUID { return ptr(f.retired) }, hot),
			want: []ProblemKind{ProblemVariantInvalid}},
		{name: "other product's variant", line: coffee(func(f *fixture) *uuid.UUID { return ptr(f.other) }, hot),
			want: []ProblemKind{ProblemVariantInvalid}},
		{name: "variant on product without variants",
			line: func(f *fixture) QuoteLine { return QuoteLine{ProductID: f.plain, VariantID: ptr(f.regular)} },
			want: []ProblemKind{ProblemVariantInvalid}},
		{name: "duplicate modifier",
			line: coffee(regular, hot, (func(f *fixture) uuid.UUID { return f.shot }),
				(func(f *fixture) uuid.UUID { return f.shot })),
			want: []ProblemKind{ProblemModifierInvalid}},
		{name: "unknown modifier", line: coffee(regular, hot, (func(*fixture) uuid.UUID { return uuid.New() })),
			want: []ProblemKind{ProblemModifierInvalid}},
		{name: "modifier from unattached group",
			line: coffee(regular, hot, (func(f *fixture) uuid.UUID { return f.foreign })),
			want: []ProblemKind{ProblemModifierInvalid}},
		{name: "archived modifier",
			line: coffee(regular, hot, (func(f *fixture) uuid.UUID { return f.oldExtra })),
			want: []ProblemKind{ProblemModifierInvalid}},
		{name: "modifier from archived group",
			line: coffee(regular, hot, (func(f *fixture) uuid.UUID { return f.fromArchived })),
			want: []ProblemKind{ProblemModifierInvalid}},
		{name: "required group missing", line: coffee(regular),
			want: []ProblemKind{ProblemModifierCountInvalid}},
		{name: "group over max",
			line: coffee(regular, hot, (func(f *fixture) uuid.UUID { return f.iced })),
			want: []ProblemKind{ProblemModifierCountInvalid}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.mutate != nil {
				tt.mutate(f)
			}
			ch := tt.ch
			if ch == "" {
				ch = ChannelPOS
			}
			at := f.at(9, 0)
			if tt.at != nil {
				at = tt.at(f)
			}
			got := f.data.resolve(tt.line(f), ch, at, f.loc, Strict)
			if !slices.Equal(kinds(got.Problems), tt.want) {
				t.Fatalf("problems = %v, want %v", kinds(got.Problems), tt.want)
			}
			if (got.Snapshot == nil) != (len(tt.want) > 0) {
				t.Fatalf("strict snapshot present = %v with problems %v", got.Snapshot != nil, tt.want)
			}
		})
	}
}

// AC-13: base + variant delta + both modifier deltas, with catalog names.
func TestResolvePrice(t *testing.T) {
	f := newFixture(t)
	line := QuoteLine{ProductID: f.coffee, VariantID: ptr(f.large), ModifierIDs: []uuid.UUID{f.iced, f.shot}}
	got := f.data.resolve(line, ChannelWeb, f.at(9, 0), f.loc, Strict)
	if len(got.Problems) > 0 {
		t.Fatalf("problems: %v", kinds(got.Problems))
	}
	s := got.Snapshot
	if s.UnitPrice != 20000+5000+1000+4000 {
		t.Errorf("unit price = %d, want 30000", s.UnitPrice)
	}
	if s.BasePrice != 20000 || s.VariantDelta != 5000 || s.ModifiersDelta != 5000 {
		t.Errorf("breakdown = %d/%d/%d", s.BasePrice, s.VariantDelta, s.ModifiersDelta)
	}
	if s.ProductName != "Kopi Susu" || s.CategoryName != "Drinks" || s.VariantName == nil || *s.VariantName != "Large" {
		t.Errorf("names = %q %q %v", s.ProductName, s.CategoryName, s.VariantName)
	}
	if len(s.Modifiers) != 2 || s.Modifiers[0].Name != "Iced" || s.Modifiers[1].Name != "Extra Shot" {
		t.Errorf("modifiers = %+v", s.Modifiers)
	}
	if s.UnitCost == nil || *s.UnitCost != 8000 {
		t.Errorf("unit cost = %v", s.UnitCost)
	}
}

// AC-14: the same invalid line fails in Strict and is snapshotted with its
// problems in Record.
func TestResolveModes(t *testing.T) {
	f := newFixture(t)
	f.data.products[f.coffee].posVisible = false
	line := QuoteLine{ProductID: f.coffee, VariantID: ptr(f.retired), ModifierIDs: []uuid.UUID{f.oldExtra}}

	strict := f.data.resolve(line, ChannelPOS, f.at(9, 0), f.loc, Strict)
	if strict.Snapshot != nil || len(strict.Problems) == 0 {
		t.Fatalf("strict = %+v", strict)
	}
	rec := f.data.resolve(line, ChannelPOS, f.at(9, 0), f.loc, Record)
	if rec.Snapshot == nil {
		t.Fatal("record mode returned no snapshot")
	}
	want := []ProblemKind{ProblemProductUnavailable, ProblemVariantInvalid, ProblemModifierInvalid, ProblemModifierCountInvalid}
	if !slices.Equal(kinds(rec.Problems), want) {
		t.Errorf("record problems = %v, want %v", kinds(rec.Problems), want)
	}
	// Priced from the current catalog, archived variant and modifier included.
	if rec.Snapshot.UnitPrice != 20000+9000+5000 {
		t.Errorf("record unit price = %d", rec.Snapshot.UnitPrice)
	}

	unknown := f.data.resolve(QuoteLine{ProductID: uuid.New()}, ChannelPOS, f.at(9, 0), f.loc, Record)
	if unknown.Snapshot != nil {
		t.Error("unknown product must not be snapshotted")
	}
}
