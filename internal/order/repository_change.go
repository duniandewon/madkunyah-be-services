package order

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/jackc/pgx/v5"
)

type addWrite struct {
	write
	in *AddLinesInput
}

type voidWrite struct {
	write
	in *VoidInput
}

type cancelWrite struct {
	write
	in *CancelInput
}

type payWrite struct {
	write
	in *PayInput
}

// change runs fn on a locked order of the outlet and returns the order as
// it stands afterwards. fn reports whether it changed anything.
func (r *Repository) change(ctx context.Context, orderID, outletID string,
	fn func(tx pgx.Tx, o *lockedOrder) (bool, error)) (*Detail, bool, error) {
	var d *Detail
	applied := false
	err := r.tx.InTx(ctx, func(tx pgx.Tx) error {
		o, err := lock(ctx, tx, orderID, outletID)
		if err != nil {
			return err
		}
		if applied, err = fn(tx, o); err != nil {
			return err
		}
		d, err = loadDetail(ctx, tx, orderID, outletID)
		return err
	})
	if err != nil {
		return nil, false, idConflict(err)
	}
	return d, applied, nil
}

// AddLines adds priced lines to an open bill (FR-ADD). A request whose line
// IDs all exist on the order is a retry; any other overlap is a conflict.
func (r *Repository) AddLines(ctx context.Context, w *addWrite) (*Detail, bool, error) {
	in := w.in
	return r.change(ctx, in.OrderID, in.OutletID, func(tx pgx.Tx, o *lockedOrder) (bool, error) {
		retry, err := knownLines(ctx, tx, in)
		if err != nil || retry {
			return false, err
		}
		if o.status != StatusPlaced {
			return false, notOpen(o.status)
		}
		var count, last int
		if err := tx.QueryRow(ctx, lastLineNo, in.OrderID).Scan(&count, &last); err != nil {
			return false, fmt.Errorf("count order lines: %w", err)
		}
		if count+len(in.Lines) > maxLines {
			return false, domain.ValidationError(map[string]string{
				"lines": "an order holds at most " + strconv.Itoa(maxLines) + " lines; it has " + strconv.Itoa(count),
			})
		}
		priced, err := priceLines(ctx, tx, in.OutletID, w.at, in.Lines)
		if err != nil {
			return false, err
		}
		if err := insertPriced(ctx, tx, in.OrderID, last+1, priced, &w.write); err != nil {
			return false, err
		}
		if err := recomputeTotals(ctx, tx, in.OrderID, w.at); err != nil {
			return false, err
		}
		return true, outboxAfter(ctx, tx, "order.updated", in.OrderID, in.OutletID, lineIDs(in.Lines), &w.write)
	})
}

// knownLines reports a retry: every line ID already on this order. Some
// but not all, or any on another order, is a conflict (FR-ADD-3).
func knownLines(ctx context.Context, tx pgx.Tx, in *AddLinesInput) (bool, error) {
	rows, err := tx.Query(ctx, linesByID, lineIDs(in.Lines))
	if err != nil {
		return false, fmt.Errorf("get lines: %w", err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID, OrderID string
	}])
	if err != nil {
		return false, fmt.Errorf("read lines: %w", err)
	}
	if len(owners) == 0 {
		return false, nil
	}
	for _, o := range owners {
		if o.OrderID != in.OrderID {
			return false, domain.NewError(domain.CodeConflict, "A line ID is already in use on another order.")
		}
	}
	if len(owners) != len(in.Lines) {
		return false, domain.NewError(domain.CodeConflict, "Some of these lines are already on the order.")
	}
	return true, nil
}

func lineIDs(lines []LineInput) []string {
	ids := make([]string, len(lines))
	for i, l := range lines {
		ids[i] = l.ID
	}
	return ids
}

// VoidLine voids one line of an open bill with a reason (FR-VOID). An
// already-voided line is a retry.
func (r *Repository) VoidLine(ctx context.Context, w *voidWrite) (*Detail, bool, error) {
	in := w.in
	return r.change(ctx, in.OrderID, in.OutletID, func(tx pgx.Tx, o *lockedOrder) (bool, error) {
		var status, product string
		var quantity int
		var total int64
		err := tx.QueryRow(ctx, lineForVoid, in.LineID, in.OrderID).Scan(&status, &product, &quantity, &total)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, domain.ErrNotFound
		}
		if err != nil {
			return false, fmt.Errorf("get line: %w", err)
		}
		if status == LineVoided {
			return false, nil
		}
		if o.status != StatusPlaced {
			return false, notOpen(o.status)
		}
		var active int
		if err := tx.QueryRow(ctx, activeLineNum, in.OrderID).Scan(&active); err != nil {
			return false, fmt.Errorf("count active lines: %w", err)
		}
		if active <= 1 {
			return false, domain.NewError(domain.CodeLastActiveLine,
				"This is the last line on the order. Cancel the order instead.")
		}
		if _, err := tx.Exec(ctx, voidLine, in.LineID, w.actorID, w.at, in.Reason); err != nil {
			return false, fmt.Errorf("void line: %w", err)
		}
		if err := recomputeTotals(ctx, tx, in.OrderID, w.at); err != nil {
			return false, err
		}
		if err := writeAudit(ctx, tx, &auditEntry{outletID: in.OutletID, orderID: in.OrderID,
			action: "order.line_voided", reason: in.Reason, valueDelta: -total,
			after: map[string]any{"line_id": in.LineID, "product_name": product, "quantity": quantity,
				"order_number": o.orderNumber},
		}, &w.write); err != nil {
			return false, err
		}
		return true, outboxAfter(ctx, tx, "order.updated", in.OrderID, in.OutletID, []string{in.LineID}, &w.write)
	})
}

