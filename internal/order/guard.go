package order

import (
	"context"
	"fmt"

	"github.com/duniandewon/madkunyah-be-service/internal/shift"
)

// Unfinished orders of a shift, oldest first, with the total before the
// limit in the same query.
const unfinishedOrders = `
	SELECT id::text, order_number, status, channel, count(*) OVER ()
	FROM orders
	WHERE shift_id = $1 AND status <> ALL($2::text[])
	ORDER BY created_at, id
	LIMIT $3`

// Guard is the shift close guard: a shift with an unfinished order cannot
// close (FR-GRD-2). It reads in the closing transaction, which holds the
// shift row, so a placement either commits first and is listed or finds no
// open shift (FR-GRD-3).
type Guard struct{}

var _ shift.OrderGuard = Guard{}

func (Guard) UnfinishedOrders(ctx context.Context, q shift.Querier, shiftID string, limit int) ([]shift.OrderRef, int, error) {
	rows, err := q.Query(ctx, unfinishedOrders, shiftID, finishedStatuses, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("get unfinished orders: %w", err)
	}
	defer rows.Close()
	var orders []shift.OrderRef
	count := 0
	for rows.Next() {
		var o shift.OrderRef
		if err := rows.Scan(&o.ID, &o.OrderNumber, &o.Status, &o.Channel, &count); err != nil {
			return nil, 0, fmt.Errorf("scan unfinished order: %w", err)
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read unfinished orders: %w", err)
	}
	return orders, count, nil
}
