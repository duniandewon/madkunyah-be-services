package order

import (
	"context"
	"fmt"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/jackc/pgx/v5"
)

const (
	// One statement per control sets the control and the line's amounts
	// together, so the all-or-none and share CHECKs hold after it.
	setLineDiscount = `
		UPDATE order_lines
		SET discount_type = $2, discount_value = $3, discount_reason = $4, discounted_by = $5,
		    discount_approved_by = $6, discounted_at = $7,
		    discount_amount = $8, line_total = $9, order_discount_share = $10
		WHERE id = $1`

	setLinePrice = `
		UPDATE order_lines
		SET unit_price = $2, original_unit_price = $3, price_override_reason = $4, price_overridden_by = $5,
		    price_override_approved_by = $6, price_overridden_at = $7,
		    discount_amount = $8, line_total = $9, order_discount_share = $10
		WHERE id = $1`

	setOrderDiscount = `
		UPDATE orders
		SET discount_type = $2, discount_value = $3, discount_reason = $4, discounted_by = $5,
		    discount_approved_by = $6, discounted_at = $7, discount_total = $8
		WHERE id = $1`
)

// controlWrite is one control route's change (FR-OPN-1).
type controlWrite struct {
	write
	in   *ControlInput
	kind controlKind
	// approver and stepUp are the step-up outcome, checked before the
	// transaction; stepUp is returned only when the request changes
	// something (FR-STP-6).
	approver string
	stepUp   error
}

// Control applies, replaces or removes one control on an open bill, then
// recomputes and stores the totals (FR-OPN-2). A request that would leave
// the control as it is changes nothing (FR-OPN-4).
func (r *Repository) Control(ctx context.Context, w *controlWrite) (*Detail, bool, error) {
	in := w.in
	return r.change(ctx, in.OrderID, in.OutletID, func(tx pgx.Tx, o *lockedOrder) (bool, error) {
		if o.status != StatusPlaced {
			return false, notOpen(o.status)
		}
		lines, err := loadStoredLines(ctx, tx, in.OrderID)
		if err != nil {
			return false, err
		}
		c := &controlChange{w: w, o: o, lines: lines, discount: o.discount, target: -1}
		if in.LineID != "" {
			if err := c.findTarget(); err != nil {
				return false, err
			}
		}
		if !c.plan() {
			return false, nil
		}
		if w.stepUp != nil {
			return false, w.stepUp
		}
		return true, c.apply(ctx, tx)
	})
}

// controlChange is one control change in progress: the order as locked,
// its lines, and the change planned on them in memory.
type controlChange struct {
	w        *controlWrite
	o        *lockedOrder
	lines    []storedLine
	discount *discountRule // the order discount after the change
	target   int           // index of the line changed, or -1
	audit    controlAudit
}

// findTarget finds the route's line: not_found when it is not on this
// order (orders E-21), line_not_active when voided (FR-OPN-5).
func (c *controlChange) findTarget() error {
	for i, l := range c.lines {
		if l.ID != c.w.in.LineID {
			continue
		}
		if l.Status != LineActive {
			return domain.NewError(domain.CodeLineNotActive, "This line has been voided.")
		}
		c.target = i
		return nil
	}
	return domain.ErrNotFound
}

// plan changes the in-memory state and reports whether anything changed.
func (c *controlChange) plan() bool {
	switch c.w.kind {
	case controlOrderDiscount:
		return c.planOrderDiscount()
	case controlLineDiscount:
		return c.planLineDiscount()
	default:
		return c.planPrice()
	}
}

// planOrderDiscount: the same type and value is a no-op, whatever the
// reason (FR-OPN-4).
func (c *controlChange) planOrderDiscount() bool {
	old, set := c.o.discount, c.w.in.Discount
	switch {
	case set == nil && old == nil:
		return false
	case set == nil:
		c.audit = controlAudit{action: "order.discount_removed", before: orderDiscountState(old, c.o.discountTotal)}
		c.discount = nil
	case old != nil && *old == *set.rule():
		return false
	default:
		c.audit = controlAudit{action: "order.discount", reason: &set.Reason}
		if old != nil {
			c.audit.before = orderDiscountState(old, c.o.discountTotal)
		}
		c.discount = set.rule()
	}
	return true
}

func (c *controlChange) planLineDiscount() bool {
	l := &c.lines[c.target]
	old, set := l.Discount, c.w.in.Discount
	switch {
	case set == nil && old == nil:
		return false
	case set == nil:
		c.audit = controlAudit{action: "line.discount_removed",
			before: lineDiscountState(l.ID, old, l.stored.DiscountAmount)}
		l.Discount = nil
	case old != nil && *old == *set.rule():
		return false
	default:
		c.audit = controlAudit{action: "line.discount", reason: &set.Reason}
		if old != nil {
			c.audit.before = lineDiscountState(l.ID, old, l.stored.DiscountAmount)
		}
		l.Discount = set.rule()
	}
	return true
}

// planPrice: an override to the quoted price removes the override
// (FR-OPN-6).
func (c *controlChange) planPrice() bool {
	l := &c.lines[c.target]
	quoted := l.UnitPrice
	if l.original != nil {
		quoted = *l.original
	}
	set := c.w.in.Override
	switch {
	case (set == nil || set.UnitPrice == quoted) && l.original == nil:
		return false
	case set == nil || set.UnitPrice == quoted:
		c.audit = controlAudit{action: "price.override_removed",
			before: overrideState(l.ID, l.UnitPrice, quoted, l.Quantity)}
		l.UnitPrice, l.original = quoted, nil
	case l.original != nil && set.UnitPrice == l.UnitPrice:
		return false
	default:
		c.audit = controlAudit{action: "price.override", reason: &set.Reason}
		if l.original != nil {
			c.audit.before = overrideState(l.ID, l.UnitPrice, quoted, l.Quantity)
		}
		l.UnitPrice, l.original = set.UnitPrice, &quoted
	}
	return true
}

