package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/jackc/pgx/v5"
)

const (
	categoryColumns = `id, name, sort_order, is_active, created_at, updated_at`

	productColumns = `
		p.id, p.category_id, p.sku, p.name, p.description, p.base_price, p.cost_price,
		p.is_web_visible, p.is_pos_visible,
		to_char(p.available_from, 'HH24:MI'), to_char(p.available_to, 'HH24:MI'),
		p.sort_order, p.is_active, p.created_at, p.updated_at`

	variantColumns = `id, product_id, name, price_delta, sku, sort_order, is_active, created_at, updated_at`

	groupColumns = `id, name, min_select, max_select, is_active, created_at, updated_at`

	modifierColumns = `id, modifier_group_id, name, price_delta, sort_order, is_active, created_at, updated_at`

	listCategories = `SELECT ` + categoryColumns + ` FROM categories
		WHERE $1 OR is_active
		ORDER BY sort_order, lower(name), id`

	getCategory = `SELECT ` + categoryColumns + ` FROM categories WHERE id = $1`

	// Ordered by category order, then product order and name (FR-READ-2).
	// The search term is a LIKE pattern escaped by the caller.
	listProducts = `SELECT ` + productColumns + `
		FROM products p
		JOIN categories c ON c.id = p.category_id
		WHERE ($1 OR p.is_active)
		  AND ($2::uuid IS NULL OR p.category_id = $2)
		  AND ($3 = '' OR p.name ILIKE '%' || $3 || '%')
		ORDER BY c.sort_order, lower(c.name), c.id, p.sort_order, lower(p.name), p.id`

	getProduct = `SELECT ` + productColumns + ` FROM products p WHERE p.id = $1`

	listVariants = `SELECT ` + variantColumns + ` FROM product_variants
		WHERE product_id = $1
		ORDER BY sort_order, lower(name), id`

	listAttachedGroups = `
		SELECT g.id, g.name, g.min_select, g.max_select, g.is_active, g.created_at, g.updated_at,
		       pmg.sort_order
		FROM product_modifier_groups pmg
		JOIN modifier_groups g ON g.id = pmg.modifier_group_id
		WHERE pmg.product_id = $1
		ORDER BY pmg.sort_order, lower(g.name), g.id`

	listGroups = `SELECT ` + groupColumns + ` FROM modifier_groups
		WHERE $1 OR is_active
		ORDER BY lower(name), id`

	getGroup = `SELECT ` + groupColumns + ` FROM modifier_groups WHERE id = $1`

	listGroupModifiers = `SELECT ` + modifierColumns + ` FROM modifiers
		WHERE modifier_group_id = ANY($1) AND ($2 OR is_active)
		ORDER BY sort_order, lower(name), id`
)

type Repository struct {
	db *postgres.DB
}

func NewRepository(db *postgres.DB) *Repository { return &Repository{db: db} }

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCategory(row rowScanner) (*Category, error) {
	var c Category
	err := row.Scan(&c.ID, &c.Name, &c.SortOrder, &c.IsActive, &c.CreatedAt, &c.UpdatedAt)
	return &c, err
}

func scanProduct(row rowScanner) (*Product, error) {
	var p Product
	err := row.Scan(&p.ID, &p.CategoryID, &p.SKU, &p.Name, &p.Description, &p.BasePrice, &p.CostPrice.Value,
		&p.IsWebVisible, &p.IsPOSVisible, &p.AvailableFrom, &p.AvailableTo,
		&p.SortOrder, &p.IsActive, &p.CreatedAt, &p.UpdatedAt)
	return &p, err
}

func scanVariant(row rowScanner) (*Variant, error) {
	var v Variant
	err := row.Scan(&v.ID, &v.ProductID, &v.Name, &v.PriceDelta, &v.SKU, &v.SortOrder, &v.IsActive,
		&v.CreatedAt, &v.UpdatedAt)
	return &v, err
}

func scanGroup(row rowScanner) (*ModifierGroup, error) {
	var g ModifierGroup
	err := row.Scan(&g.ID, &g.Name, &g.MinSelect, &g.MaxSelect, &g.IsActive, &g.CreatedAt, &g.UpdatedAt)
	g.Required = g.MinSelect >= 1
	g.Modifiers = []Modifier{}
	return &g, err
}

func scanModifier(row rowScanner) (*Modifier, error) {
	var m Modifier
	err := row.Scan(&m.ID, &m.ModifierGroupID, &m.Name, &m.PriceDelta, &m.SortOrder, &m.IsActive,
		&m.CreatedAt, &m.UpdatedAt)
	return &m, err
}

