package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/shift"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	orderOutlet = `SELECT outlet_id::text FROM orders WHERE id = $1`
	// The counter prefix is the one the shift's sequences were created
	// with at open, so a later change to outlets.order_prefix cannot leave
	// the shift without a sequence. WEB is the counter prefix only when
	// the outlet's own prefix is WEB.
	shiftPrefix = `
		SELECT prefix FROM order_sequences
		WHERE shift_id = $1
		ORDER BY prefix = $2, prefix
		LIMIT 1`

	// The order discount, when there is one, is applied by the creator at
	// placement (pos-controls FR-PLA-1).
	insertOrder = `
		INSERT INTO orders (id, outlet_id, shift_id, device_id, order_prefix, order_sequence, order_number,
		                    channel, fulfillment_summary, status, table_number, note, subtotal, discount_total,
		                    grand_total, business_date, placed_at, created_by, created_at, updated_at,
		                    discount_type, discount_value, discount_reason, discounted_by, discount_approved_by,
		                    discounted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pos', $8, 'placed', $9, $10, $11, $12, $13, $14::date,
		        $15::timestamptz, $16::uuid, $15, $15, $17::text, $18, $19,
		        CASE WHEN $17::text IS NULL THEN NULL ELSE $16::uuid END, $20::uuid,
		        CASE WHEN $17::text IS NULL THEN NULL ELSE $15::timestamptz END)`

	// Lines and modifiers go in one statement each, whatever the count
	// (NFR-PERF-1). Nullable UUIDs travel as text.
	// A line's controls are applied by the staff member adding it.
	insertLines = `
		INSERT INTO order_lines (id, order_id, line_no, product_id, product_variant_id, product_name, variant_name,
		                         category_id, category_name, unit_price, unit_cost, quantity, fulfillment_type,
		                         line_total, note, created_by, created_at,
		                         discount_type, discount_value, discount_reason, discounted_by, discount_approved_by,
		                         discounted_at, discount_amount, order_discount_share,
		                         original_unit_price, price_override_reason, price_overridden_by,
		                         price_override_approved_by, price_overridden_at)
		SELECT l.id::uuid, $1, l.line_no, l.product_id::uuid, l.variant_id::uuid, l.product_name, l.variant_name,
		       l.category_id::uuid, l.category_name, l.unit_price, l.unit_cost, l.quantity, l.fulfillment_type,
		       l.line_total, l.note, $2::uuid, $3::timestamptz,
		       l.dtype, l.dvalue, l.dreason, CASE WHEN l.dtype IS NULL THEN NULL ELSE $2::uuid END,
		       l.dapprover::uuid, CASE WHEN l.dtype IS NULL THEN NULL ELSE $3::timestamptz END, l.damount, l.share,
		       l.original, l.oreason, CASE WHEN l.original IS NULL THEN NULL ELSE $2::uuid END,
		       l.oapprover::uuid, CASE WHEN l.original IS NULL THEN NULL ELSE $3::timestamptz END
		FROM unnest($4::text[], $5::int[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[],
		            $11::text[], $12::bigint[], $13::bigint[], $14::int[], $15::text[], $16::bigint[], $17::text[],
		            $18::text[], $19::bigint[], $20::text[], $21::text[], $22::bigint[], $23::bigint[],
		            $24::bigint[], $25::text[], $26::text[])
		  AS l(id, line_no, product_id, variant_id, product_name, variant_name, category_id, category_name,
		       unit_price, unit_cost, quantity, fulfillment_type, line_total, note,
		       dtype, dvalue, dreason, dapprover, damount, share, original, oreason, oapprover)`

	insertModifiers = `
		INSERT INTO order_line_modifiers (id, order_line_id, modifier_id, modifier_name, price_delta)
		SELECT m.id::uuid, m.line_id::uuid, m.modifier_id::uuid, m.name, m.delta
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::bigint[])
		  AS m(id, line_id, modifier_id, name, delta)`

	insertEvent = `
		INSERT INTO order_status_events (id, order_id, from_status, to_status, reason, staff_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	insertOutbox = `
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, created_at)
		VALUES ($1, 'order', $2, $3, $4, $5)`

	insertAudit = `
		INSERT INTO audit_logs (id, outlet_id, actor_id, actor_type, action, entity_type, entity_id,
		                        value_delta, reason, after, approved_by, created_at)
		VALUES ($1, $2, $3, 'staff', $4, 'order', $5, $6, $7, $8, $3, $9)`

	// Control audit rows name their own approver (pos-controls FR-CAUD-1),
	// all in one statement.
	insertAudits = `
		INSERT INTO audit_logs (id, outlet_id, actor_id, actor_type, action, entity_type, entity_id,
		                        value_delta, reason, before, after, approved_by, created_at)
		SELECT a.id::uuid, $1, $2, 'staff', a.action, 'order', $3, a.delta, a.reason, a.before::jsonb,
		       a.after::jsonb, a.approver::uuid, $4
		FROM unnest($5::text[], $6::text[], $7::bigint[], $8::text[], $9::text[], $10::text[], $11::text[])
		  AS a(id, action, delta, reason, before, after, approver)`

	// Every change takes the order row first (FR-ORD-6).
	lockOrder = `
		SELECT status, paid_at IS NOT NULL, order_number, grand_total, discount_type, discount_value,
		       discount_total
		FROM orders WHERE id = $1 AND outlet_id = $2 FOR UPDATE`

	linesByID     = `SELECT id::text, order_id::text FROM order_lines WHERE id = ANY($1::uuid[])`
	lastLineNo    = `SELECT count(*), coalesce(max(line_no), 0) FROM order_lines WHERE order_id = $1`
	activeLineNum = `SELECT count(*) FROM order_lines WHERE order_id = $1 AND status = 'active'`

	// What a recompute needs from every line, with its stored amounts.
	storedLines = `
		SELECT id::text, line_no, unit_price, quantity, discount_type, discount_value, fulfillment_type, status,
		       original_unit_price, product_name, discount_amount, line_total, order_discount_share
		FROM order_lines WHERE order_id = $1 ORDER BY line_no`

	// Every line whose amounts changed, in one statement (NFR-PERF-2).
	updateLineAmounts = `
		UPDATE order_lines l
		SET discount_amount = v.amount, line_total = v.total, order_discount_share = v.share
		FROM unnest($1::uuid[], $2::bigint[], $3::bigint[], $4::bigint[]) AS v(id, amount, total, share)
		WHERE l.id = v.id`

	updateTotals = `
		UPDATE orders
		SET subtotal = $2, discount_total = $3, service_charge = $4, packaging_fee = $5, tax_total = $6,
		    grand_total = $7, fulfillment_summary = $8, updated_at = $9
		WHERE id = $1`

	lineForVoid = `
		SELECT status, product_name, quantity, line_total
		FROM order_lines WHERE id = $1 AND order_id = $2`

	voidLine = `
		UPDATE order_lines SET status = 'voided', voided_by = $2, voided_at = $3, void_reason = $4
		WHERE id = $1`

	setStatus = `UPDATE orders SET status = $2, updated_at = $3 WHERE id = $1`

	paymentOrder = `SELECT order_id::text FROM payments WHERE id = $1`

	insertPayment = `
		INSERT INTO payments (id, order_id, method, amount, status, external_ref, recorded_by, settled_at, created_at)
		VALUES ($1, $2, $3, $4, 'settled', $5, $6, $7, $7)`

	markPaid = `UPDATE orders SET status = 'completed', paid_at = $2, updated_at = $2 WHERE id = $1`
)

const uniqueViolation = "23505"

// txRunner runs fn in one transaction; *postgres.DB implements it.
type txRunner interface {
	InTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

type Repository struct {
	tx   txRunner
	pool shift.Querier
}

func NewRepository(db *postgres.DB) *Repository { return &Repository{tx: db, pool: db.Pool} }

// write is a change made at one server time by the acting staff member.
type write struct {
	at      time.Time
	actorID string
}

type placeWrite struct {
	write
	in *PlaceInput
	// stepUp is the first failed approval of the request's controls,
	// returned only when the order is new (pos-controls FR-STP-6).
	stepUp error
}

// Place writes a new order, or returns the stored one for a retry of its ID
// at this outlet (FR-PLC-6).
//
// Two placements of one ID can race past the existence check. The loser's
// insert waits for the winner and fails on the primary key; one more attempt
// then answers from the committed order. Its number rolls back with it.
func (r *Repository) Place(ctx context.Context, w *placeWrite) (*Detail, bool, error) {
	d, applied, err := r.tryPlace(ctx, w)
	if violates(err, "orders_pkey") {
		d, applied, err = r.tryPlace(ctx, w)
	}
	return d, applied, idConflict(err)
}

func (r *Repository) tryPlace(ctx context.Context, w *placeWrite) (*Detail, bool, error) {
	var d *Detail
	applied := false
	err := r.tx.InTx(ctx, func(tx pgx.Tx) error {
		existing, err := existingOrder(ctx, tx, w.in.ID, w.in.OutletID)
		if err != nil || existing != nil {
			d = existing
			return err
		}
		if d, err = placeNew(ctx, tx, w); err != nil {
			return err
		}
		applied = true
		return writeOutbox(ctx, tx, "order.placed", &d.Order, nil, w.at)
	})
	if err != nil {
		return nil, false, err
	}
	return d, applied, nil
}

// existingOrder returns the stored order for a retried ID, nil when the ID
// is new, or conflict when another outlet holds it.
func existingOrder(ctx context.Context, tx pgx.Tx, orderID, outletID string) (*Detail, error) {
	var outlet string
	err := tx.QueryRow(ctx, orderOutlet, orderID).Scan(&outlet)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("get order: %w", err)
	case outlet != outletID:
		return nil, domain.NewError(domain.CodeConflict, "This order ID is already in use.")
	}
	return loadDetail(ctx, tx, orderID, outletID)
}

// placeNew runs FR-PLC-4: the open shift, the prices, the number, then the
// rows.
func placeNew(ctx context.Context, tx pgx.Tx, w *placeWrite) (*Detail, error) {
	in := w.in
	sh, err := shift.Current(ctx, tx, in.OutletID)
	if errors.Is(err, shift.ErrNoOpenShift) {
		return nil, domain.NewError(domain.CodeNoOpenShift, "No shift is open at this outlet.")
	}
	if err != nil {
		return nil, err
	}
	priced, err := priceLines(ctx, tx, in.OutletID, w.at, in.Lines)
	if err != nil {
		return nil, err
	}
	// Controls count only on lines that can be sold (pos-controls FR-PLA-4),
	// and are all checked before a number is taken (FR-PLA-3).
	if w.stepUp != nil {
		return nil, w.stepUp
	}
	rule := in.Discount.rule()
	t, err := computeTotals(pricedTotals(priced, 1), rule)
	if err != nil {
		return nil, err
	}
	if err := checkOrderDiscount(rule, t.Subtotal); err != nil {
		return nil, err
	}
	prefix, seq, number, err := nextNumber(ctx, tx, sh.ID)
	if err != nil {
		return nil, err
	}
	dType, dValue := ruleColumns(rule)
	var dReason, dApprover *string
	if d := in.Discount; d != nil {
		dReason, dApprover = &d.Reason, &d.approver
	}
	if _, err := tx.Exec(ctx, insertOrder, in.ID, in.OutletID, sh.ID, in.DeviceID, prefix, seq, number,
		t.FulfillmentSummary, in.TableNumber, in.Note, t.Subtotal, t.DiscountTotal, t.GrandTotal, sh.BusinessDate,
		w.at, w.actorID, dType, dValue, dReason, dApprover); err != nil {
		return nil, fmt.Errorf("insert order: %w", err)
	}
	if err := insertPriced(ctx, tx, in.ID, 1, priced, t.Lines, &w.write); err != nil {
		return nil, err
	}
	if err := writeAudits(ctx, tx, in.OutletID, in.ID, placementAudits(in, priced, &t, number), &w.write); err != nil {
		return nil, err
	}
	if err := writeEvent(ctx, tx, in.ID, nil, StatusPlaced, nil, &w.write); err != nil {
		return nil, err
	}
	return loadDetail(ctx, tx, in.ID, in.OutletID)
}

// nextNumber takes the shift's next counter number, under the prefix its
// sequences were created with.
func nextNumber(ctx context.Context, tx pgx.Tx, shiftID string) (string, int, string, error) {
	var prefix string
	if err := tx.QueryRow(ctx, shiftPrefix, shiftID, shift.PrefixWeb).Scan(&prefix); err != nil {
		return "", 0, "", fmt.Errorf("get order prefix: %w", err)
	}
	seq, number, err := shift.NextNumber(ctx, tx, shiftID, prefix)
	return prefix, seq, number, err
}

// placementAudits are the audit rows of a new order's controls: the
// lines' first, then the order discount measured against the subtotal
// (FR-CAUD-1).
func placementAudits(in *PlaceInput, priced []pricedLine, t *Totals, number string) []controlAudit {
	audits := lineAudits(priced, t.Lines, number)
	if d := in.Discount; d != nil {
		audits = append(audits, controlAudit{action: "order.discount", valueDelta: -t.DiscountTotal,
			reason: &d.Reason, approver: d.approver,
			after: withLabels(orderDiscountState(d.rule(), t.DiscountTotal), number, "")})
	}
	return audits
}

// ruleColumns are a discount's type and value columns, null when there is
// none.
func ruleColumns(r *discountRule) (*string, *int64) {
	if r == nil {
		return nil, nil
	}
	return &r.Type, &r.Value
}

// pricedTotals is the totals' view of new lines numbered from firstNo.
func pricedTotals(priced []pricedLine, firstNo int) []totalsLine {
	lines := make([]totalsLine, len(priced))
	for i, p := range priced {
		lines[i] = totalsLine{ID: p.in.ID, LineNo: firstNo + i, UnitPrice: p.unitPrice(), Quantity: p.in.Quantity,
			Discount: p.in.Discount.rule(), FulfillmentType: p.in.FulfillmentType, Status: LineActive}
	}
	return lines
}

// insertPriced writes the lines, numbered from firstNo in request order,
// with their computed amounts and controls, and their modifiers.
func insertPriced(ctx context.Context, tx pgx.Tx, orderID string, firstNo int, priced []pricedLine,
	amounts []lineAmounts, w *write) error {
	c := newLineColumns(len(priced))
	var mods modifierRows
	for i, p := range priced {
		c.add(i, firstNo+i, p, amounts[i])
		mods.add(p)
	}
	if _, err := tx.Exec(ctx, insertLines, orderID, w.actorID, w.at, c.ids, c.lineNos, c.products, c.variants,
		c.names, c.variantNames, c.categories, c.categoryNames, c.prices, c.costs, c.quantities, c.fulfillment,
		c.totals, c.notes, c.dTypes, c.dValues, c.dReasons, c.dApprovers, c.dAmounts, c.shares, c.originals,
		c.oReasons, c.oApprovers); err != nil {
		return fmt.Errorf("insert order lines: %w", err)
	}
	if len(mods.ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, insertModifiers, mods.ids, mods.lineIDs, mods.modifierIDs, mods.names,
		mods.deltas); err != nil {
		return fmt.Errorf("insert order line modifiers: %w", err)
	}
	return nil
}

// lineColumns collects new lines as columns for one insert.
type lineColumns struct {
	ids, products, names, categories, categoryNames, fulfillment []string
	variants, variantNames, notes, dTypes, dReasons, dApprovers  []*string
	oReasons, oApprovers                                         []*string
	lineNos, quantities                                          []int32
	prices, totals, dAmounts, shares                             []int64
	costs, dValues, originals                                    []*int64
}

func newLineColumns(n int) *lineColumns {
	str, ptr := func() []string { return make([]string, n) }, func() []*string { return make([]*string, n) }
	num, opt := func() []int64 { return make([]int64, n) }, func() []*int64 { return make([]*int64, n) }
	return &lineColumns{
		ids: str(), products: str(), names: str(), categories: str(), categoryNames: str(), fulfillment: str(),
		variants: ptr(), variantNames: ptr(), notes: ptr(), dTypes: ptr(), dReasons: ptr(), dApprovers: ptr(),
		oReasons: ptr(), oApprovers: ptr(), lineNos: make([]int32, n), quantities: make([]int32, n),
		prices: num(), totals: num(), dAmounts: num(), shares: num(), costs: opt(), dValues: opt(), originals: opt(),
	}
}

func (c *lineColumns) add(i, lineNo int, p pricedLine, a lineAmounts) {
	c.ids[i], c.lineNos[i], c.quantities[i] = p.in.ID, int32(lineNo), int32(p.in.Quantity)
	c.products[i], c.names[i] = p.snap.ProductID.String(), p.snap.ProductName
	c.categories[i], c.categoryNames[i] = p.snap.CategoryID.String(), p.snap.CategoryName
	c.variantNames[i], c.prices[i], c.costs[i] = p.snap.VariantName, p.unitPrice(), p.snap.UnitCost
	c.fulfillment[i], c.notes[i] = p.in.FulfillmentType, p.in.Note
	c.totals[i], c.dAmounts[i], c.shares[i] = a.LineTotal, a.DiscountAmount, a.Share
	if p.snap.VariantID != nil {
		v := p.snap.VariantID.String()
		c.variants[i] = &v
	}
	if d := p.in.Discount; d != nil {
		c.dTypes[i], c.dValues[i], c.dReasons[i], c.dApprovers[i] = &d.Type, &d.Value, &d.Reason, &d.approver
	}
	if o := p.in.Override; o != nil {
		original := p.snap.UnitPrice
		c.originals[i], c.oReasons[i], c.oApprovers[i] = &original, &o.Reason, &o.approver
	}
}

// modifierRows collects modifier snapshots as columns for one insert.
type modifierRows struct {
	ids, lineIDs, modifierIDs, names []string
	deltas                           []int64
}

func (m *modifierRows) add(p pricedLine) {
	for _, s := range p.snap.Modifiers {
		m.ids = append(m.ids, uuid.NewString())
		m.lineIDs = append(m.lineIDs, p.in.ID)
		m.modifierIDs = append(m.modifierIDs, s.ID.String())
		m.names = append(m.names, s.Name)
		m.deltas = append(m.deltas, s.PriceDelta)
	}
}

// writeEvent records one status change (FR-ORD-4).
func writeEvent(ctx context.Context, tx pgx.Tx, orderID string, from *string, to string, reason *string,
	w *write) error {
	if _, err := tx.Exec(ctx, insertEvent, uuid.NewString(), orderID, from, to, reason, w.actorID, w.at); err != nil {
		return fmt.Errorf("insert order status event: %w", err)
	}
	return nil
}

// eventPayload is an outbox payload (FR-EVT-2). Consumers read the rest
// from the order.
type eventPayload struct {
	V            int       `json:"v"`
	OrderID      string    `json:"order_id"`
	OutletID     string    `json:"outlet_id"`
	ShiftID      string    `json:"shift_id"`
	BusinessDate string    `json:"business_date"`
	Channel      string    `json:"channel"`
	OrderNumber  string    `json:"order_number"`
	Status       string    `json:"status"`
	GrandTotal   int64     `json:"grand_total"`
	LineIDs      *[]string `json:"line_ids,omitempty"`
}

// writeOutbox records an order event for a future worker, from the order
// as it stands after the change (FR-EVT-1).
func writeOutbox(ctx context.Context, tx pgx.Tx, event string, d *Order, lineIDs []string, at time.Time) error {
	p := eventPayload{V: 1, OrderID: d.ID, OutletID: d.OutletID, ShiftID: d.ShiftID, BusinessDate: d.BusinessDate,
		Channel: d.Channel, OrderNumber: d.OrderNumber, Status: d.Status, GrandTotal: d.GrandTotal}
	// nil leaves line_ids out; an empty list is sent as [] (FR-OPN-7).
	if lineIDs != nil {
		p.LineIDs = &lineIDs
	}
	if _, err := tx.Exec(ctx, insertOutbox, uuid.NewString(), d.ID, event, p, at); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// auditEntry is one audit_logs row for a void or a cancellation
// (FR-OAUD-1). The actor is their own approver: no step-up here.
type auditEntry struct {
	outletID, orderID, action, reason string
	valueDelta                        int64
	after                             map[string]any
}

func writeAudit(ctx context.Context, tx pgx.Tx, a *auditEntry, w *write) error {
	if _, err := tx.Exec(ctx, insertAudit, uuid.NewString(), a.outletID, w.actorID, a.action, a.orderID,
		a.valueDelta, a.reason, a.after, w.at); err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// controlAudit is one audit_logs row for a control (pos-controls
// FR-CAUD-1). before and after are nil when there is no such state.
type controlAudit struct {
	action        string
	valueDelta    int64
	reason        *string
	before, after map[string]any
	approver      string
}

// writeAudits writes control audit rows in one statement.
func writeAudits(ctx context.Context, tx pgx.Tx, outletID, orderID string, audits []controlAudit, w *write) error {
	if len(audits) == 0 {
		return nil
	}
	n := len(audits)
	ids, actions, approvers := make([]string, n), make([]string, n), make([]string, n)
	deltas := make([]int64, n)
	reasons, befores, afters := make([]*string, n), make([]*string, n), make([]*string, n)
	for i, a := range audits {
		ids[i], actions[i], approvers[i], deltas[i], reasons[i] = uuid.NewString(), a.action, a.approver,
			a.valueDelta, a.reason
		var err error
		if befores[i], err = jsonOrNil(a.before); err != nil {
			return err
		}
		if afters[i], err = jsonOrNil(a.after); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, insertAudits, outletID, w.actorID, orderID, w.at, ids, actions, deltas, reasons,
		befores, afters, approvers); err != nil {
		return fmt.Errorf("insert audit logs: %w", err)
	}
	return nil
}

func jsonOrNil(m map[string]any) (*string, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode audit state: %w", err)
	}
	s := string(b)
	return &s, nil
}

// lockedOrder is the order row as a change sees it, under FOR UPDATE.
type lockedOrder struct {
	status        string
	paid          bool
	orderNumber   string
	grandTotal    int64
	discount      *discountRule
	discountTotal int64
}

// lock takes the order row, or not_found for an order of another outlet.
func lock(ctx context.Context, tx pgx.Tx, orderID, outletID string) (*lockedOrder, error) {
	var o lockedOrder
	var dType *string
	var dValue *int64
	err := tx.QueryRow(ctx, lockOrder, orderID, outletID).Scan(&o.status, &o.paid, &o.orderNumber, &o.grandTotal,
		&dType, &dValue, &o.discountTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock order: %w", err)
	}
	if dType != nil && dValue != nil {
		o.discount = &discountRule{Type: *dType, Value: *dValue}
	}
	return &o, nil
}

// notOpen refuses a change to an order that is no longer an open bill
// (FR-ORD-7).
func notOpen(status string) error {
	return &domain.Error{
		Code:    domain.CodeOrderNotOpen,
		Message: "This order can no longer be changed.",
		Details: map[string]string{"status": status},
	}
}

// storedLine is a line as a recompute reads it, with its stored amounts.
type storedLine struct {
	totalsLine
	original    *int64
	productName string
	stored      lineAmounts
}

func loadStoredLines(ctx context.Context, tx pgx.Tx, orderID string) ([]storedLine, error) {
	rows, err := tx.Query(ctx, storedLines, orderID)
	if err != nil {
		return nil, fmt.Errorf("get order lines: %w", err)
	}
	defer rows.Close()
	var lines []storedLine
	for rows.Next() {
		var l storedLine
		var dType *string
		var dValue *int64
		if err := rows.Scan(&l.ID, &l.LineNo, &l.UnitPrice, &l.Quantity, &dType, &dValue, &l.FulfillmentType,
			&l.Status, &l.original, &l.productName, &l.stored.DiscountAmount, &l.stored.LineTotal,
			&l.stored.Share); err != nil {
			return nil, fmt.Errorf("scan order line: %w", err)
		}
		if dType != nil && dValue != nil {
			l.Discount = &discountRule{Type: *dType, Value: *dValue}
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read order lines: %w", err)
	}
	return lines, nil
}

func totalsOf(lines []storedLine) []totalsLine {
	out := make([]totalsLine, len(lines))
	for i := range lines {
		out[i] = lines[i].totalsLine
	}
	return out
}

// recompute recalculates and stores every line's amounts and the order's
// totals after lines were added or voided (pos-controls FR-TOT-14).
// Existing snapshots are never re-priced (FR-TOT-5).
func recompute(ctx context.Context, tx pgx.Tx, orderID string, discount *discountRule, at time.Time) error {
	lines, err := loadStoredLines(ctx, tx, orderID)
	if err != nil {
		return err
	}
	t, err := computeTotals(totalsOf(lines), discount)
	if err != nil {
		return err
	}
	return storeTotals(ctx, tx, orderID, lines, &t, at)
}

// storeTotals writes the amounts of the lines that changed, in one
// statement, and the order's totals.
func storeTotals(ctx context.Context, tx pgx.Tx, orderID string, lines []storedLine, t *Totals,
	at time.Time) error {
	var ids []string
	var amounts, totals, shares []int64
	for i, l := range lines {
		if a := t.Lines[i]; a != l.stored {
			ids, amounts, totals, shares = append(ids, l.ID), append(amounts, a.DiscountAmount),
				append(totals, a.LineTotal), append(shares, a.Share)
		}
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, updateLineAmounts, ids, amounts, totals, shares); err != nil {
			return fmt.Errorf("update line amounts: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, updateTotals, orderID, t.Subtotal, t.DiscountTotal, t.ServiceCharge,
		t.PackagingFee, t.TaxTotal, t.GrandTotal, t.FulfillmentSummary, at); err != nil {
		return fmt.Errorf("update order totals: %w", err)
	}
	return nil
}

// violates reports whether err is a unique violation of the constraint.
func violates(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

// idConflict turns a client-generated ID that collided with a row written
// concurrently elsewhere into conflict.
func idConflict(err error) error {
	for _, c := range []string{"orders_pkey", "order_lines_pkey", "payments_pkey"} {
		if violates(err, c) {
			return domain.WrapError(domain.CodeConflict, "This ID is already in use.", err)
		}
	}
	return err
}
