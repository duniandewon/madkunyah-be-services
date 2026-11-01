package catalog

import (
	"context"
	"fmt"
	"time"
)

const (
	listPublicOutlets = `
		SELECT id, name, address, phone, timezone FROM outlets
		WHERE is_active
		ORDER BY lower(name), id`

	storeOutlet = `
		SELECT id, name, timezone, to_char(business_day_cutoff, 'HH24:MI')
		FROM outlets WHERE id = $1 AND is_active`

	// Active products in active categories with the outlet's override for
	// the channel; visibility is resolved in Go (FR-OUT-2). Ordered as the
	// dashboard orders them (FR-CAT-5, FR-READ-2).
	storeProducts = `
		SELECT p.id, p.name, p.description, p.base_price, p.is_web_visible, p.is_pos_visible,
		       to_char(p.available_from, 'HH24:MI'), to_char(p.available_to, 'HH24:MI'),
		       c.id, c.name,
		       s.product_id IS NOT NULL, s.price, s.is_visible,
		       to_char(s.available_from, 'HH24:MI'), to_char(s.available_to, 'HH24:MI')
		FROM products p
		JOIN categories c ON c.id = p.category_id AND c.is_active
		LEFT JOIN product_channel_settings s ON s.product_id = p.id AND s.outlet_id = $1 AND s.channel = $2
		WHERE p.is_active
		ORDER BY c.sort_order, lower(c.name), c.id, p.sort_order, lower(p.name), p.id`

	storeVariants = `
		SELECT id, product_id, name, price_delta FROM product_variants
		WHERE is_active AND product_id = ANY($1)
		ORDER BY sort_order, lower(name), id`

	storeGroups = `
		SELECT pmg.product_id, g.id, g.name, g.min_select, g.max_select
		FROM product_modifier_groups pmg
		JOIN modifier_groups g ON g.id = pmg.modifier_group_id AND g.is_active
		WHERE pmg.product_id = ANY($1)
		ORDER BY pmg.sort_order, lower(g.name), g.id`

	storeModifiers = `
		SELECT id, modifier_group_id, name, price_delta FROM modifiers
		WHERE is_active AND modifier_group_id = ANY($1)
		ORDER BY sort_order, lower(name), id`

	// Every set row, expired or not: the storefront stays the same as time
	// passes, and readers evaluate expiry (FR-BND-3).
	storeSoldOut = `
		SELECT product_id::text, product_variant_id::text, modifier_id::text, sold_out_until
		FROM outlet_sold_out
		WHERE outlet_id = $1 AND is_sold_out
		ORDER BY coalesce(product_id, product_variant_id, modifier_id)`

	recordBundleFetch = `UPDATE devices SET catalog_version = $2, catalog_fetched_at = now() WHERE id = $1`
)

func (r *Repository) PublicOutlets(ctx context.Context) ([]PublicOutlet, error) {
	scan := func(row rowScanner) (*PublicOutlet, error) {
		var o PublicOutlet
		return &o, row.Scan(&o.ID, &o.Name, &o.Address, &o.Phone, &o.Timezone)
	}
	return collect(ctx, r.db.Pool, scan, "outlets", listPublicOutlets)
}

func (r *Repository) RecordBundleFetch(ctx context.Context, deviceID, version string) error {
	if _, err := r.db.Pool.Exec(ctx, recordBundleFetch, deviceID, version); err != nil {
		return fmt.Errorf("record bundle fetch: %w", err)
	}
	return nil
}

// storeBuild holds the storefront while it is assembled.
type storeBuild struct {
	sf       *Storefront
	ids      []string
	products map[string]*StoreProduct
	groups   map[string][]*StoreGroup // by group ID: one entry per product it is attached to
}

// Storefront builds the outlet's storefront for the channel from Postgres.
// An unknown or inactive outlet is not_found (FR-MENU-2).
func (r *Repository) Storefront(ctx context.Context, outletID string, ch Channel) (*Storefront, error) {
	sf := &Storefront{Categories: []StoreCategory{}, SoldOut: []SoldOutRule{}}
	o := &sf.Outlet
	if _, err := one(r.db.Pool.QueryRow(ctx, storeOutlet, outletID), func(row rowScanner) (*StoreOutlet, error) {
		return o, row.Scan(&o.ID, &o.Name, &o.Timezone, &o.BusinessDayCutoff)
	}, "outlet"); err != nil {
		return nil, err
	}
	b := &storeBuild{sf: sf, products: map[string]*StoreProduct{}, groups: map[string][]*StoreGroup{}}
	if err := b.loadProducts(ctx, r.db.Pool, outletID, ch); err != nil {
		return nil, err
	}
	b.index()
	steps := []func(context.Context, Querier) error{
		b.loadVariants, b.loadGroups, b.loadModifiers,
		func(ctx context.Context, q Querier) error { return b.loadSoldOut(ctx, q, outletID) },
	}
	for _, step := range steps {
		if err := step(ctx, r.db.Pool); err != nil {
			return nil, err
		}
	}
	return sf, nil
}

