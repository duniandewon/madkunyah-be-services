package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	insertCategory = `
		INSERT INTO categories (id, name, sort_order) VALUES ($1, $2, $3)
		RETURNING created_at, updated_at`

	updateCategory = `
		UPDATE categories SET name = $2, sort_order = $3, is_active = $4, updated_at = now()
		WHERE id = $1 RETURNING updated_at`

	insertProduct = `
		INSERT INTO products (id, category_id, sku, name, description, base_price, cost_price,
		                      is_web_visible, is_pos_visible, available_from, available_to, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::time, $11::time, $12)
		RETURNING created_at, updated_at`

	updateProduct = `
		UPDATE products SET category_id = $2, sku = $3, name = $4, description = $5, base_price = $6,
		       cost_price = $7, is_web_visible = $8, is_pos_visible = $9,
		       available_from = $10::time, available_to = $11::time, sort_order = $12, is_active = $13,
		       updated_at = now()
		WHERE id = $1 RETURNING updated_at`

	lockProduct = `SELECT 1 FROM products WHERE id = $1 FOR UPDATE`

	insertVariant = `
		INSERT INTO product_variants (id, product_id, name, price_delta, sku, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`

	getVariantForUpdate = `SELECT ` + variantColumns + ` FROM product_variants
		WHERE id = $1 AND product_id = $2 FOR UPDATE`

	updateVariant = `
		UPDATE product_variants SET name = $2, price_delta = $3, sku = $4, sort_order = $5, is_active = $6,
		       updated_at = now()
		WHERE id = $1 RETURNING updated_at`

	countOtherActiveVariants = `
		SELECT count(*) FROM product_variants WHERE product_id = $1 AND is_active AND id <> $2`

	insertGroup = `
		INSERT INTO modifier_groups (id, name, min_select, max_select) VALUES ($1, $2, $3, $4)
		RETURNING created_at, updated_at`

	updateGroup = `
		UPDATE modifier_groups SET name = $2, min_select = $3, max_select = $4, is_active = $5,
		       updated_at = now()
		WHERE id = $1 RETURNING updated_at`

	lockGroup = `SELECT 1 FROM modifier_groups WHERE id = $1 FOR UPDATE`

	// A product restore holds its groups against a concurrent modifier
	// archive or min_select change, which lock the group FOR UPDATE, so one
	// of the two sees the other's result in its satisfiability check.
	lockAttachedGroups = `
		SELECT 1 FROM modifier_groups
		WHERE id IN (SELECT modifier_group_id FROM product_modifier_groups WHERE product_id = $1)
		FOR SHARE`

	insertModifier = `
		INSERT INTO modifiers (id, modifier_group_id, name, price_delta, sort_order) VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`

	getModifierForUpdate = `SELECT ` + modifierColumns + ` FROM modifiers
		WHERE id = $1 AND modifier_group_id = $2 FOR UPDATE`

	updateModifier = `
		UPDATE modifiers SET name = $2, price_delta = $3, sort_order = $4, is_active = $5, updated_at = now()
		WHERE id = $1 RETURNING updated_at`

	// FOR SHARE holds the groups against a concurrent archive or min_select
	// change until the attachment commits.
	groupStates = `SELECT id::text, is_active FROM modifier_groups WHERE id = ANY($1) FOR SHARE`

	currentAttachments = `
		SELECT modifier_group_id::text FROM product_modifier_groups
		WHERE product_id = $1 ORDER BY sort_order, modifier_group_id`

	deleteAttachments = `DELETE FROM product_modifier_groups WHERE product_id = $1`

	insertAttachments = `
		INSERT INTO product_modifier_groups (product_id, modifier_group_id, sort_order)
		SELECT $1, g, (o - 1)::int FROM unnest($2::uuid[]) WITH ORDINALITY AS t(g, o)`

	// SKUs are unique across products and variants (FR-PRD-3). Each table's
	// own index covers itself; the advisory lock serializes the cross-table
	// check for one SKU.
	lockSKU = `SELECT pg_advisory_xact_lock(hashtextextended('catalog.sku:' || lower($1), 0))`

	skuTaken = `
		SELECT EXISTS (SELECT 1 FROM products WHERE lower(sku) = lower($1) AND id <> $2)
		    OR EXISTS (SELECT 1 FROM product_variants WHERE lower(sku) = lower($1) AND id <> $2)`

	// Active products attached to an active group that has fewer active
	// modifiers than its min_select (FR-MOD-5), among the given groups or
	// products. Run after a write, inside its transaction.
	unsatisfiableProducts = `
		SELECT p.id, p.name
		FROM product_modifier_groups pmg
		JOIN products p ON p.id = pmg.product_id AND p.is_active
		JOIN modifier_groups g ON g.id = pmg.modifier_group_id AND g.is_active
		WHERE (pmg.modifier_group_id = ANY($1) OR pmg.product_id = ANY($2))
		  AND g.min_select > (SELECT count(*) FROM modifiers m WHERE m.modifier_group_id = g.id AND m.is_active)
		GROUP BY p.id, p.name
		ORDER BY lower(p.name), p.id`

	groupInUse = `
		SELECT p.id, p.name
		FROM product_modifier_groups pmg
		JOIN products p ON p.id = pmg.product_id AND p.is_active
		WHERE pmg.modifier_group_id = $1
		ORDER BY lower(p.name), p.id`
)

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