// Cancel ends an open bill without payment, keeping its lines and totals
// (FR-CAN). An already-cancelled order is a retry.
func (r *Repository) Cancel(ctx context.Context, w *cancelWrite) (*Detail, bool, error) {
	in := w.in
	return r.change(ctx, in.OrderID, in.OutletID, func(tx pgx.Tx, o *lockedOrder) (bool, error) {
		if o.status == StatusCancelled {
			return false, nil
		}
		if o.status != StatusPlaced {
			return false, notOpen(o.status)
		}
		var active int
		if err := tx.QueryRow(ctx, activeLineNum, in.OrderID).Scan(&active); err != nil {
			return false, fmt.Errorf("count active lines: %w", err)
		}
		if _, err := tx.Exec(ctx, setStatus, in.OrderID, StatusCancelled, w.at); err != nil {
			return false, fmt.Errorf("cancel order: %w", err)
		}
		from := o.status
		if err := writeEvent(ctx, tx, in.OrderID, &from, StatusCancelled, &in.Reason, &w.write); err != nil {
			return false, err
		}
		if err := writeAudit(ctx, tx, &auditEntry{outletID: in.OutletID, orderID: in.OrderID,
			action: "order.cancelled", reason: in.Reason, valueDelta: -o.grandTotal,
			after: map[string]any{"order_number": o.orderNumber, "active_lines": active},
		}, &w.write); err != nil {
			return false, err
		}
		return true, outboxAfter(ctx, tx, "order.cancelled", in.OrderID, in.OutletID, nil, &w.write)
	})
}

// Pay records a settled payment for exactly the grand total and completes
// the order (FR-PAY). The payment ID makes a retry safe.
func (r *Repository) Pay(ctx context.Context, w *payWrite) (*Detail, bool, error) {
	in := w.in
	return r.change(ctx, in.OrderID, in.OutletID, func(tx pgx.Tx, o *lockedOrder) (bool, error) {
		var owner string
		err := tx.QueryRow(ctx, paymentOrder, in.PaymentID).Scan(&owner)
		switch {
		case err == nil && owner == in.OrderID:
			return false, nil
		case err == nil:
			return false, domain.NewError(domain.CodeConflict, "This payment ID is already in use.")
		case !errors.Is(err, pgx.ErrNoRows):
			return false, fmt.Errorf("get payment: %w", err)
		}
		if err := payable(o, in.Amount); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, insertPayment, in.PaymentID, in.OrderID, in.Method, in.Amount,
			in.ExternalRef, w.actorID, w.at); err != nil {
			return false, fmt.Errorf("insert payment: %w", err)
		}
		if _, err := tx.Exec(ctx, markPaid, in.OrderID, w.at); err != nil {
			return false, fmt.Errorf("complete order: %w", err)
		}
		from := o.status
		if err := writeEvent(ctx, tx, in.OrderID, &from, StatusCompleted, nil, &w.write); err != nil {
			return false, err
		}
		return true, outboxAfter(ctx, tx, "order.paid", in.OrderID, in.OutletID, nil, &w.write)
	})
}

// payable checks a new payment against the locked order (FR-PAY-3,
// FR-PAY-5).
func payable(o *lockedOrder, amount int64) error {
	switch {
	case o.paid:
		return domain.NewError(domain.CodeOrderAlreadyPaid, "This order has already been paid.")
	case o.status != StatusPlaced:
		return notOpen(o.status)
	case amount != o.grandTotal:
		return &domain.Error{
			Code:    domain.CodeAmountMismatch,
			Message: "The amount does not match the order total.",
			Details: map[string]int64{"grand_total": o.grandTotal},
		}
	}
	return nil
}

// outboxAfter writes an outbox event from the order as the change left it.
func outboxAfter(ctx context.Context, tx pgx.Tx, event, orderID, outletID string, lineIDs []string, w *write) error {
	o, err := loadHeader(ctx, tx, orderID, outletID)
	if err != nil {
		return err
	}
	return writeOutbox(ctx, tx, event, o, lineIDs, w.at)
}
