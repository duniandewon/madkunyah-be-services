package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Querier is the caller's transaction (or pool). Quote reads through it so
// the price it checks is the price the caller commits (FR-QTE-1).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Channel string

const (
	ChannelWeb Channel = "web"
	ChannelPOS Channel = "pos"
)

type QuoteMode int

const (
	// Strict fails any line with a problem: web checkout, online POS orders.
	Strict QuoteMode = iota
	// Record prices a sale that already happened (offline POS replay): it
	// returns a snapshot from the current catalog alongside the problems.
	Record
)

type QuoteRequest struct {
	OutletID uuid.UUID
	Channel  Channel
	// At is the fulfilment time the availability window is evaluated at.
	At    time.Time
	Mode  QuoteMode
	Lines []QuoteLine
}

type QuoteLine struct {
	ProductID   uuid.UUID
	VariantID   *uuid.UUID
	ModifierIDs []uuid.UUID
}

// ProblemKind is a stable reason a line cannot be sold as requested. The
// orders feature maps these to HTTP errors.
type ProblemKind string

const (
	ProblemProductUnavailable   ProblemKind = "product_unavailable"
	ProblemOutsideWindow        ProblemKind = "outside_window"
	ProblemVariantRequired      ProblemKind = "variant_required"
	ProblemVariantInvalid       ProblemKind = "variant_invalid"
	ProblemModifierInvalid      ProblemKind = "modifier_invalid"
	ProblemModifierCountInvalid ProblemKind = "modifier_count_invalid"
	ProblemProductSoldOut       ProblemKind = "product_sold_out"
	ProblemVariantSoldOut       ProblemKind = "variant_sold_out"
	ProblemModifierSoldOut      ProblemKind = "modifier_sold_out"
)

// Problem names the offending variant, modifier or modifier group in ID, or
// uuid.Nil when the problem is the product itself.
type Problem struct {
	Kind ProblemKind
	ID   uuid.UUID
}

// LineSnapshot is what an order line copies at sale time (FR-QTE-4).
type LineSnapshot struct {
	ProductID    uuid.UUID
	ProductName  string
	CategoryID   uuid.UUID
	CategoryName string
	VariantID    *uuid.UUID
	VariantName  *string
	// BasePrice is the product price at the outlet on the channel: the
	// outlet override when set, else products.base_price.
	BasePrice      int64
	VariantDelta   int64
	ModifiersDelta int64
	// UnitPrice = BasePrice + VariantDelta + ModifiersDelta, tax-exclusive.
	UnitPrice int64
	// UnitCost is products.cost_price, nil when unknown.
	UnitCost  *int64
	Modifiers []ModifierSnapshot
}

type ModifierSnapshot struct {
	ID         uuid.UUID
	Name       string
	PriceDelta int64
}

// LineResult carries a snapshot, problems, or both (Record mode). In Strict
// mode a line with problems has no snapshot. A product that does not exist
// never has one.
type LineResult struct {
	Snapshot *LineSnapshot
	Problems []Problem
}

type QuoteResult struct {
	Lines []LineResult
}

// Valid reports whether no line has a problem.
func (r *QuoteResult) Valid() bool {
	for _, l := range r.Lines {
		if len(l.Problems) > 0 {
			return false
		}
	}
	return true
}

// ErrOutletUnavailable fails the whole quote: the outlet is unknown or
// inactive (FR-QTE-2).
var ErrOutletUnavailable = errors.New("catalog: outlet unavailable")

