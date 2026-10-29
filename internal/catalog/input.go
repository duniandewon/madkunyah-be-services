package catalog

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

// Length limits (NFR-SEC-2). Struct tags enforce them on the raw value; the
// service trims and re-checks.
const (
	maxCategoryName = 60
	maxProductName  = 100
	maxDescription  = 1000
	maxSKU          = 64
	maxVariantName  = 60
	maxGroupName    = 60
	maxModifierName = 60
	maxSelectLimit  = 50
	maxSearchLen    = 100
)

type CategoryInput struct {
	Name      string `json:"name" validate:"required,max=60"`
	SortOrder int32  `json:"sort_order"`
}

type CategoryPatch struct {
	Name      *string `json:"name" validate:"omitempty,max=60"`
	SortOrder *int32  `json:"sort_order"`
	IsActive  *bool   `json:"is_active"`
}

type ProductInput struct {
	CategoryID    string  `json:"category_id" validate:"required,uuid"`
	Name          string  `json:"name" validate:"required,max=100"`
	Description   *string `json:"description" validate:"omitempty,max=1000"`
	SKU           *string `json:"sku" validate:"omitempty,max=64"`
	BasePrice     *int64  `json:"base_price" validate:"required"`
	CostPrice     *int64  `json:"cost_price"`
	IsWebVisible  *bool   `json:"is_web_visible"`
	IsPOSVisible  *bool   `json:"is_pos_visible"`
	AvailableFrom *string `json:"available_from"`
	AvailableTo   *string `json:"available_to"`
	SortOrder     int32   `json:"sort_order"`
}

type ProductPatch struct {
	CategoryID    *string          `json:"category_id" validate:"omitempty,uuid"`
	Name          *string          `json:"name" validate:"omitempty,max=100"`
	Description   Nullable[string] `json:"description"`
	SKU           Nullable[string] `json:"sku"`
	BasePrice     *int64           `json:"base_price"`
	CostPrice     Nullable[int64]  `json:"cost_price"`
	IsWebVisible  *bool            `json:"is_web_visible"`
	IsPOSVisible  *bool            `json:"is_pos_visible"`
	AvailableFrom Nullable[string] `json:"available_from"`
	AvailableTo   Nullable[string] `json:"available_to"`
	SortOrder     *int32           `json:"sort_order"`
	IsActive      *bool            `json:"is_active"`
}

type VariantInput struct {
	Name       string  `json:"name" validate:"required,max=60"`
	PriceDelta int64   `json:"price_delta"`
	SKU        *string `json:"sku" validate:"omitempty,max=64"`
	SortOrder  int32   `json:"sort_order"`
}

type VariantPatch struct {
	Name       *string          `json:"name" validate:"omitempty,max=60"`
	PriceDelta *int64           `json:"price_delta"`
	SKU        Nullable[string] `json:"sku"`
	SortOrder  *int32           `json:"sort_order"`
	IsActive   *bool            `json:"is_active"`
}

type GroupInput struct {
	Name      string `json:"name" validate:"required,max=60"`
	MinSelect int32  `json:"min_select"`
	MaxSelect *int32 `json:"max_select"`
}

type GroupPatch struct {
	Name      *string `json:"name" validate:"omitempty,max=60"`
	MinSelect *int32  `json:"min_select"`
	MaxSelect *int32  `json:"max_select"`
	IsActive  *bool   `json:"is_active"`
}

type ModifierInput struct {
	Name       string `json:"name" validate:"required,max=60"`
	PriceDelta int64  `json:"price_delta"`
	SortOrder  int32  `json:"sort_order"`
}

type ModifierPatch struct {
	Name       *string `json:"name" validate:"omitempty,max=60"`
	PriceDelta *int64  `json:"price_delta"`
	SortOrder  *int32  `json:"sort_order"`
	IsActive   *bool   `json:"is_active"`
}

// AttachInput replaces a product's modifier groups; the array order becomes
// the per-product sort order.
type AttachInput struct {
	ModifierGroupIDs []string `json:"modifier_group_ids" validate:"max=50,dive,uuid"`
}

type ProductFilter struct {
	CategoryID      *string
	Query           string
	IncludeArchived bool
}

// checks accumulates field errors into one validation_failed response.
type checks map[string]string

func (c checks) err() error {
	if len(c) == 0 {
		return nil
	}
	return domain.ValidationError(c)
}

// name trims a required name and checks its length.
func (c checks) name(field, v string, max int) string {
	v = strings.TrimSpace(v)
	switch n := utf8.RuneCountInString(v); {
	case n == 0:
		c[field] = "must not be empty"
	case n > max:
		c[field] = fmt.Sprintf("must be at most %d characters", max)
	}
	return v
}

// text trims an optional string; empty becomes nil.
func (c checks) text(field string, v *string, max int) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > max {
		c[field] = fmt.Sprintf("must be at most %d characters", max)
	}
	return &s
}

// price enforces FR-PRD-4. Wrong JSON types are rejected by the decoder.
func (c checks) price(field string, v, max int64) {
	if v < 0 || v > max {
		c[field] = fmt.Sprintf("must be an integer from 0 to %d", max)
	}
}

func (c checks) badType(field string, bad bool, want string) bool {
	if bad {
		c[field] = "must be " + want
	}
	return bad
}

// window validates and normalizes a daily window (FR-AVL-1): both ends or
// neither, "HH:MM", and from != to.
func (c checks) window(from, to *string) (*string, *string) {
	if (from == nil) != (to == nil) {
		c["available_from"] = "available_from and available_to must be set together"
		return from, to
	}
	if from == nil {
		return nil, nil
	}
	f, okF := parseClock(*from)
	t, okT := parseClock(*to)
	if !okF {
		c["available_from"] = "must be a time as HH:MM"
	}
	if !okT {
		c["available_to"] = "must be a time as HH:MM"
	}
	if !okF || !okT {
		return from, to
	}
	if f == t {
		c["available_to"] = "must differ from available_from"
	}
	fs, ts := formatClock(f), formatClock(t)
	return &fs, &ts
}

func (c checks) selectRange(minSel, maxSel int32) {
	if minSel < 0 || minSel > maxSelectLimit {
		c["min_select"] = fmt.Sprintf("must be from 0 to %d", maxSelectLimit)
	}
	if maxSel < 1 || maxSel > maxSelectLimit {
		c["max_select"] = fmt.Sprintf("must be from 1 to %d", maxSelectLimit)
	}
	if _, bad := c["min_select"]; !bad && minSel > maxSel {
		c["min_select"] = "must not exceed max_select"
	}
}

// parseClock parses a strict "HH:MM" into minutes after midnight.
func parseClock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	digits := s[:2] + s[3:]
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func formatClock(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}