var (
	errSKUTaken = domain.NewError(domain.CodeSKUTaken,
		"This SKU is already used by a product or variant. SKUs are never reused.")
	errLastActiveVariant = domain.NewError(domain.CodeLastActiveVariant,
		"A product with variants must keep at least one active variant. Archive the product instead.")
)

// nameScope words the name_taken message for each entity type.
var nameScope = map[string]string{
	entityCategory: "An active category",
	entityProduct:  "An active product in this category",
	entityVariant:  "An active variant of this product",
	entityModifier: "An active modifier in this group",
}

// writeErr maps constraint violations to client errors. A foreign key on
// the URL's parent is not_found; the product's category is a body field.
func writeErr(err error, entityType string) error {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if ok {
		switch {
		case pgErr.Code == pgUniqueViolation && strings.HasSuffix(pgErr.ConstraintName, "_sku"):
			return errSKUTaken
		case pgErr.Code == pgUniqueViolation && strings.HasSuffix(pgErr.ConstraintName, "_name_active"):
			return domain.NewError(domain.CodeNameTaken, nameScope[entityType]+" already has this name.")
		case pgErr.Code == pgForeignKeyViolation && pgErr.ConstraintName == "products_category_id_fkey":
			return domain.ValidationError(map[string]string{"category_id": "does not exist"})
		case pgErr.Code == pgForeignKeyViolation:
			return domain.ErrNotFound
		}
	}
	return fmt.Errorf("save %s: %w", entityType, err)
}

func claimSKU(ctx context.Context, tx pgx.Tx, sku *string, ownerID string) error {
	if sku == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, lockSKU, *sku); err != nil {
		return fmt.Errorf("lock sku: %w", err)
	}
	var taken bool
	if err := tx.QueryRow(ctx, skuTaken, *sku, ownerID).Scan(&taken); err != nil {
		return fmt.Errorf("check sku: %w", err)
	}
	if taken {
		return errSKUTaken
	}
	return nil
}

func affected(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]AffectedProduct, error) {
	scan := func(row rowScanner) (*AffectedProduct, error) {
		var p AffectedProduct
		return &p, row.Scan(&p.ID, &p.Name)
	}
	return collect(ctx, tx, scan, "affected products", query, args...)
}

// checkSatisfiable fails the transaction with modifier_group_unsatisfiable
// if any of the given groups or products is left unsatisfiable.
func checkSatisfiable(ctx context.Context, tx pgx.Tx, groupIDs, productIDs []string) error {
	products, err := affected(ctx, tx, unsatisfiableProducts, nonNil(groupIDs), nonNil(productIDs))
	if err != nil {
		return err
	}
	if len(products) == 0 {
		return nil
	}
	e := domain.NewError(domain.CodeModifierGroupUnsatisfiable,
		"A required modifier group would have fewer active modifiers than its minimum selection.")
	e.Details = map[string][]AffectedProduct{"products": products}
	return e
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func lockRow(ctx context.Context, tx pgx.Tx, query, id, what string) error {
	var found int
	err := tx.QueryRow(ctx, query, id).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", what, err)
	}
	return nil
}