const (
	quoteOutlet = `SELECT timezone FROM outlets WHERE id = $1 AND is_active`

	// Each query takes the outlet's override for the channel and the
	// outlet's sold-out rows along, so overrides and sold-out cost no extra
	// round trip.
	quoteProducts = `
		SELECT p.id, p.name, p.base_price, p.cost_price, p.is_active,
		       p.is_web_visible, p.is_pos_visible,
		       to_char(p.available_from, 'HH24:MI'), to_char(p.available_to, 'HH24:MI'),
		       c.id, c.name, c.is_active,
		       s.product_id IS NOT NULL, s.price, s.is_visible,
		       to_char(s.available_from, 'HH24:MI'), to_char(s.available_to, 'HH24:MI'),
		       so.id IS NOT NULL, so.sold_out_until
		FROM products p
		JOIN categories c ON c.id = p.category_id
		LEFT JOIN product_channel_settings s ON s.product_id = p.id AND s.outlet_id = $2 AND s.channel = $3
		LEFT JOIN outlet_sold_out so ON so.product_id = p.id AND so.outlet_id = $2 AND so.is_sold_out
		WHERE p.id = ANY($1)`

	quoteVariants = `
		SELECT v.id, v.product_id, v.name, v.price_delta, v.is_active, so.id IS NOT NULL, so.sold_out_until
		FROM product_variants v
		LEFT JOIN outlet_sold_out so ON so.product_variant_id = v.id AND so.outlet_id = $2 AND so.is_sold_out
		WHERE v.product_id = ANY($1)`

	quoteGroups = `
		SELECT pmg.product_id, g.id, g.min_select, g.max_select, g.is_active
		FROM product_modifier_groups pmg
		JOIN modifier_groups g ON g.id = pmg.modifier_group_id
		WHERE pmg.product_id = ANY($1)`

	quoteModifiers = `
		SELECT m.id, m.modifier_group_id, m.name, m.price_delta, m.is_active, so.id IS NOT NULL, so.sold_out_until
		FROM modifiers m
		LEFT JOIN outlet_sold_out so ON so.modifier_id = m.id AND so.outlet_id = $2 AND so.is_sold_out
		WHERE m.id = ANY($1)`
)

// Quote validates and prices order lines against the catalog in Postgres,
// never a cache (FR-OQ-4). It runs a fixed number of queries whatever the
// line count (NFR-PERF-1). Prices, visibility and windows are the outlet's
// for the channel (FR-OQ-1); sold-out is evaluated at At (FR-OQ-2).
func Quote(ctx context.Context, q Querier, req QuoteRequest) (QuoteResult, error) {
	var tz string
	if err := q.QueryRow(ctx, quoteOutlet, req.OutletID).Scan(&tz); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return QuoteResult{}, ErrOutletUnavailable
		}
		return QuoteResult{}, fmt.Errorf("quote outlet: %w", err)
	}
	loc, err := location(tz)
	if err != nil {
		return QuoteResult{}, fmt.Errorf("outlet timezone %q: %w", tz, err)
	}
	data, err := loadQuoteData(ctx, q, req)
	if err != nil {
		return QuoteResult{}, err
	}
	res := QuoteResult{Lines: make([]LineResult, len(req.Lines))}
	for i, line := range req.Lines {
		res.Lines[i] = data.resolve(line, req.Channel, req.At, loc, req.Mode)
	}
	return res, nil
}

type quoteProduct struct {
	productDefaults
	id, categoryID     uuid.UUID
	name, categoryName string
	costPrice          *int64
	override           *override
	soldOut            soldOut
}

type quoteVariant struct {
	id, productID uuid.UUID
	name          string
	priceDelta    int64
	active        bool
	soldOut       soldOut
}

type quoteGroup struct {
	id                   uuid.UUID
	minSelect, maxSelect int32
	active               bool
}

type quoteModifier struct {
	id, groupID uuid.UUID
	name        string
	priceDelta  int64
	active      bool
	soldOut     soldOut
}

// quoteData is the catalog slice a quote needs, keyed for resolve.
type quoteData struct {
	products  map[uuid.UUID]*quoteProduct
	variants  map[uuid.UUID][]quoteVariant // by product
	groups    map[uuid.UUID][]quoteGroup   // attached, by product
	modifiers map[uuid.UUID]*quoteModifier
}

