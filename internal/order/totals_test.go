package order

import (
	"slices"
	"testing"
)

// AC-6: totals and fulfillment summary come from active lines only.
func TestComputeTotals(t *testing.T) {
	active := func(total int64, f string) totalsLine { return totalsLine{total, f, LineActive} }
	voided := func(total int64, f string) totalsLine { return totalsLine{total, f, LineVoided} }
	for _, tt := range []struct {
		name    string
		lines   []totalsLine
		total   int64
		summary string
	}{
		{"all dine-in", []totalsLine{active(10000, FulfillmentDineIn), active(25000, FulfillmentDineIn)}, 35000, FulfillmentDineIn},
		{"all takeaway", []totalsLine{active(18000, FulfillmentTakeaway)}, 18000, FulfillmentTakeaway},
		{"mixed", []totalsLine{active(10000, FulfillmentDineIn), active(5000, FulfillmentTakeaway)}, 15000, FulfillmentMixed},
		{"voided takeaway excluded", []totalsLine{active(10000, FulfillmentDineIn), voided(5000, FulfillmentTakeaway)}, 10000, FulfillmentDineIn},
		{"voided dine-in excluded", []totalsLine{voided(10000, FulfillmentDineIn), active(7000, FulfillmentTakeaway),
			active(3000, FulfillmentTakeaway)}, 10000, FulfillmentTakeaway},
		{"zero total", []totalsLine{active(0, FulfillmentDineIn)}, 0, FulfillmentDineIn},
	} {
		got := computeTotals(tt.lines)
		if got.Subtotal != tt.total || got.GrandTotal != tt.total || got.FulfillmentSummary != tt.summary ||
			got.DiscountTotal != 0 || got.ServiceCharge != 0 || got.PackagingFee != 0 || got.TaxTotal != 0 {
			t.Errorf("%s: %+v", tt.name, got)
		}
	}
}

func TestLineTotal(t *testing.T) {
	if got := lineTotal(27000, 2, 0); got != 54000 {
		t.Fatalf("line total = %d", got)
	}
	if got := lineTotal(27000, 2, 4000); got != 50000 {
		t.Fatalf("line total with discount = %d", got)
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