func (b *storeBuild) loadProducts(ctx context.Context, q Querier, outletID string, ch Channel) error {
	rows, err := q.Query(ctx, storeProducts, outletID, string(ch))
	if err != nil {
		return fmt.Errorf("load storefront products: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			p                    StoreProduct
			d                    = productDefaults{active: true, catActive: true}
			catID, catName       string
			from, to, oFrom, oTo *string
			hasOverride          bool
			o                    = &override{}
		)
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &d.basePrice, &d.webVisible, &d.posVisible,
			&from, &to, &catID, &catName, &hasOverride, &o.price, &o.visible, &oFrom, &oTo); err != nil {
			return fmt.Errorf("scan storefront product: %w", err)
		}
		d.window = newWindow(from, to)
		if !hasOverride {
			o = nil
		} else {
			o.window = newWindow(oFrom, oTo)
		}
		eff := resolveChannel(&d, ch, o)
		if !eff.visible {
			continue
		}
		p.Price, p.Window = eff.price, eff.window.public()
		p.Variants, p.ModifierGroups = []StoreVariant{}, []StoreGroup{}
		b.addProduct(catID, catName, p)
	}
	return rows.Err()
}

// addProduct appends to the last category when it is the same one: rows
// arrive grouped by category. Categories with no visible product never get
// an entry (FR-MENU-3).
func (b *storeBuild) addProduct(catID, catName string, p StoreProduct) {
	cats := b.sf.Categories
	if len(cats) == 0 || cats[len(cats)-1].ID != catID {
		b.sf.Categories = append(cats, StoreCategory{ID: catID, Name: catName})
	}
	last := &b.sf.Categories[len(b.sf.Categories)-1]
	last.Products = append(last.Products, p)
}

// index points products at their final slots. It runs once every product
// is in, since appending to a category may move its products.
func (b *storeBuild) index() {
	for ci := range b.sf.Categories {
		for pi := range b.sf.Categories[ci].Products {
			p := &b.sf.Categories[ci].Products[pi]
			b.products[p.ID] = p
			b.ids = append(b.ids, p.ID)
		}
	}
}

func (b *storeBuild) loadVariants(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, storeVariants, b.ids)
	if err != nil {
		return fmt.Errorf("load storefront variants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v StoreVariant
		var productID string
		if err := rows.Scan(&v.ID, &productID, &v.Name, &v.PriceDelta); err != nil {
			return fmt.Errorf("scan storefront variant: %w", err)
		}
		p := b.products[productID]
		p.Variants = append(p.Variants, v)
	}
	return rows.Err()
}

func (b *storeBuild) loadGroups(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, storeGroups, b.ids)
	if err != nil {
		return fmt.Errorf("load storefront groups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g StoreGroup
		var productID string
		if err := rows.Scan(&productID, &g.ID, &g.Name, &g.MinSelect, &g.MaxSelect); err != nil {
			return fmt.Errorf("scan storefront group: %w", err)
		}
		g.Required = g.MinSelect >= 1
		g.Modifiers = []StoreModifier{}
		p := b.products[productID]
		p.ModifierGroups = append(p.ModifierGroups, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Point at the groups in their final slices only now that appends are
	// done.
	for _, p := range b.products {
		for i := range p.ModifierGroups {
			g := &p.ModifierGroups[i]
			b.groups[g.ID] = append(b.groups[g.ID], g)
		}
	}
	return nil
}

func (b *storeBuild) loadModifiers(ctx context.Context, q Querier) error {
	if len(b.groups) == 0 {
		return nil
	}
	ids := make([]string, 0, len(b.groups))
	for id := range b.groups {
		ids = append(ids, id)
	}
	rows, err := q.Query(ctx, storeModifiers, ids)
	if err != nil {
		return fmt.Errorf("load storefront modifiers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m StoreModifier
		var groupID string
		if err := rows.Scan(&m.ID, &groupID, &m.Name, &m.PriceDelta); err != nil {
			return fmt.Errorf("scan storefront modifier: %w", err)
		}
		for _, g := range b.groups[groupID] {
			g.Modifiers = append(g.Modifiers, m)
		}
	}
	return rows.Err()
}

// loadSoldOut keeps the rows that target an item in the storefront.
func (b *storeBuild) loadSoldOut(ctx context.Context, q Querier, outletID string) error {
	present := map[string]bool{}
	for id, p := range b.products {
		present[id] = true
		for _, v := range p.Variants {
			present[v.ID] = true
		}
	}
	for _, gs := range b.groups {
		for _, m := range gs[0].Modifiers {
			present[m.ID] = true
		}
	}
	rows, err := q.Query(ctx, storeSoldOut, outletID)
	if err != nil {
		return fmt.Errorf("load storefront sold out: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r SoldOutRule
		var until *time.Time
		if err := rows.Scan(&r.ProductID, &r.VariantID, &r.ModifierID, &until); err != nil {
			return fmt.Errorf("scan storefront sold out: %w", err)
		}
		if !present[r.targetID()] {
			continue
		}
		if until != nil {
			u := until.UTC()
			r.SoldOutUntil = &u
		}
		b.sf.SoldOut = append(b.sf.SoldOut, r)
	}
	return rows.Err()
}
