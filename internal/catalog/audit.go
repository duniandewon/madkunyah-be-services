package catalog

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const insertAudit = `
	INSERT INTO audit_logs (id, actor_id, actor_type, action, entity_type, entity_id, value_delta, before, after)
	VALUES ($1, $2, 'staff', $3, $4, $5, $6, $7, $8)`

// Audit entity types (FR-CAUD-2).
const (
	entityCategory = "category"
	entityProduct  = "product"
	entityVariant  = "product_variant"
	entityGroup    = "modifier_group"
	entityModifier = "modifier"
)

// actionPrefix is the action namespace of each entity type.
var actionPrefix = map[string]string{
	entityCategory: "category",
	entityProduct:  "product",
	entityVariant:  "variant",
	entityGroup:    "modifier_group",
	entityModifier: "modifier",
}

// priceField is the one money column of each entity type, for value_delta.
var priceField = map[string]string{
	entityProduct:  "base_price",
	entityVariant:  "price_delta",
	entityModifier: "price_delta",
}

// fields is an entity as a flat column → value map, values being nil or
// comparable scalars so two snapshots can be diffed with !=.
type fields map[string]any

func val[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func (c *Category) fields() fields {
	return fields{"name": c.Name, "sort_order": c.SortOrder, "is_active": c.IsActive}
}

func (p *Product) fields() fields {
	return fields{
		"category_id": p.CategoryID, "sku": val(p.SKU), "name": p.Name, "description": val(p.Description),
		"base_price": p.BasePrice, "cost_price": val(p.CostPrice.Value),
		"is_web_visible": p.IsWebVisible, "is_pos_visible": p.IsPOSVisible,
		"available_from": val(p.AvailableFrom), "available_to": val(p.AvailableTo),
		"sort_order": p.SortOrder, "is_active": p.IsActive,
	}
}

func (v *Variant) fields() fields {
	return fields{"name": v.Name, "price_delta": v.PriceDelta, "sku": val(v.SKU),
		"sort_order": v.SortOrder, "is_active": v.IsActive}
}

func (g *ModifierGroup) fields() fields {
	return fields{"name": g.Name, "min_select": g.MinSelect, "max_select": g.MaxSelect, "is_active": g.IsActive}
}

func (m *Modifier) fields() fields {
	return fields{"name": m.Name, "price_delta": m.PriceDelta, "sort_order": m.SortOrder, "is_active": m.IsActive}
}

// diff returns only the fields that changed, as before and after maps.
func diff(before, after fields) (fields, fields) {
	b, a := fields{}, fields{}
	for k, v := range after {
		if before[k] != v {
			b[k], a[k] = before[k], v
		}
	}
	return b, a
}

// change is one audited catalog write.
type change struct {
	entityType string
	entityID   string
	action     string
	before     fields
	after      fields
}

// created audits a new entity: no before, every field after.
func created(entityType, id string, f fields) change {
	return change{entityType: entityType, entityID: id, action: actionPrefix[entityType] + ".created", after: f}
}

// updated audits the changed fields of an edit, naming an is_active flip
// as archived or restored. It reports false when nothing changed
// (FR-CAUD-3).
func updated(entityType, id string, before, after fields) (change, bool) {
	b, a := diff(before, after)
	if len(a) == 0 {
		return change{}, false
	}
	action := ".updated"
	if active, ok := a["is_active"]; ok {
		action = ".restored"
		if active == false {
			action = ".archived"
		}
	}
	return change{entityType: entityType, entityID: id, action: actionPrefix[entityType] + action,
		before: b, after: a}, true
}

// writeAudit records a change in the caller's transaction (FR-CAUD-1),
// with value_delta when an edit moved the entity's price. A creation sets a
// price rather than changing one, so it carries none.
func writeAudit(ctx context.Context, tx pgx.Tx, actorID string, c change) error {
	var delta *int64
	if f, ok := priceField[c.entityType]; ok {
		newV, okNew := c.after[f].(int64)
		oldV, okOld := c.before[f].(int64)
		if okNew && okOld {
			d := newV - oldV
			delta = &d
		}
	}
	_, err := tx.Exec(ctx, insertAudit, uuid.NewString(), actorID, c.action, c.entityType, c.entityID,
		delta, jsonOrNull(c.before), jsonOrNull(c.after))
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

func jsonOrNull(f fields) any {
	if f == nil {
		return nil
	}
	return f
}