func (r *Repository) CreateCategory(ctx context.Context, actorID string, c *Category) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, insertCategory, c.ID, c.Name, c.SortOrder).
			Scan(&c.CreatedAt, &c.UpdatedAt); err != nil {
			return writeErr(err, entityCategory)
		}
		return writeAudit(ctx, tx, actorID, created(entityCategory, c.ID, c.fields()))
	})
}

// UpdateCategory locks the category, applies fn and, if anything changed,
// saves and audits it. Unchanged edits write nothing.
func (r *Repository) UpdateCategory(ctx context.Context, actorID, id string, fn func(*Category) error) (*Category, error) {
	var c *Category
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		if c, err = one(tx.QueryRow(ctx, getCategory+" FOR UPDATE", id), scanCategory, "category"); err != nil {
			return err
		}
		before := c.fields()
		if err := fn(c); err != nil {
			return err
		}
		ch, ok := updated(entityCategory, id, before, c.fields())
		if !ok {
			return nil
		}
		if err := tx.QueryRow(ctx, updateCategory, id, c.Name, c.SortOrder, c.IsActive).
			Scan(&c.UpdatedAt); err != nil {
			return writeErr(err, entityCategory)
		}
		return writeAudit(ctx, tx, actorID, ch)
	})
	return c, err
}

func (r *Repository) CreateProduct(ctx context.Context, actorID string, p *Product) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := claimSKU(ctx, tx, p.SKU, p.ID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, insertProduct, p.ID, p.CategoryID, p.SKU, p.Name, p.Description,
			p.BasePrice, p.CostPrice.Value, p.IsWebVisible, p.IsPOSVisible, p.AvailableFrom, p.AvailableTo,
			p.SortOrder).Scan(&p.CreatedAt, &p.UpdatedAt); err != nil {
			return writeErr(err, entityProduct)
		}
		return writeAudit(ctx, tx, actorID, created(entityProduct, p.ID, p.fields()))
	})
}

// UpdateProduct is UpdateCategory for products. Restoring a product
// re-checks that its required groups can be satisfied.
func (r *Repository) UpdateProduct(ctx context.Context, actorID, id string, fn func(*Product) error) (*Product, error) {
	var p *Product
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		if p, err = one(tx.QueryRow(ctx, getProduct+" FOR UPDATE", id), scanProduct, "product"); err != nil {
			return err
		}
		before := p.fields()
		if err := fn(p); err != nil {
			return err
		}
		ch, ok := updated(entityProduct, id, before, p.fields())
		if !ok {
			return nil
		}
		if _, moved := ch.after["sku"]; moved {
			if err := claimSKU(ctx, tx, p.SKU, p.ID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, updateProduct, id, p.CategoryID, p.SKU, p.Name, p.Description, p.BasePrice,
			p.CostPrice.Value, p.IsWebVisible, p.IsPOSVisible, p.AvailableFrom, p.AvailableTo, p.SortOrder,
			p.IsActive).Scan(&p.UpdatedAt); err != nil {
			return writeErr(err, entityProduct)
		}
		if ch.after["is_active"] == true {
			if _, err := tx.Exec(ctx, lockAttachedGroups, id); err != nil {
				return fmt.Errorf("lock attached groups: %w", err)
			}
			if err := checkSatisfiable(ctx, tx, nil, []string{id}); err != nil {
				return err
			}
		}
		return writeAudit(ctx, tx, actorID, ch)
	})
	return p, err
}

func (r *Repository) CreateVariant(ctx context.Context, actorID string, v *Variant) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := claimSKU(ctx, tx, v.SKU, v.ID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, insertVariant, v.ID, v.ProductID, v.Name, v.PriceDelta, v.SKU, v.SortOrder).
			Scan(&v.CreatedAt, &v.UpdatedAt); err != nil {
			return writeErr(err, entityVariant)
		}
		return writeAudit(ctx, tx, actorID, created(entityVariant, v.ID, v.fields()))
	})
}

