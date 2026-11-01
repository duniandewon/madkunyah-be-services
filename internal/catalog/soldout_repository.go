package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	soldOutOutlet = `
		SELECT timezone, to_char(business_day_cutoff, 'HH24:MI')
		FROM outlets WHERE id = $1 AND is_active`

	// Only one of the three target columns is set, and IDs are UUIDs, so
	// the row is found by its target ID alone.
	getSoldOut = `
		SELECT id, is_sold_out, sold_out_until, changed_at, changed_by, changed_via, device_id
		FROM outlet_sold_out
		WHERE outlet_id = $1 AND coalesce(product_id, product_variant_id, modifier_id) = $2
		FOR UPDATE`

	// Serializes toggles of one item at one outlet, including the first,
	// when there is no row to lock yet.
	lockSoldOut = `SELECT pg_advisory_xact_lock(hashtextextended('catalog.sold_out:' || $1::text || ':' || $2::text, 0))`

	insertSoldOut = `
		INSERT INTO outlet_sold_out (id, outlet_id, product_id, product_variant_id, modifier_id, is_sold_out,
		                             sold_out_until, changed_at, changed_by, changed_via, device_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	updateSoldOut = `
		UPDATE outlet_sold_out
		SET is_sold_out = $2, sold_out_until = $3, changed_at = $4, changed_by = $5, changed_via = $6,
		    device_id = $7, updated_at = now()
		WHERE id = $1`

	listSoldOut = `
		SELECT so.product_id::text, so.product_variant_id::text, so.modifier_id::text,
		       coalesce(p.name, v.name, m.name), vp.name,
		       so.sold_out_until, so.changed_at, so.changed_by, st.full_name, so.changed_via
		FROM outlet_sold_out so
		JOIN staff st ON st.id = so.changed_by
		LEFT JOIN products p ON p.id = so.product_id
		LEFT JOIN product_variants v ON v.id = so.product_variant_id
		LEFT JOIN products vp ON vp.id = v.product_id
		LEFT JOIN modifiers m ON m.id = so.modifier_id
		WHERE so.outlet_id = $1 AND so.is_sold_out
		  AND (so.sold_out_until IS NULL OR so.sold_out_until > $2)
		  AND coalesce(p.is_active, v.is_active, m.is_active)
		ORDER BY so.changed_at DESC, so.id`
)

// targetActive holds, per entity type, the query telling whether the item
// exists and is active (FR-SO-7).
var targetActive = map[string]string{
	entityProduct:  `SELECT is_active FROM products WHERE id = $1`,
	entityVariant:  `SELECT is_active FROM product_variants WHERE id = $1`,
	entityModifier: `SELECT is_active FROM modifiers WHERE id = $1`,
}

func checkTarget(ctx context.Context, tx pgx.Tx, entity, id string) error {
	var active bool
	err := tx.QueryRow(ctx, targetActive[entity], id).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check sold-out target: %w", err)
	}
	return nil
}

// SetSoldOut applies decideSoldOut to the item's row under a lock and, if
// it changed, saves and audits it in one transaction.
func (r *Repository) SetSoldOut(ctx context.Context, w *SoldOutWrite, now time.Time) (*SoldOutResult, error) {
	var res *SoldOutResult
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var tz, cutoff string
		err := tx.QueryRow(ctx, soldOutOutlet, w.OutletID).Scan(&tz, &cutoff)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get outlet: %w", err)
		}
		loc, err := location(tz)
		if err != nil {
			return fmt.Errorf("outlet timezone %q: %w", tz, err)
		}
		minutes, _ := parseClock(cutoff)
		if err := checkTarget(ctx, tx, w.Entity, w.ItemID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, lockSoldOut, w.OutletID, w.ItemID); err != nil {
			return fmt.Errorf("lock sold out: %w", err)
		}
		cur, err := getSoldOutRow(ctx, tx, w.OutletID, w.ItemID)
		if err != nil {
			return err
		}
		next, applied := decideSoldOut(w, cur, now, loc, minutes)
		if !applied {
			res = &SoldOutResult{SoldOutState: cur.state(w.Entity, w.ItemID, now)}
			return nil
		}
		if err := saveSoldOut(ctx, tx, w, next); err != nil {
			return err
		}
		res = &SoldOutResult{Applied: true, SoldOutState: next.state(w.Entity, w.ItemID, now)}
		return writeAudit(ctx, tx, w.ActorID, soldOutChange(w, cur, next))
	})
	return res, err
}

func getSoldOutRow(ctx context.Context, tx pgx.Tx, outletID, itemID string) (*soldOutRow, error) {
	r := &soldOutRow{found: true}
	err := tx.QueryRow(ctx, getSoldOut, outletID, itemID).Scan(&r.id, &r.isSoldOut, &r.until, &r.changedAt,
		&r.changedBy, &r.changedVia, &r.deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &soldOutRow{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sold out: %w", err)
	}
	return r, nil
}

func saveSoldOut(ctx context.Context, tx pgx.Tx, w *SoldOutWrite, next *soldOutRow) error {
	if next.id != "" {
		if _, err := tx.Exec(ctx, updateSoldOut, next.id, next.isSoldOut, next.until, next.changedAt,
			next.changedBy, next.changedVia, next.deviceID); err != nil {
			return fmt.Errorf("update sold out: %w", err)
		}
		return nil
	}
	next.id = uuid.NewString()
	target := map[string]*string{w.Entity: &w.ItemID}
	if _, err := tx.Exec(ctx, insertSoldOut, next.id, w.OutletID, target[entityProduct], target[entityVariant],
		target[entityModifier], next.isSoldOut, next.until, next.changedAt, next.changedBy, next.changedVia,
		next.deviceID); err != nil {
		return fmt.Errorf("insert sold out: %w", err)
	}
	return nil
}

// soldOutChange audits a toggle against the item, scoped to the outlet. A
// POS toggle also records the device and the client's changed_at
// (FR-OAUD-3).
func soldOutChange(w *SoldOutWrite, cur, next *soldOutRow) change {
	b, a := diff(cur.fields(), next.fields())
	if w.Via == ViaPOS {
		a["device_id"] = val(w.DeviceID)
		a["changed_at"] = w.ChangedAt.UTC().Format(time.RFC3339Nano)
	}
	action := "sold_out.set"
	if !w.SoldOut {
		action = "sold_out.cleared"
	}
	outletID := w.OutletID
	return change{outletID: &outletID, entityType: w.Entity, entityID: w.ItemID, action: action, before: b, after: a}
}

func (r *Repository) ListSoldOut(ctx context.Context, outletID string, at time.Time) ([]SoldOutEntry, error) {
	if err := checkOutlet(ctx, r.db.Pool, outletID); err != nil {
		return nil, err
	}
	scan := func(row rowScanner) (*SoldOutEntry, error) {
		var e SoldOutEntry
		err := row.Scan(&e.Item.ProductID, &e.Item.VariantID, &e.Item.ModifierID, &e.Name, &e.ProductName,
			&e.SoldOutUntil, &e.ChangedAt, &e.ChangedBy.ID, &e.ChangedBy.FullName, &e.ChangedVia)
		if e.Item.VariantID == nil {
			e.ProductName = nil
		}
		return &e, err
	}
	return collect(ctx, r.db.Pool, scan, "sold out items", listSoldOut, outletID, at)
}
