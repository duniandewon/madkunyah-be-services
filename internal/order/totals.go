package order

import (
	"cmp"
	"math/bits"
	"slices"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

// Discount types (FR-CTL-1).
const (
	DiscountPercent = "percent"
	DiscountFixed   = "fixed"
)

// discountRule is a discount as stored: its type and value. The rupiah
// amount is always computed from it (FR-CTL-5).
type discountRule struct {
	Type  string
	Value int64
}

// totalsLine is what the totals need from one line. UnitPrice is the price
// charged: the override when there is one (FR-TOT-7).
type totalsLine struct {
	ID              string
	LineNo          int
	UnitPrice       int64
	Quantity        int
	Discount        *discountRule
	FulfillmentType string
	Status          string
}

// lineAmounts are a line's computed money columns.
type lineAmounts struct {
	DiscountAmount int64
	LineTotal      int64
	// Share is the line's part of the order discount (FR-TOT-13).
	Share int64
}

// Totals are an order's stored money columns and fulfillment summary, and
// each line's amounts in the order the lines were given.
type Totals struct {
	Subtotal           int64
	DiscountTotal      int64
	ServiceCharge      int64
	PackagingFee       int64
	TaxTotal           int64
	GrandTotal         int64
	FulfillmentSummary string
	Lines              []lineAmounts
}

// computeTotals derives every line's amounts and the order's totals, the
// one calculation every write path uses (FR-TOT-7..16):
//
//	gross       = unit_price × quantity
//	line_total  = gross − line discount (percent rounds half up)
//	subtotal    = Σ line_total of active lines
//	discount    = order discount on the subtotal (fixed is clamped to it)
//	grand_total = subtotal − discount
//
// The order discount is allocated over active lines in proportion to their
// line_total. A fixed line discount above its gross is
// discount_exceeds_total, on voided lines too: their controls never change
// after the void, so they cannot exceed. Voided lines keep their amounts
// and get no share. An order with no active line has no summary; callers
// never store one.
func computeTotals(lines []totalsLine, orderDiscount *discountRule) (Totals, error) {
	t := Totals{Lines: make([]lineAmounts, len(lines))}
	dineIn, takeaway := false, false
	for i, l := range lines {
		gross := l.UnitPrice * int64(l.Quantity)
		amount := discountAmount(l.Discount, gross)
		if amount > gross {
			return Totals{}, discountExceeds(l.ID, gross)
		}
		t.Lines[i] = lineAmounts{DiscountAmount: amount, LineTotal: gross - amount}
		if l.Status != LineActive {
			continue
		}
		t.Subtotal += gross - amount
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
	t.DiscountTotal = min(discountAmount(orderDiscount, t.Subtotal), t.Subtotal)
	allocate(lines, &t)
	t.GrandTotal = t.Subtotal - t.DiscountTotal + t.ServiceCharge + t.PackagingFee + t.TaxTotal
	return t, nil
}

// discountAmount is the rupiah a discount takes off base: a percentage
// rounded half up, or the fixed value as it is (FR-TOT-8, FR-TOT-11).
func discountAmount(d *discountRule, base int64) int64 {
	switch {
	case d == nil:
		return 0
	case d.Type == DiscountPercent:
		return (base*d.Value + 50) / 100
	default:
		return d.Value
	}
}

// allocate spreads t.DiscountTotal over the active lines by largest
// remainder (FR-TOT-13): each line takes the floor of its proportional
// share, and the rupiah left over go one each to the largest remainders,
// ties to the lower line_no. The shares sum to the discount exactly.
func allocate(lines []totalsLine, t *Totals) {
	if t.DiscountTotal == 0 || t.Subtotal == 0 {
		return
	}
	type remainder struct {
		index int
		rem   uint64
	}
	var rems []remainder
	left := t.DiscountTotal
	for i, l := range lines {
		total := t.Lines[i].LineTotal
		if l.Status != LineActive || total == 0 {
			continue
		}
		// discount × line_total can pass 2^63 for large orders; the
		// quotient never exceeds line_total, so it fits.
		hi, lo := bits.Mul64(uint64(t.DiscountTotal), uint64(total))
		share, rem := bits.Div64(hi, lo, uint64(t.Subtotal))
		t.Lines[i].Share = int64(share)
		left -= int64(share)
		rems = append(rems, remainder{i, rem})
	}
	slices.SortStableFunc(rems, func(a, b remainder) int {
		if c := cmp.Compare(b.rem, a.rem); c != 0 {
			return c
		}
		return cmp.Compare(lines[a.index].LineNo, lines[b.index].LineNo)
	})
	for k := int64(0); k < left; k++ {
		t.Lines[rems[k].index].Share++
	}
}

// discountExceeds refuses a fixed discount above what it applies to, or an
// override below a fixed line discount (FR-TOT-8, FR-TOT-11, FR-TOT-15).
// lineID is empty for the order discount.
func discountExceeds(lineID string, maxAmount int64) error {
	details := map[string]any{"max": maxAmount}
	if lineID != "" {
		details["line_id"] = lineID
	}
	return &domain.Error{
		Code:    domain.CodeDiscountExceedsTotal,
		Message: "The discount is larger than the amount it applies to.",
		Details: details,
	}
}

// checkOrderDiscount refuses a fixed order discount above the subtotal when
// it is applied; later recomputes clamp it instead (FR-TOT-11).
func checkOrderDiscount(d *discountRule, subtotal int64) error {
	if d != nil && d.Type == DiscountFixed && d.Value > subtotal {
		return discountExceeds("", subtotal)
	}
	return nil
}

// finishedStatuses never block a shift close; every other status does
// (FR-GRD-1). This is the one definition of "unfinished".
var finishedStatuses = []string{StatusCompleted, StatusCancelled, StatusExpired, StatusRefunded}