// UpdateVariant locks the product first, so two concurrent archives cannot
// both pass the last-active-variant check (FR-VAR-5).
func (r *Repository) UpdateVariant(ctx context.Context, actorID, productID, id string,
	fn func(*Variant) error) (*Variant, error) {
	var v *Variant
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := lockRow(ctx, tx, lockProduct, productID, "product"); err != nil {
			return err
		}
		var err error
		if v, err = one(tx.QueryRow(ctx, getVariantForUpdate, id, productID), scanVariant, "variant"); err != nil {
			return err
		}
		before := v.fields()
		if err := fn(v); err != nil {
			return err
		}
		ch, ok := updated(entityVariant, id, before, v.fields())
		if !ok {
			return nil
		}
		if ch.after["is_active"] == false {
			var others int
			if err := tx.QueryRow(ctx, countOtherActiveVariants, productID, id).Scan(&others); err != nil {
				return fmt.Errorf("count active variants: %w", err)
			}
			if others == 0 {
				return errLastActiveVariant
			}
		}
		if _, moved := ch.after["sku"]; moved {
			if err := claimSKU(ctx, tx, v.SKU, v.ID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, updateVariant, id, v.Name, v.PriceDelta, v.SKU, v.SortOrder, v.IsActive).
			Scan(&v.UpdatedAt); err != nil {
			return writeErr(err, entityVariant)
		}
		return writeAudit(ctx, tx, actorID, ch)
	})
	return v, err
}

func (r *Repository) CreateGroup(ctx context.Context, actorID string, g *ModifierGroup) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, insertGroup, g.ID, g.Name, g.MinSelect, g.MaxSelect).
			Scan(&g.CreatedAt, &g.UpdatedAt); err != nil {
			return writeErr(err, entityGroup)
		}
		return writeAudit(ctx, tx, actorID, created(entityGroup, g.ID, g.fields()))
	})
}

// UpdateGroup refuses to archive a group attached to active products
// (FR-MOD-7) and to raise min_select, or restore, beyond what its active
// modifiers can satisfy (FR-MOD-5).
func (r *Repository) UpdateGroup(ctx context.Context, actorID, id string,
	fn func(*ModifierGroup) error) (*ModifierGroup, error) {
	var g *ModifierGroup
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		if g, err = one(tx.QueryRow(ctx, getGroup+" FOR UPDATE", id), scanGroup, "modifier group"); err != nil {
			return err
		}
		before := g.fields()
		if err := fn(g); err != nil {
			return err
		}
		g.Required = g.MinSelect >= 1
		ch, ok := updated(entityGroup, id, before, g.fields())
		if !ok {
			return fillModifiers(ctx, tx, []*ModifierGroup{g}, true)
		}
		if ch.after["is_active"] == false {
			if err := checkNotInUse(ctx, tx, id); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, updateGroup, id, g.Name, g.MinSelect, g.MaxSelect, g.IsActive).
			Scan(&g.UpdatedAt); err != nil {
			return writeErr(err, entityGroup)
		}
		_, minChanged := ch.after["min_select"]
		if minChanged || ch.after["is_active"] == true {
			if err := checkSatisfiable(ctx, tx, []string{id}, nil); err != nil {
				return err
			}
		}
		if err := writeAudit(ctx, tx, actorID, ch); err != nil {
			return err
		}
		return fillModifiers(ctx, tx, []*ModifierGroup{g}, true)
	})
	return g, err
}

func checkNotInUse(ctx context.Context, tx pgx.Tx, groupID string) error {
	products, err := affected(ctx, tx, groupInUse, groupID)
	if err != nil {
		return err
	}
	if len(products) == 0 {
		return nil
	}
	e := domain.NewError(domain.CodeModifierGroupInUse,
		"This modifier group is attached to active products. Detach it first.")
	e.Details = map[string][]AffectedProduct{"products": products}
	return e
}

func (r *Repository) CreateModifier(ctx context.Context, actorID string, m *Modifier) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, insertModifier, m.ID, m.ModifierGroupID, m.Name, m.PriceDelta, m.SortOrder).
			Scan(&m.CreatedAt, &m.UpdatedAt); err != nil {
			return writeErr(err, entityModifier)
		}
		return writeAudit(ctx, tx, actorID, created(entityModifier, m.ID, m.fields()))
	})
}

