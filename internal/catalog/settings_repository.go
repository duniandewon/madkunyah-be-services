package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/jackc/pgx/v5"
)

const (
	outletActive = `SELECT 1 FROM outlets WHERE id = $1 AND is_active`

	outletProductColumns = productColumns + `, c.is_active`

	listOutletProducts = `SELECT ` + outletProductColumns + `
		FROM products p
		JOIN categories c ON c.id = p.category_id
		WHERE $1 OR p.is_active
		ORDER BY c.sort_order, lower(c.name), c.id, p.sort_order, lower(p.name), p.id`

	getOutletProduct = `SELECT ` + outletProductColumns + `
		FROM products p
		JOIN categories c ON c.id = p.category_id
		WHERE p.id = $1`

	settingsColumns = `price, is_visible, to_char(available_from, 'HH24:MI'), to_char(available_to, 'HH24:MI'),
		updated_by, updated_at`

	listOutletSettings = `SELECT product_id, channel, ` + settingsColumns + `
		FROM product_channel_settings WHERE outlet_id = $1`

	getSettings = `SELECT ` + settingsColumns + ` FROM product_channel_settings
		WHERE product_id = $1 AND outlet_id = $2 AND channel = $3`

	// A missing row cannot be locked, so writers of one (product, outlet,
	// channel) serialize on an advisory lock instead.
	lockSettings = `SELECT pg_advisory_xact_lock(hashtextextended(
		'catalog.channel:' || $1::text || ':' || $2::text || ':' || $3::text, 0))`

	upsertSettings = `
		INSERT INTO product_channel_settings
		       (product_id, outlet_id, channel, price, is_visible, available_from, available_to, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6::time, $7::time, $8)
		ON CONFLICT (product_id, outlet_id, channel) DO UPDATE
		SET price = EXCLUDED.price, is_visible = EXCLUDED.is_visible,
		    available_from = EXCLUDED.available_from, available_to = EXCLUDED.available_to,
		    updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING updated_at`

	deleteSettings = `DELETE FROM product_channel_settings WHERE product_id = $1 AND outlet_id = $2 AND channel = $3`
)

// Audit actions for outlet settings (FR-OAUD-2).
const (
	actionOutletProductUpdated = "outlet_product.updated"
	actionOutletProductReset   = "outlet_product.reset"
)

func scanOutletRow(row rowScanner) (*outletRow, error) {
	var o outletRow
	p := &o.product
	err := row.Scan(&p.ID, &p.CategoryID, &p.SKU, &p.Name, &p.Description, &p.BasePrice, &p.CostPrice.Value,
		&p.IsWebVisible, &p.IsPOSVisible, &p.AvailableFrom, &p.AvailableTo,
		&p.SortOrder, &p.IsActive, &p.CreatedAt, &p.UpdatedAt, &o.catActive)
	return &o, err
}

func scanSettings(row rowScanner, s *ChannelSettings, extra ...any) error {
	return row.Scan(append(extra, &s.Price, &s.IsVisible, &s.AvailableFrom, &s.AvailableTo,
		&s.UpdatedBy, &s.UpdatedAt)...)
}

// checkOutlet fails with not_found unless the outlet exists and is active
// (FR-OUT-8).
func checkOutlet(ctx context.Context, q Querier, outletID string) error {
	var found int
	err := q.QueryRow(ctx, outletActive, outletID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check outlet: %w", err)
	}
	return nil
}

