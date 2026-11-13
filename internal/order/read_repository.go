package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/shift"
	"github.com/jackc/pgx/v5"
)

const (
	selectOrder = `
		SELECT o.id::text, o.outlet_id::text, o.shift_id::text, o.device_id::text, o.order_number, o.channel,
		       o.status, o.fulfillment_summary, o.business_date::text, o.table_number, o.note,
		       o.subtotal, o.discount_total, o.service_charge, o.packaging_fee, o.tax_total, o.grand_total,
		       o.placed_at, o.paid_at, o.created_by::text, cb.full_name, o.created_at
		FROM orders o
		LEFT JOIN staff cb ON cb.id = o.created_by`

	getOrder = selectOrder + ` WHERE o.id = $1 AND o.outlet_id = $2`

	// Keyset pagination in index order; a null filter matches everything.
	// Within one shift the business date is constant, so the POS list is
	// newest first by creation.
	listOrders = selectOrder + `
		WHERE o.outlet_id = $1
		  AND ($2::date IS NULL OR o.business_date >= $2::date)
		  AND ($3::date IS NULL OR o.business_date <= $3::date)
		  AND ($4::text IS NULL OR o.status = $4::text)
		  AND ($5::uuid IS NULL OR o.shift_id = $5::uuid)
		  AND ($6::text IS NULL OR o.order_number = $6::text)
		  AND ($7::date IS NULL OR (o.business_date, o.created_at, o.id) < ($7::date, $8::timestamptz, $9::uuid))
		ORDER BY o.business_date DESC, o.created_at DESC, o.id DESC
		LIMIT $10`

	linesOf = `
		SELECT l.id::text, l.product_id::text, l.product_variant_id::text, l.product_name, l.variant_name,
		       l.category_id::text, l.category_name, l.unit_price, l.quantity, l.discount_amount, l.line_total,
		       l.fulfillment_type, l.note, l.status, l.created_by::text, cb.full_name, l.created_at,
		       l.voided_by::text, vb.full_name, l.voided_at, l.void_reason
		FROM order_lines l
		LEFT JOIN staff cb ON cb.id = l.created_by
		LEFT JOIN staff vb ON vb.id = l.voided_by
		WHERE l.order_id = $1
		ORDER BY l.line_no`

	modifiersOf = `
		SELECT m.order_line_id::text, m.modifier_id::text, m.modifier_name, m.price_delta
		FROM order_line_modifiers m
		JOIN order_lines l ON l.id = m.order_line_id
		WHERE l.order_id = $1
		ORDER BY m.modifier_name, m.modifier_id`

	paymentsOf = `
		SELECT p.id::text, p.method, p.amount, p.status, p.external_ref, p.recorded_by::text, rb.full_name,
		       p.settled_at
		FROM payments p
		LEFT JOIN staff rb ON rb.id = p.recorded_by
		WHERE p.order_id = $1
		ORDER BY p.created_at, p.id`

	eventsOf = `
		SELECT e.from_status, e.to_status, e.reason, e.staff_id::text, s.full_name, e.created_at
		FROM order_status_events e
		LEFT JOIN staff s ON s.id = e.staff_id
		WHERE e.order_id = $1
		ORDER BY e.created_at, e.id`

	openShiftID = `SELECT id::text FROM shifts WHERE outlet_id = $1 AND status = 'open'`

	// The outlet's trading day at $2: the local date, less the cutoff.
	businessToday = `
		SELECT (($2::timestamptz AT TIME ZONE timezone) - business_day_cutoff::interval)::date::text
		FROM outlets WHERE id = $1`
)

// Get returns one order of the outlet with everything on it (FR-RD-2,
// FR-RD-4).
func (r *Repository) Get(ctx context.Context, outletID, orderID string) (*Detail, error) {
	return loadDetail(ctx, r.pool, orderID, outletID)
}

// List returns up to f.Limit orders, newest first (FR-RD-1, FR-RD-3).
func (r *Repository) List(ctx context.Context, f *ListFilter) ([]Order, error) {
	var afterDate, afterID *string
	var afterAt *time.Time
	if f.After != nil {
		afterDate, afterAt, afterID = &f.After.date, &f.After.createdAt, &f.After.id
	}
	rows, err := r.pool.Query(ctx, listOrders, f.OutletID, nullIfEmpty(f.From), nullIfEmpty(f.To),
		nullIfEmpty(f.Status), nullIfEmpty(f.ShiftID), nullIfEmpty(f.OrderNumber),
		afterDate, afterAt, afterID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		orders = append(orders, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read orders: %w", err)
	}
	return orders, nil
}

// OpenShiftID returns the outlet's open shift, or "" when none is open.
func (r *Repository) OpenShiftID(ctx context.Context, outletID string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, openShiftID, outletID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get open shift: %w", err)
	}
	return id, nil
}

// BusinessToday returns the outlet's business date at `at`, by the same
// rule as a shift's (shifts FR-SH-6).
func (r *Repository) BusinessToday(ctx context.Context, outletID string, at time.Time) (string, error) {
	var date string
	err := r.pool.QueryRow(ctx, businessToday, outletID, at).Scan(&date)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get business date: %w", err)
	}
	return date, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type rowScanner interface {
	Scan(dest ...any) error
}