// apply recomputes with the planned state and writes the control, the
// totals, the audit row and the outbox event (FR-OPN-2, FR-OPN-7).
func (c *controlChange) apply(ctx context.Context, tx pgx.Tx) error {
	t, err := computeTotals(totalsOf(c.lines), c.discount)
	if err != nil {
		return err
	}
	if c.audit.action == "order.discount" {
		if err := checkOrderDiscount(c.discount, t.Subtotal); err != nil {
			return err
		}
	}
	if err := c.storeControl(ctx, tx, &t); err != nil {
		return err
	}
	if err := storeTotals(ctx, tx, c.w.in.OrderID, c.lines, &t, c.w.at); err != nil {
		return err
	}
	c.audit.after = c.afterState(&t)
	c.audit.valueDelta = t.GrandTotal - c.o.grandTotal
	c.audit.approver = c.w.approver
	in := c.w.in
	if err := writeAudits(ctx, tx, in.OutletID, in.OrderID, []controlAudit{c.audit}, &c.w.write); err != nil {
		return err
	}
	lineIDs := []string{}
	if c.target >= 0 {
		lineIDs = []string{in.LineID}
	}
	return outboxAfter(ctx, tx, "order.updated", in.OrderID, in.OutletID, lineIDs, &c.w.write)
}

// storeControl writes the changed control with the target line's amounts.
func (c *controlChange) storeControl(ctx context.Context, tx pgx.Tx, t *Totals) error {
	w := c.w
	// Who, why and when are set when a control is applied, null on removal.
	var by, approver, reason *string
	var at *time.Time
	if c.audit.reason != nil {
		by, approver, at, reason = &w.actorID, &w.approver, &w.at, c.audit.reason
	}
	if c.target < 0 {
		dType, dValue := ruleColumns(c.discount)
		if _, err := tx.Exec(ctx, setOrderDiscount, w.in.OrderID, dType, dValue, reason, by, approver, at,
			t.DiscountTotal); err != nil {
			return fmt.Errorf("set order discount: %w", err)
		}
		return nil
	}
	l, a := &c.lines[c.target], t.Lines[c.target]
	var err error
	if w.kind == controlLineDiscount {
		dType, dValue := ruleColumns(l.Discount)
		_, err = tx.Exec(ctx, setLineDiscount, l.ID, dType, dValue, reason, by, approver, at,
			a.DiscountAmount, a.LineTotal, a.Share)
	} else {
		_, err = tx.Exec(ctx, setLinePrice, l.ID, l.UnitPrice, l.original, reason, by, approver, at,
			a.DiscountAmount, a.LineTotal, a.Share)
	}
	if err != nil {
		return fmt.Errorf("set line control: %w", err)
	}
	// The line is stored; storeTotals need not write it again.
	l.stored = a
	return nil
}

// afterState is the audit's after for an applied control; nil on removal.
func (c *controlChange) afterState(t *Totals) map[string]any {
	if c.audit.reason == nil {
		return nil
	}
	number := c.o.orderNumber
	if c.target < 0 {
		return withLabels(orderDiscountState(c.discount, t.DiscountTotal), number, "")
	}
	l := &c.lines[c.target]
	if c.w.kind == controlLineDiscount {
		return withLabels(lineDiscountState(l.ID, l.Discount, t.Lines[c.target].DiscountAmount), number, l.productName)
	}
	return withLabels(overrideState(l.ID, l.UnitPrice, *l.original, l.Quantity), number, l.productName)
}

// lineAudits are the audit rows of the controls sent with new lines, each
// measured on its own (FR-CAUD-1): an override against the quoted price, a
// line discount against the gross.
func lineAudits(priced []pricedLine, amounts []lineAmounts, orderNumber string) []controlAudit {
	var audits []controlAudit
	for i, p := range priced {
		if o := p.in.Override; o != nil {
			audits = append(audits, controlAudit{action: "price.override",
				valueDelta: (o.UnitPrice - p.snap.UnitPrice) * int64(p.in.Quantity), reason: &o.Reason,
				approver: o.approver,
				after: withLabels(overrideState(p.in.ID, o.UnitPrice, p.snap.UnitPrice, p.in.Quantity),
					orderNumber, p.snap.ProductName)})
		}
		if d := p.in.Discount; d != nil {
			audits = append(audits, controlAudit{action: "line.discount", valueDelta: -amounts[i].DiscountAmount,
				reason: &d.Reason, approver: d.approver,
				after: withLabels(lineDiscountState(p.in.ID, d.rule(), amounts[i].DiscountAmount),
					orderNumber, p.snap.ProductName)})
		}
	}
	return audits
}

func orderDiscountState(d *discountRule, amount int64) map[string]any {
	return map[string]any{"type": d.Type, "value": d.Value, "amount": amount}
}

func lineDiscountState(lineID string, d *discountRule, amount int64) map[string]any {
	return map[string]any{"line_id": lineID, "type": d.Type, "value": d.Value, "amount": amount}
}

func overrideState(lineID string, unitPrice, original int64, quantity int) map[string]any {
	return map[string]any{"line_id": lineID, "unit_price": unitPrice, "original_unit_price": original,
		"quantity": quantity}
}

// withLabels adds the order number, and the product name for a line, to an
// audit's after.
func withLabels(m map[string]any, orderNumber, productName string) map[string]any {
	m["order_number"] = orderNumber
	if productName != "" {
		m["product_name"] = productName
	}
	return m
}
