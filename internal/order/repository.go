package order

import (
	"context"
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

	insertOrder = `
		INSERT INTO orders (id, outlet_id, shift_id, device_id, order_prefix, order_sequence, order_number,
		                    channel, fulfillment_summary, status, table_number, note, subtotal, grand_total,
		                    business_date, placed_at, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pos', $8, 'placed', $9, $10, $11, $12, $13::date, $14, $15, $14, $14)`

	// Lines and modifiers go in one statement each, whatever the count
	// (NFR-PERF-1). Nullable UUIDs travel as text.
	insertLines = `
		INSERT INTO order_lines (id, order_id, line_no, product_id, product_variant_id, product_name, variant_name,
		                         category_id, category_name, unit_price, unit_cost, quantity, fulfillment_type,
		                         line_total, note, created_by, created_at)
		SELECT l.id::uuid, $1, l.line_no, l.product_id::uuid, l.variant_id::uuid, l.product_name, l.variant_name,
		       l.category_id::uuid, l.category_name, l.unit_price, l.unit_cost, l.quantity, l.fulfillment_type,
		       l.line_total, l.note, $2, $3
		FROM unnest($4::text[], $5::int[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[],
		            $11::text[], $12::bigint[], $13::bigint[], $14::int[], $15::text[], $16::bigint[], $17::text[])
		  AS l(id, line_no, product_id, variant_id, product_name, variant_name, category_id, category_name,
		       unit_price, unit_cost, quantity, fulfillment_type, line_total, note)`

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

	// Every change takes the order row first (FR-ORD-6).
	lockOrder = `
		SELECT status, paid_at IS NOT NULL, order_number, grand_total
		FROM orders WHERE id = $1 AND outlet_id = $2 FOR UPDATE`

	linesByID     = `SELECT id::text, order_id::text FROM order_lines WHERE id = ANY($1::uuid[])`
	lastLineNo    = `SELECT count(*), coalesce(max(line_no), 0) FROM order_lines WHERE order_id = $1`
	lineTotals    = `SELECT line_total, fulfillment_type, status FROM order_lines WHERE order_id = $1`
	activeLineNum = `SELECT count(*) FROM order_lines WHERE order_id = $1 AND status = 'active'`

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
	var prefix string
	if err := tx.QueryRow(ctx, shiftPrefix, sh.ID, shift.PrefixWeb).Scan(&prefix); err != nil {
		return nil, fmt.Errorf("get order prefix: %w", err)
	}
	seq, number, err := shift.NextNumber(ctx, tx, sh.ID, prefix)
	if err != nil {
		return nil, err
	}
	t := computeTotals(pricedTotals(priced))
	if _, err := tx.Exec(ctx, insertOrder, in.ID, in.OutletID, sh.ID, in.DeviceID, prefix, seq, number,
		t.FulfillmentSummary, in.TableNumber, in.Note, t.Subtotal, t.GrandTotal, sh.BusinessDate,
		w.at, w.actorID); err != nil {
		return nil, fmt.Errorf("insert order: %w", err)
	}
	if err := insertPriced(ctx, tx, in.ID, 1, priced, &w.write); err != nil {
		return nil, err
	}
	if err := writeEvent(ctx, tx, in.ID, nil, StatusPlaced, nil, &w.write); err != nil {
		return nil, err
	}
	return loadDetail(ctx, tx, in.ID, in.OutletID)
}

func pricedTotals(priced []pricedLine) []totalsLine {
	lines := make([]totalsLine, len(priced))
	for i, p := range priced {
		lines[i] = totalsLine{LineTotal: p.lineTotal, FulfillmentType: p.in.FulfillmentType, Status: LineActive}
	}
	return lines
}

// insertPriced writes the lines, numbered from firstNo in request order,
// and their modifiers.
func insertPriced(ctx context.Context, tx pgx.Tx, orderID string, firstNo int, priced []pricedLine, w *write) error {
	n := len(priced)
	ids, products, names, categories, categoryNames, fulfillment := make([]string, n), make([]string, n),
		make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	variants, variantNames, notes := make([]*string, n), make([]*string, n), make([]*string, n)
	lineNos, quantities := make([]int32, n), make([]int32, n)
	prices, totals, costs := make([]int64, n), make([]int64, n), make([]*int64, n)
	var mods modifierRows
	for i, p := range priced {
		ids[i], lineNos[i], quantities[i] = p.in.ID, int32(firstNo+i), int32(p.in.Quantity)
		products[i], names[i] = p.snap.ProductID.String(), p.snap.ProductName
		categories[i], categoryNames[i] = p.snap.CategoryID.String(), p.snap.CategoryName
		variantNames[i], prices[i], costs[i], totals[i] = p.snap.VariantName, p.snap.UnitPrice, p.snap.UnitCost, p.lineTotal
		fulfillment[i], notes[i] = p.in.FulfillmentType, p.in.Note
		if p.snap.VariantID != nil {
			v := p.snap.VariantID.String()
			variants[i] = &v
		}
		mods.add(p)
	}
	if _, err := tx.Exec(ctx, insertLines, orderID, w.actorID, w.at, ids, lineNos, products, variants, names,
		variantNames, categories, categoryNames, prices, costs, quantities, fulfillment, totals, notes); err != nil {
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
	V            int      `json:"v"`
	OrderID      string   `json:"order_id"`
	OutletID     string   `json:"outlet_id"`
	ShiftID      string   `json:"shift_id"`
	BusinessDate string   `json:"business_date"`
	Channel      string   `json:"channel"`
	OrderNumber  string   `json:"order_number"`
	Status       string   `json:"status"`
	GrandTotal   int64    `json:"grand_total"`
	LineIDs      []string `json:"line_ids,omitempty"`
}

// writeOutbox records an order event for a future worker, from the order
// as it stands after the change (FR-EVT-1).
func writeOutbox(ctx context.Context, tx pgx.Tx, event string, d *Order, lineIDs []string, at time.Time) error {
	p := eventPayload{V: 1, OrderID: d.ID, OutletID: d.OutletID, ShiftID: d.ShiftID, BusinessDate: d.BusinessDate,
		Channel: d.Channel, OrderNumber: d.OrderNumber, Status: d.Status, GrandTotal: d.GrandTotal, LineIDs: lineIDs}
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

// lockedOrder is the order row as a change sees it, under FOR UPDATE.
type lockedOrder struct {
	status      string
	paid        bool
	orderNumber string
	grandTotal  int64
}

// lock takes the order row, or not_found for an order of another outlet.
func lock(ctx context.Context, tx pgx.Tx, orderID, outletID string) (*lockedOrder, error) {
	var o lockedOrder
	err := tx.QueryRow(ctx, lockOrder, orderID, outletID).Scan(&o.status, &o.paid, &o.orderNumber, &o.grandTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock order: %w", err)
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

// recomputeTotals stores the totals of the order's active lines
// (FR-TOT-5). Existing snapshots are never re-priced.
func recomputeTotals(ctx context.Context, tx pgx.Tx, orderID string, at time.Time) error {
	rows, err := tx.Query(ctx, lineTotals, orderID)
	if err != nil {
		return fmt.Errorf("get line totals: %w", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[totalsLine])
	if err != nil {
		return fmt.Errorf("read line totals: %w", err)
	}
	t := computeTotals(lines)
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