func staffRef(id, name *string) *StaffRef {
	if id == nil || name == nil {
		return nil
	}
	return &StaffRef{ID: *id, Name: *name}
}

func scanOrder(row rowScanner) (*Order, error) {
	var o Order
	var createdBy, createdByName *string
	err := row.Scan(&o.ID, &o.OutletID, &o.ShiftID, &o.DeviceID, &o.OrderNumber, &o.Channel,
		&o.Status, &o.FulfillmentSummary, &o.BusinessDate, &o.TableNumber, &o.Note,
		&o.Subtotal, &o.DiscountTotal, &o.ServiceCharge, &o.PackagingFee, &o.TaxTotal, &o.GrandTotal,
		&o.PlacedAt, &o.PaidAt, &createdBy, &createdByName, &o.CreatedAt)
	if err != nil {
		return nil, err
	}
	o.CreatedBy = staffRef(createdBy, createdByName)
	return &o, nil
}

// loadHeader reads one order of the outlet without its children, or
// not_found.
func loadHeader(ctx context.Context, q shift.Querier, orderID, outletID string) (*Order, error) {
	o, err := scanOrder(q.QueryRow(ctx, getOrder, orderID, outletID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	return o, nil
}

// loadDetail reads one order with its lines, modifiers, payments and
// events in five queries, whatever the line count (NFR-PERF-3).
func loadDetail(ctx context.Context, q shift.Querier, orderID, outletID string) (*Detail, error) {
	o, err := loadHeader(ctx, q, orderID, outletID)
	if err != nil {
		return nil, err
	}
	d := &Detail{Order: *o}
	if d.Lines, err = loadLines(ctx, q, orderID); err != nil {
		return nil, err
	}
	if d.Payments, err = loadPayments(ctx, q, orderID); err != nil {
		return nil, err
	}
	if d.Events, err = loadEvents(ctx, q, orderID); err != nil {
		return nil, err
	}
	return d, nil
}

func loadLines(ctx context.Context, q shift.Querier, orderID string) ([]Line, error) {
	rows, err := q.Query(ctx, linesOf, orderID)
	if err != nil {
		return nil, fmt.Errorf("get order lines: %w", err)
	}
	defer rows.Close()
	lines := []Line{}
	index := map[string]int{}
	for rows.Next() {
		var l Line
		var createdBy, createdByName, voidedBy, voidedByName *string
		if err := rows.Scan(&l.ID, &l.ProductID, &l.VariantID, &l.ProductName, &l.VariantName,
			&l.CategoryID, &l.CategoryName, &l.UnitPrice, &l.Quantity, &l.DiscountAmount, &l.LineTotal,
			&l.FulfillmentType, &l.Note, &l.Status, &createdBy, &createdByName, &l.CreatedAt,
			&voidedBy, &voidedByName, &l.VoidedAt, &l.VoidReason); err != nil {
			return nil, fmt.Errorf("scan order line: %w", err)
		}
		l.CreatedBy, l.VoidedBy = staffRef(createdBy, createdByName), staffRef(voidedBy, voidedByName)
		l.Modifiers = []Modifier{}
		index[l.ID] = len(lines)
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read order lines: %w", err)
	}
	return lines, attachModifiers(ctx, q, orderID, lines, index)
}

func attachModifiers(ctx context.Context, q shift.Querier, orderID string, lines []Line, index map[string]int) error {
	rows, err := q.Query(ctx, modifiersOf, orderID)
	if err != nil {
		return fmt.Errorf("get order line modifiers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var lineID string
		var m Modifier
		if err := rows.Scan(&lineID, &m.ID, &m.Name, &m.PriceDelta); err != nil {
			return fmt.Errorf("scan order line modifier: %w", err)
		}
		if i, ok := index[lineID]; ok {
			lines[i].Modifiers = append(lines[i].Modifiers, m)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read order line modifiers: %w", err)
	}
	return nil
}

func loadPayments(ctx context.Context, q shift.Querier, orderID string) ([]Payment, error) {
	rows, err := q.Query(ctx, paymentsOf, orderID)
	if err != nil {
		return nil, fmt.Errorf("get payments: %w", err)
	}
	defer rows.Close()
	payments := []Payment{}
	for rows.Next() {
		var p Payment
		var by, byName *string
		if err := rows.Scan(&p.ID, &p.Method, &p.Amount, &p.Status, &p.ExternalRef, &by, &byName,
			&p.SettledAt); err != nil {
			return nil, fmt.Errorf("scan payment: %w", err)
		}
		p.RecordedBy = staffRef(by, byName)
		payments = append(payments, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read payments: %w", err)
	}
	return payments, nil
}

func loadEvents(ctx context.Context, q shift.Querier, orderID string) ([]Event, error) {
	rows, err := q.Query(ctx, eventsOf, orderID)
	if err != nil {
		return nil, fmt.Errorf("get order events: %w", err)
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		var by, byName *string
		if err := rows.Scan(&e.From, &e.To, &e.Reason, &by, &byName, &e.At); err != nil {
			return nil, fmt.Errorf("scan order event: %w", err)
		}
		e.Staff = staffRef(by, byName)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read order events: %w", err)
	}
	return events, nil
}