// collect runs a query and scans every row.
func collect[T any](ctx context.Context, q Querier, scan func(rowScanner) (*T, error),
	what, query string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", what, err)
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", what, err)
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	return out, nil
}

// one scans a single row, mapping no rows to not_found.
func one[T any](row pgx.Row, scan func(rowScanner) (*T, error), what string) (*T, error) {
	v, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", what, err)
	}
	return v, nil
}

func (r *Repository) ListCategories(ctx context.Context, includeArchived bool) ([]Category, error) {
	return collect(ctx, r.db.Pool, scanCategory, "categories", listCategories, includeArchived)
}

func (r *Repository) GetCategory(ctx context.Context, id string) (*Category, error) {
	return one(r.db.Pool.QueryRow(ctx, getCategory, id), scanCategory, "category")
}

func (r *Repository) ListProducts(ctx context.Context, f ProductFilter) ([]Product, error) {
	return collect(ctx, r.db.Pool, scanProduct, "products", listProducts,
		f.IncludeArchived, f.CategoryID, likeEscape(f.Query))
}

func (r *Repository) GetProduct(ctx context.Context, id string) (*ProductDetail, error) {
	p, err := one(r.db.Pool.QueryRow(ctx, getProduct, id), scanProduct, "product")
	if err != nil {
		return nil, err
	}
	d := &ProductDetail{Product: *p}
	if d.Variants, err = collect(ctx, r.db.Pool, scanVariant, "variants", listVariants, id); err != nil {
		return nil, err
	}
	if d.ModifierGroups, err = attachedGroups(ctx, r.db.Pool, id); err != nil {
		return nil, err
	}
	return d, nil
}

func (r *Repository) ListGroups(ctx context.Context, includeArchived bool) ([]ModifierGroup, error) {
	groups, err := collect(ctx, r.db.Pool, scanGroup, "modifier groups", listGroups, includeArchived)
	if err != nil {
		return nil, err
	}
	ptrs := make([]*ModifierGroup, len(groups))
	for i := range groups {
		ptrs[i] = &groups[i]
	}
	if err := fillModifiers(ctx, r.db.Pool, ptrs, includeArchived); err != nil {
		return nil, err
	}
	return groups, nil
}

// GetGroup returns a group with all its modifiers, archived ones included.
func (r *Repository) GetGroup(ctx context.Context, id string) (*ModifierGroup, error) {
	g, err := one(r.db.Pool.QueryRow(ctx, getGroup, id), scanGroup, "modifier group")
	if err != nil {
		return nil, err
	}
	if err := fillModifiers(ctx, r.db.Pool, []*ModifierGroup{g}, true); err != nil {
		return nil, err
	}
	return g, nil
}

// attachedGroups loads a product's groups in its order, each with all its
// modifiers, archived ones included (FR-READ-3).
func attachedGroups(ctx context.Context, q Querier, productID string) ([]AttachedGroup, error) {
	scan := func(row rowScanner) (*AttachedGroup, error) {
		var a AttachedGroup
		g := &a.ModifierGroup
		err := row.Scan(&g.ID, &g.Name, &g.MinSelect, &g.MaxSelect, &g.IsActive, &g.CreatedAt, &g.UpdatedAt,
			&a.SortOrder)
		g.Required = g.MinSelect >= 1
		g.Modifiers = []Modifier{}
		return &a, err
	}
	groups, err := collect(ctx, q, scan, "attached groups", listAttachedGroups, productID)
	if err != nil {
		return nil, err
	}
	ptrs := make([]*ModifierGroup, len(groups))
	for i := range groups {
		ptrs[i] = &groups[i].ModifierGroup
	}
	if err := fillModifiers(ctx, q, ptrs, true); err != nil {
		return nil, err
	}
	return groups, nil
}

// fillModifiers loads the modifiers of every group in one query.
func fillModifiers(ctx context.Context, q Querier, groups []*ModifierGroup, includeArchived bool) error {
	if len(groups) == 0 {
		return nil
	}
	byID := make(map[string]*ModifierGroup, len(groups))
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		byID[g.ID] = g
		ids = append(ids, g.ID)
	}
	mods, err := collect(ctx, q, scanModifier, "modifiers", listGroupModifiers, ids, includeArchived)
	if err != nil {
		return err
	}
	for _, m := range mods {
		g := byID[m.ModifierGroupID]
		g.Modifiers = append(g.Modifiers, m)
	}
	return nil
}
