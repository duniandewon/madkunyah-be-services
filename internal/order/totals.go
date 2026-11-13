package order

// totalsLine is what the totals need from one line.
type totalsLine struct {
	LineTotal       int64
	FulfillmentType string
	Status          string
}

// Totals are an order's stored money columns and fulfillment summary.
type Totals struct {
	Subtotal           int64
	DiscountTotal      int64
	ServiceCharge      int64
	PackagingFee       int64
	TaxTotal           int64
	GrandTotal         int64
	FulfillmentSummary string
}

// lineTotal is unit_price × quantity − discount_amount (FR-TOT-2).
func lineTotal(unitPrice int64, quantity int, discount int64) int64 {
	return unitPrice*int64(quantity) - discount
}

// computeTotals derives the order's totals and fulfillment summary from its
// active lines only (FR-TOT-3, FR-TOT-4). There are no discounts or charges
// at launch, so the grand total is the subtotal. An order with no active
// line has no summary; callers never store one.
func computeTotals(lines []totalsLine) Totals {
	var t Totals
	dineIn, takeaway := false, false
	for _, l := range lines {
		if l.Status != LineActive {
			continue
		}
		t.Subtotal += l.LineTotal
		switch l.FulfillmentType {
		case FulfillmentDineIn:
			dineIn = true
		case FulfillmentTakeaway:
			takeaway = true
		}
	}
	switch {
	case dineIn && takeaway:
		t.FulfillmentSummary = FulfillmentMixed
	case dineIn:
		t.FulfillmentSummary = FulfillmentDineIn
	case takeaway:
		t.FulfillmentSummary = FulfillmentTakeaway
	}
	t.GrandTotal = t.Subtotal - t.DiscountTotal + t.ServiceCharge + t.PackagingFee + t.TaxTotal
	return t
}

// finishedStatuses never block a shift close; every other status does
// (FR-GRD-1). This is the one definition of "unfinished".
var finishedStatuses = []string{StatusCompleted, StatusCancelled, StatusExpired, StatusRefunded}
