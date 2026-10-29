// Package catalog maintains the global menu (categories, products, variants,
// modifier groups) and prices order lines against it through Quote.
package catalog

import (
	"encoding/json"
	"time"
)

type Category struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SortOrder int32     `json:"sort_order"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Product struct {
	ID          string  `json:"id"`
	CategoryID  string  `json:"category_id"`
	SKU         *string `json:"sku"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	BasePrice   int64   `json:"base_price"`
	// CostPrice is serialized only for callers holding cost.view; for
	// everyone else the field is absent, not null (FR-PRD-7).
	CostPrice    CostPrice `json:"cost_price,omitzero"`
	IsWebVisible bool      `json:"is_web_visible"`
	IsPOSVisible bool      `json:"is_pos_visible"`
	// AvailableFrom and AvailableTo are a daily "HH:MM" window in the
	// outlet's local time; both nil means always in window.
	AvailableFrom *string   `json:"available_from"`
	AvailableTo   *string   `json:"available_to"`
	SortOrder     int32     `json:"sort_order"`
	IsActive      bool      `json:"is_active"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// CostPrice hides itself unless Shown is set, so a product is redacted by
// default and only an explicit cost.view check reveals it.
type CostPrice struct {
	Value *int64
	Shown bool
}

func (c CostPrice) IsZero() bool { return !c.Shown }

func (c CostPrice) MarshalJSON() ([]byte, error) { return json.Marshal(c.Value) }

type Variant struct {
	ID         string    `json:"id"`
	ProductID  string    `json:"product_id"`
	Name       string    `json:"name"`
	PriceDelta int64     `json:"price_delta"`
	SKU        *string   `json:"sku"`
	SortOrder  int32     `json:"sort_order"`
	IsActive   bool      `json:"is_active"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type ModifierGroup struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MinSelect int32  `json:"min_select"`
	MaxSelect int32  `json:"max_select"`
	// Required is derived: min_select >= 1 (FR-MOD-2).
	Required  bool       `json:"required"`
	IsActive  bool       `json:"is_active"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Modifiers []Modifier `json:"modifiers"`
}

type Modifier struct {
	ID              string    `json:"id"`
	ModifierGroupID string    `json:"modifier_group_id"`
	Name            string    `json:"name"`
	PriceDelta      int64     `json:"price_delta"`
	SortOrder       int32     `json:"sort_order"`
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// AttachedGroup is a modifier group as attached to one product, with that
// product's ordering.
type AttachedGroup struct {
	ModifierGroup
	SortOrder int32 `json:"sort_order"`
}

// ProductDetail is a product with its variants and attached modifier groups,
// archived children included.
type ProductDetail struct {
	Product
	Variants       []Variant       `json:"variants"`
	ModifierGroups []AttachedGroup `json:"modifier_groups"`
}

// AffectedProduct names a product in a 409 that a write would break.
type AffectedProduct struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Nullable is a PATCH field that tells absent (Set false), null (Set, not
// Valid) and a value apart. A value of the wrong JSON type sets Bad instead
// of failing the decode, so the service can report it against its field.
type Nullable[T any] struct {
	Set   bool
	Valid bool
	Bad   bool
	V     T
}

func (n *Nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		return nil
	}
	if err := json.Unmarshal(b, &n.V); err != nil {
		n.Bad = true
		return nil
	}
	n.Valid = true
	return nil
}

// Ptr returns the value, or nil for null.
func (n Nullable[T]) Ptr() *T {
	if !n.Valid {
		return nil
	}
	v := n.V
	return &v
}