// UpdateModifier locks the group first, so concurrent archives of two
// modifiers in one group are checked one after the other (FR-MOD-5).
func (r *Repository) UpdateModifier(ctx context.Context, actorID, groupID, id string,
	fn func(*Modifier) error) (*Modifier, error) {
	var m *Modifier
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := lockRow(ctx, tx, lockGroup, groupID, "modifier group"); err != nil {
			return err
		}
		var err error
		if m, err = one(tx.QueryRow(ctx, getModifierForUpdate, id, groupID), scanModifier, "modifier"); err != nil {
			return err
		}
		before := m.fields()
		if err := fn(m); err != nil {
			return err
		}
		ch, ok := updated(entityModifier, id, before, m.fields())
		if !ok {
			return nil
		}
		if err := tx.QueryRow(ctx, updateModifier, id, m.Name, m.PriceDelta, m.SortOrder, m.IsActive).
			Scan(&m.UpdatedAt); err != nil {
			return writeErr(err, entityModifier)
		}
		if ch.after["is_active"] == false {
			if err := checkSatisfiable(ctx, tx, []string{groupID}, nil); err != nil {
				return err
			}
		}
		return writeAudit(ctx, tx, actorID, ch)
	})
	return m, err
}

// SetProductGroups replaces a product's attached groups atomically
// (FR-MOD-6). groupIDs are canonical UUID strings without duplicates.
func (r *Repository) SetProductGroups(ctx context.Context, actorID, productID string,
	groupIDs []string) ([]AttachedGroup, error) {
	var out []AttachedGroup
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := lockRow(ctx, tx, lockProduct, productID, "product"); err != nil {
			return err
		}
		if err := checkAttachable(ctx, tx, groupIDs); err != nil {
			return err
		}
		current, err := collect(ctx, tx, scanString, "attachments", currentAttachments, productID)
		if err != nil {
			return err
		}
		if !slices.Equal(current, groupIDs) {
			if err := replaceAttachments(ctx, tx, actorID, productID, current, groupIDs); err != nil {
				return err
			}
		}
		out, err = attachedGroups(ctx, tx, productID)
		return err
	})
	return out, err
}

func replaceAttachments(ctx context.Context, tx pgx.Tx, actorID, productID string, current, groupIDs []string) error {
	if _, err := tx.Exec(ctx, deleteAttachments, productID); err != nil {
		return fmt.Errorf("detach groups: %w", err)
	}
	if _, err := tx.Exec(ctx, insertAttachments, productID, groupIDs); err != nil {
		return fmt.Errorf("attach groups: %w", err)
	}
	if err := checkSatisfiable(ctx, tx, nil, []string{productID}); err != nil {
		return err
	}
	return writeAudit(ctx, tx, actorID, change{
		entityType: entityProduct, entityID: productID, action: "product.groups_changed",
		before: fields{"modifier_group_ids": current}, after: fields{"modifier_group_ids": groupIDs},
	})
}

// checkAttachable rejects unknown and archived groups (FR-MOD-6).
func checkAttachable(ctx context.Context, tx pgx.Tx, groupIDs []string) error {
	if len(groupIDs) == 0 {
		return nil
	}
	scan := func(row rowScanner) (*groupState, error) {
		var s groupState
		return &s, row.Scan(&s.id, &s.active)
	}
	states, err := collect(ctx, tx, scan, "modifier groups", groupStates, groupIDs)
	if err != nil {
		return err
	}
	active := map[string]bool{}
	for _, s := range states {
		active[s.id] = s.active
	}
	for _, id := range groupIDs {
		if !active[id] {
			return domain.ValidationError(map[string]string{
				"modifier_group_ids": "unknown or archived modifier group: " + id,
			})
		}
	}
	return nil
}

type groupState struct {
	id     string
	active bool
}

func scanString(row rowScanner) (*string, error) {
	var s string
	return &s, row.Scan(&s)
}

// likeEscape makes a search term match literally inside ILIKE.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
