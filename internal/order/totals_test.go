package order

import (
	"slices"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

func active(price int64, qty int, f string) totalsLine {
	return totalsLine{UnitPrice: price, Quantity: qty, FulfillmentType: f, Status: LineActive}
}

// AC-6 (orders): totals and fulfillment summary come from active lines only.
func TestComputeTotals(t *testing.T) {
	voided := func(price int64, f string) totalsLine {
		return totalsLine{UnitPrice: price, Quantity: 1, FulfillmentType: f, Status: LineVoided}
	}
	for _, tt := range []struct {
		name    string
		lines   []totalsLine
		total   int64
		summary string
	}{
		{"all dine-in", []totalsLine{active(10000, 1, FulfillmentDineIn), active(25000, 1, FulfillmentDineIn)}, 35000, FulfillmentDineIn},
		{"all takeaway", []totalsLine{active(9000, 2, FulfillmentTakeaway)}, 18000, FulfillmentTakeaway},
		{"mixed", []totalsLine{active(10000, 1, FulfillmentDineIn), active(5000, 1, FulfillmentTakeaway)}, 15000, FulfillmentMixed},
		{"voided takeaway excluded", []totalsLine{active(10000, 1, FulfillmentDineIn), voided(5000, FulfillmentTakeaway)}, 10000, FulfillmentDineIn},
		{"voided dine-in excluded", []totalsLine{voided(10000, FulfillmentDineIn), active(7000, 1, FulfillmentTakeaway),
			active(3000, 1, FulfillmentTakeaway)}, 10000, FulfillmentTakeaway},
		{"zero total", []totalsLine{active(0, 1, FulfillmentDineIn)}, 0, FulfillmentDineIn},
	} {
		got, err := computeTotals(tt.lines, nil)
		if err != nil || got.Subtotal != tt.total || got.GrandTotal != tt.total || got.FulfillmentSummary != tt.summary ||
			got.DiscountTotal != 0 || got.ServiceCharge != 0 || got.PackagingFee != 0 || got.TaxTotal != 0 {
			t.Errorf("%s: %+v, %v", tt.name, got, err)
		}
	}
}

func pct(v int64) *discountRule   { return &discountRule{Type: DiscountPercent, Value: v} }
func fixed(v int64) *discountRule { return &discountRule{Type: DiscountFixed, Value: v} }

// withDiscount is a line with a line discount.
func withDiscount(l totalsLine, d *discountRule) totalsLine {
	l.Discount = d
	return l
}

// AC-1 (pos-controls FR-TOT-7..16): line and order discounts, rounding,
// overrides, allocation and clamping.
func TestComputeTotalsControls(t *testing.T) {
	dine := FulfillmentDineIn
	for _, tt := range []struct {
		name     string
		lines    []totalsLine
		order    *discountRule
		amounts  []lineAmounts
		subtotal int64
		discount int64
	}{
		{"E-1 10% off 27,000 × 2", []totalsLine{withDiscount(active(27000, 2, dine), pct(10))}, nil,
			[]lineAmounts{{5400, 48600, 0}}, 48600, 0},
		{"E-2 15% off 9,950 rounds half up", []totalsLine{withDiscount(active(9950, 1, dine), pct(15))}, nil,
			[]lineAmounts{{1493, 8457, 0}}, 8457, 0},
		{"fixed line discount", []totalsLine{withDiscount(active(27000, 1, dine), fixed(2000))}, nil,
			[]lineAmounts{{2000, 25000, 0}}, 25000, 0},
		{"fixed equal to gross", []totalsLine{withDiscount(active(27000, 1, dine), fixed(27000))}, nil,
			[]lineAmounts{{27000, 0, 0}}, 0, 0},
		{"E-3 100% off every line", []totalsLine{withDiscount(active(27000, 1, dine), pct(100)),
			withDiscount(active(10000, 3, dine), pct(100))}, nil,
			[]lineAmounts{{27000, 0, 0}, {30000, 0, 0}}, 0, 0},
		{"override down (unit price is the override)", []totalsLine{active(20000, 2, dine)}, nil,
			[]lineAmounts{{0, 40000, 0}}, 40000, 0},
		{"E-15 override to 0", []totalsLine{active(0, 2, dine)}, nil, []lineAmounts{{0, 0, 0}}, 0, 0},
		{"E-16 override with 10% discount", []totalsLine{withDiscount(active(20000, 1, dine), pct(10))}, nil,
			[]lineAmounts{{2000, 18000, 0}}, 18000, 0},
		{"percent order discount", []totalsLine{active(27000, 2, dine)}, pct(10),
			[]lineAmounts{{0, 54000, 5400}}, 54000, 5400},
		{"fixed order discount", []totalsLine{active(10000, 1, dine), active(30000, 1, dine)}, fixed(4000),
			[]lineAmounts{{0, 10000, 1000}, {0, 30000, 3000}}, 40000, 4000},
		{"E-9 remainders 10,000 / 10,000 / 10,001", []totalsLine{active(10000, 1, dine), active(10000, 1, dine),
			active(10001, 1, dine)}, pct(10),
			[]lineAmounts{{0, 10000, 1000}, {0, 10000, 1000}, {0, 10001, 1000}}, 30001, 3000},
		{"ties go to the lower line_no", []totalsLine{{LineNo: 2, UnitPrice: 100, Quantity: 1, Status: LineActive},
			{LineNo: 1, UnitPrice: 100, Quantity: 1, Status: LineActive}}, fixed(1),
			[]lineAmounts{{0, 100, 0}, {0, 100, 1}}, 200, 1},
		{"voided and zero lines get no share", []totalsLine{active(10000, 1, dine),
			{UnitPrice: 5000, Quantity: 1, Status: LineVoided}, active(0, 1, dine)}, pct(50),
			[]lineAmounts{{0, 10000, 5000}, {0, 5000, 0}, {0, 0, 0}}, 10000, 5000},
		{"line and order discounts together", []totalsLine{withDiscount(active(10000, 1, dine), fixed(2000)),
			active(2000, 1, dine)}, pct(10),
			[]lineAmounts{{2000, 8000, 800}, {0, 2000, 200}}, 10000, 1000},
		{"E-10 fixed order discount clamped to the subtotal", []totalsLine{active(10000, 1, dine),
			{UnitPrice: 20000, Quantity: 1, Status: LineVoided}, active(5000, 1, dine)}, fixed(20000),
			[]lineAmounts{{0, 10000, 10000}, {0, 20000, 0}, {0, 5000, 5000}}, 15000, 15000},
		{"order discount on a zero subtotal", []totalsLine{active(0, 1, dine)}, pct(10),
			[]lineAmounts{{0, 0, 0}}, 0, 0},
		{"large amounts do not overflow", []totalsLine{active(100_000_000, 999, dine), active(100_000_000, 999, dine)},
			pct(33), []lineAmounts{{0, 99_900_000_000, 32_967_000_000}, {0, 99_900_000_000, 32_967_000_000}},
			199_800_000_000, 65_934_000_000},
	} {
		got, err := computeTotals(tt.lines, tt.order)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if !slices.Equal(got.Lines, tt.amounts) || got.Subtotal != tt.subtotal || got.DiscountTotal != tt.discount ||
			got.GrandTotal != tt.subtotal-tt.discount {
			t.Errorf("%s: %+v", tt.name, got)
		}
		var shares int64
		for i, a := range got.Lines {
			shares += a.Share
			if a.Share > a.LineTotal || (tt.lines[i].Status != LineActive && a.Share != 0) {
				t.Errorf("%s: line %d share %d", tt.name, i, a.Share)
			}
		}
		if shares != got.DiscountTotal {
			t.Errorf("%s: shares %d, discount %d", tt.name, shares, got.DiscountTotal)
		}
	}
}

// E-4, FR-TOT-15: a fixed discount above the gross is refused, with the
// line and the most it could be.
func TestComputeTotalsExceeds(t *testing.T) {
	l := withDiscount(active(27000, 1, FulfillmentDineIn), fixed(30000))
	l.ID = "line-1"
	_, err := computeTotals([]totalsLine{active(1000, 1, FulfillmentDineIn), l}, nil)
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeDiscountExceedsTotal {
		t.Fatalf("err = %v", err)
	}
	if d := de.Details.(map[string]any); d["max"] != int64(27000) || d["line_id"] != "line-1" {
		t.Fatalf("details = %v", de.Details)
	}
	if err := checkOrderDiscount(fixed(30001), 30000); domain.AsError(err) == nil ||
		domain.AsError(err).Details.(map[string]any)["max"] != int64(30000) {
		t.Fatalf("order discount above subtotal: %v", err)
	}
	if err := checkOrderDiscount(fixed(30000), 30000); err != nil {
		t.Fatalf("order discount equal to subtotal: %v", err)
	}
	if err := checkOrderDiscount(pct(100), 0); err != nil {
		t.Fatalf("percent order discount: %v", err)
	}
}

// AC-20 (unit): exactly the four final statuses are finished; every other
// status the schema allows blocks a close.
func TestFinishedStatuses(t *testing.T) {
	want := map[string]bool{StatusCompleted: true, StatusCancelled: true, StatusExpired: true, StatusRefunded: true}
	for _, s := range allStatuses {
		if slices.Contains(finishedStatuses, s) != want[s] {
			t.Errorf("status %q: finished = %v", s, !want[s])
		}
	}
	for _, s := range finishedStatuses {
		if !slices.Contains(allStatuses, s) {
			t.Errorf("finished status %q is not a status", s)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c, ok := parseCursor(cursor{date: "2026-11-12", id: "0b4f6a0e-6a8b-4a8e-9a43-1d1a5c0e2f11"}.String())
	if !ok || c.date != "2026-11-12" || c.id != "0b4f6a0e-6a8b-4a8e-9a43-1d1a5c0e2f11" {
		t.Fatalf("cursor = %+v, %v", c, ok)
	}
	for _, bad := range []string{"x", "YQ", cursor{date: "bad", id: "x"}.String()} {
		if _, ok := parseCursor(bad); ok {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}