func loadQuoteData(ctx context.Context, q Querier, req QuoteRequest) (*quoteData, error) {
	lines := req.Lines
	d := &quoteData{
		products:  map[uuid.UUID]*quoteProduct{},
		variants:  map[uuid.UUID][]quoteVariant{},
		groups:    map[uuid.UUID][]quoteGroup{},
		modifiers: map[uuid.UUID]*quoteModifier{},
	}
	if len(lines) == 0 {
		return d, nil
	}
	productIDs := make([]uuid.UUID, 0, len(lines))
	var modifierIDs []uuid.UUID
	for _, l := range lines {
		productIDs = append(productIDs, l.ProductID)
		modifierIDs = append(modifierIDs, l.ModifierIDs...)
	}
	if err := d.loadProducts(ctx, q, productIDs, req.OutletID, req.Channel); err != nil {
		return nil, err
	}
	if err := d.loadVariants(ctx, q, productIDs, req.OutletID); err != nil {
		return nil, err
	}
	if err := d.loadGroups(ctx, q, productIDs); err != nil {
		return nil, err
	}
	if err := d.loadModifiers(ctx, q, modifierIDs, req.OutletID); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *quoteData) loadProducts(ctx context.Context, q Querier, ids []uuid.UUID, outletID uuid.UUID, ch Channel) error {
	rows, err := q.Query(ctx, quoteProducts, ids, outletID, string(ch))
	if err != nil {
		return fmt.Errorf("quote products: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p quoteProduct
		var from, to, oFrom, oTo *string
		var hasOverride bool
		o := &override{}
		if err := rows.Scan(&p.id, &p.name, &p.basePrice, &p.costPrice, &p.active,
			&p.webVisible, &p.posVisible, &from, &to,
			&p.categoryID, &p.categoryName, &p.catActive,
			&hasOverride, &o.price, &o.visible, &oFrom, &oTo,
			&p.soldOut.set, &p.soldOut.until); err != nil {
			return fmt.Errorf("scan quote product: %w", err)
		}
		p.window = newWindow(from, to)
		if hasOverride {
			o.window = newWindow(oFrom, oTo)
			p.override = o
		}
		d.products[p.id] = &p
	}
	return rows.Err()
}

func (d *quoteData) loadVariants(ctx context.Context, q Querier, productIDs []uuid.UUID, outletID uuid.UUID) error {
	rows, err := q.Query(ctx, quoteVariants, productIDs, outletID)
	if err != nil {
		return fmt.Errorf("quote variants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v quoteVariant
		if err := rows.Scan(&v.id, &v.productID, &v.name, &v.priceDelta, &v.active,
			&v.soldOut.set, &v.soldOut.until); err != nil {
			return fmt.Errorf("scan quote variant: %w", err)
		}
		d.variants[v.productID] = append(d.variants[v.productID], v)
	}
	return rows.Err()
}

func (d *quoteData) loadGroups(ctx context.Context, q Querier, productIDs []uuid.UUID) error {
	rows, err := q.Query(ctx, quoteGroups, productIDs)
	if err != nil {
		return fmt.Errorf("quote modifier groups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var productID uuid.UUID
		var g quoteGroup
		if err := rows.Scan(&productID, &g.id, &g.minSelect, &g.maxSelect, &g.active); err != nil {
			return fmt.Errorf("scan quote modifier group: %w", err)
		}
		d.groups[productID] = append(d.groups[productID], g)
	}
	return rows.Err()
}

func (d *quoteData) loadModifiers(ctx context.Context, q Querier, ids []uuid.UUID, outletID uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, quoteModifiers, ids, outletID)
	if err != nil {
		return fmt.Errorf("quote modifiers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m quoteModifier
		if err := rows.Scan(&m.id, &m.groupID, &m.name, &m.priceDelta, &m.active,
			&m.soldOut.set, &m.soldOut.until); err != nil {
			return fmt.Errorf("scan quote modifier: %w", err)
		}
		d.modifiers[m.id] = &m
	}
	return rows.Err()
}