func (r *Repository) OutletCatalog(ctx context.Context, outletID string, includeArchived bool) ([]outletRow, error) {
	if err := checkOutlet(ctx, r.db.Pool, outletID); err != nil {
		return nil, err
	}
	products, err := collect(ctx, r.db.Pool, scanOutletRow, "outlet products", listOutletProducts, includeArchived)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*outletRow, len(products))
	for i := range products {
		byID[products[i].product.ID] = &products[i]
	}
	rows, err := r.db.Pool.Query(ctx, listOutletSettings, outletID)
	if err != nil {
		return nil, fmt.Errorf("list outlet settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var productID string
		var ch Channel
		s := &ChannelSettings{}
		if err := scanSettings(rows, s, &productID, &ch); err != nil {
			return nil, fmt.Errorf("scan outlet settings: %w", err)
		}
		p := byID[productID]
		if p == nil {
			continue // an archived product, not asked for
		}
		if ch == ChannelWeb {
			p.web = s
		} else {
			p.pos = s
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read outlet settings: %w", err)
	}
	return products, nil
}

// lockedSettings checks the outlet and product, takes the settings lock and
// returns the product with the current row, nil if there is none.
func lockedSettings(ctx context.Context, tx pgx.Tx, k settingsKey) (*outletRow, *ChannelSettings, error) {
	if err := checkOutlet(ctx, tx, k.outletID); err != nil {
		return nil, nil, err
	}
	p, err := one(tx.QueryRow(ctx, getOutletProduct, k.productID), scanOutletRow, "product")
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, lockSettings, k.productID, k.outletID, string(k.channel)); err != nil {
		return nil, nil, fmt.Errorf("lock outlet settings: %w", err)
	}
	cur := &ChannelSettings{}
	err = scanSettings(tx.QueryRow(ctx, getSettings, k.productID, k.outletID, string(k.channel)), cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get outlet settings: %w", err)
	}
	return p, cur, nil
}

// UpdateChannelSettings applies fn to the current row (or an empty one)
// and, if anything changed, saves and audits it in one transaction.
func (r *Repository) UpdateChannelSettings(ctx context.Context, actorID string, k settingsKey,
	fn func(*ChannelSettings) error) (*ChannelResult, error) {
	var res *ChannelResult
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		p, cur, err := lockedSettings(ctx, tx, k)
		if err != nil {
			return err
		}
		next := &ChannelSettings{}
		if cur != nil {
			*next = *cur
		}
		before := next.fields()
		if err := fn(next); err != nil {
			return err
		}
		res = &ChannelResult{ProductID: k.productID, OutletID: k.outletID, Channel: k.channel}
		b, a := diff(before, next.fields())
		if len(a) == 0 {
			res.ChannelView = channelView(&p.product, p.catActive, k.channel, cur)
			return nil
		}
		next.UpdatedBy = actorID
		if err := tx.QueryRow(ctx, upsertSettings, k.productID, k.outletID, string(k.channel), next.Price,
			next.IsVisible, next.AvailableFrom, next.AvailableTo, actorID).Scan(&next.UpdatedAt); err != nil {
			return fmt.Errorf("save outlet settings: %w", err)
		}
		res.ChannelView = channelView(&p.product, p.catActive, k.channel, next)
		ch := settingsChange(k, actionOutletProductUpdated, b, a)
		if _, moved := a["price"]; moved {
			d := effectivePrice(next, p.product.BasePrice) - effectivePrice(cur, p.product.BasePrice)
			ch.delta = &d
		}
		return writeAudit(ctx, tx, actorID, ch)
	})
	return res, err
}

// ResetChannelSettings deletes the row after check approves it. Resetting
// a missing or all-null row changes nothing and audits nothing.
func (r *Repository) ResetChannelSettings(ctx context.Context, actorID string, k settingsKey,
	check func(*ChannelSettings) error) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		p, cur, err := lockedSettings(ctx, tx, k)
		if err != nil || cur == nil {
			return err
		}
		if err := check(cur); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, deleteSettings, k.productID, k.outletID, string(k.channel)); err != nil {
			return fmt.Errorf("delete outlet settings: %w", err)
		}
		b, a := diff(cur.fields(), (&ChannelSettings{}).fields())
		if len(a) == 0 {
			return nil
		}
		ch := settingsChange(k, actionOutletProductReset, b, a)
		if cur.Price != nil {
			d := p.product.BasePrice - *cur.Price
			ch.delta = &d
		}
		return writeAudit(ctx, tx, actorID, ch)
	})
}

// settingsChange audits a settings write against the product, scoped to the
// outlet, with the channel alongside the changed fields.
func settingsChange(k settingsKey, action string, before, after fields) change {
	before["channel"], after["channel"] = string(k.channel), string(k.channel)
	outletID := k.outletID
	return change{outletID: &outletID, entityType: entityProduct, entityID: k.productID, action: action,
		before: before, after: after}
}
