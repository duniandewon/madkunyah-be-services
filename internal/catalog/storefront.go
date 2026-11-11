package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Storefront is one outlet's catalog on one channel with its rules left
// raw: effective prices, windows as "HH:MM", and sold-out rows with their
// expiry rather than evaluated flags. It does not change as time passes, so
// it can be cached and hashed. On pos it is the bundle (FR-BND-2); on web
// it is the cached menu before time evaluation (FR-CACHE-3).
type Storefront struct {
	// Version is the bundle version; empty on the web storefront.
	Version    string          `json:"version"`
	Outlet     StoreOutlet     `json:"outlet"`
	Categories []StoreCategory `json:"categories"`
	SoldOut    []SoldOutRule   `json:"sold_out"`
}

type StoreOutlet struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
	// BusinessDayCutoff is "HH:MM" outlet time; the POS needs it to expire
	// "today" sold-out items locally.
	BusinessDayCutoff string `json:"business_day_cutoff"`
}

type StoreCategory struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Products []StoreProduct `json:"products"`
}

type StoreProduct struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	// Price is the outlet's price on the channel.
	Price          int64          `json:"price"`
	Window         *Window        `json:"window"`
	Variants       []StoreVariant `json:"variants"`
	ModifierGroups []StoreGroup   `json:"modifier_groups"`
}

type StoreVariant struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceDelta int64  `json:"price_delta"`
}

type StoreGroup struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Required  bool            `json:"required"`
	MinSelect int32           `json:"min_select"`
	MaxSelect int32           `json:"max_select"`
	Modifiers []StoreModifier `json:"modifiers"`
}

type StoreModifier struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceDelta int64  `json:"price_delta"`
}

// SoldOutRule is a raw sold-out row: exactly one target, and an expiry or
// null for until cleared.
type SoldOutRule struct {
	ProductID    *string    `json:"product_id,omitempty"`
	VariantID    *string    `json:"variant_id,omitempty"`
	ModifierID   *string    `json:"modifier_id,omitempty"`
	SoldOutUntil *time.Time `json:"sold_out_until"`
}

func (r *SoldOutRule) targetID() string {
	switch {
	case r.ProductID != nil:
		return *r.ProductID
	case r.VariantID != nil:
		return *r.VariantID
	case r.ModifierID != nil:
		return *r.ModifierID
	}
	return ""
}

// PublicOutlet is an outlet as the online shop lists it (FR-MENU-1).
type PublicOutlet struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Address  *string `json:"address"`
	Phone    *string `json:"phone"`
	Timezone string  `json:"timezone"`
}

// Menu is the web storefront evaluated at request time (FR-MENU-4).
type Menu struct {
	Outlet     MenuOutlet     `json:"outlet"`
	Categories []MenuCategory `json:"categories"`
}

type MenuOutlet struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
}

type MenuCategory struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Products []MenuProduct `json:"products"`
}

type MenuProduct struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Description    *string       `json:"description"`
	Price          int64         `json:"price"`
	Orderable      bool          `json:"orderable"`
	SoldOut        bool          `json:"sold_out"`
	InWindow       bool          `json:"in_window"`
	Window         *Window       `json:"window"`
	Variants       []MenuVariant `json:"variants"`
	ModifierGroups []MenuGroup   `json:"modifier_groups"`
}

type MenuVariant struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceDelta int64  `json:"price_delta"`
	SoldOut    bool   `json:"sold_out"`
}

type MenuGroup struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Required  bool           `json:"required"`
	MinSelect int32          `json:"min_select"`
	MaxSelect int32          `json:"max_select"`
	Modifiers []MenuModifier `json:"modifiers"`
}

type MenuModifier struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceDelta int64  `json:"price_delta"`
	SoldOut    bool   `json:"sold_out"`
}

// soldOutAt returns the IDs of the items sold out at t.
func (sf *Storefront) soldOutAt(t time.Time) map[string]bool {
	out := map[string]bool{}
	for _, r := range sf.SoldOut {
		if (soldOut{set: true, until: r.SoldOutUntil}).at(t) {
			out[r.targetID()] = true
		}
	}
	return out
}

// menu evaluates the time-dependent flags at now (FR-CACHE-3, FR-SO-4).
func (sf *Storefront) menu(now time.Time) (*Menu, error) {
	loc, err := location(sf.Outlet.Timezone)
	if err != nil {
		return nil, fmt.Errorf("outlet timezone %q: %w", sf.Outlet.Timezone, err)
	}
	sold := sf.soldOutAt(now)
	m := &Menu{
		Outlet:     MenuOutlet{ID: sf.Outlet.ID, Name: sf.Outlet.Name, Timezone: sf.Outlet.Timezone},
		Categories: make([]MenuCategory, 0, len(sf.Categories)),
	}
	for _, c := range sf.Categories {
		mc := MenuCategory{ID: c.ID, Name: c.Name, Products: make([]MenuProduct, 0, len(c.Products))}
		for i := range c.Products {
			mc.Products = append(mc.Products, menuProduct(&c.Products[i], sold, now, loc))
		}
		m.Categories = append(m.Categories, mc)
	}
	return m, nil
}

func menuProduct(p *StoreProduct, sold map[string]bool, now time.Time, loc *time.Location) MenuProduct {
	mp := MenuProduct{
		ID: p.ID, Name: p.Name, Description: p.Description, Price: p.Price, Window: p.Window,
		SoldOut:        sold[p.ID],
		Variants:       make([]MenuVariant, 0, len(p.Variants)),
		ModifierGroups: make([]MenuGroup, 0, len(p.ModifierGroups)),
	}
	if p.Window != nil {
		mp.InWindow = inWindow(newWindow(&p.Window.From, &p.Window.To), now, loc)
	} else {
		mp.InWindow = true
	}
	variantLeft := len(p.Variants) == 0
	for _, v := range p.Variants {
		mp.Variants = append(mp.Variants, MenuVariant{ID: v.ID, Name: v.Name, PriceDelta: v.PriceDelta, SoldOut: sold[v.ID]})
		variantLeft = variantLeft || !sold[v.ID]
	}
	groupsOK := true
	for _, g := range p.ModifierGroups {
		mg := MenuGroup{ID: g.ID, Name: g.Name, Required: g.Required, MinSelect: g.MinSelect, MaxSelect: g.MaxSelect,
			Modifiers: make([]MenuModifier, 0, len(g.Modifiers))}
		var left int32
		for _, mod := range g.Modifiers {
			mg.Modifiers = append(mg.Modifiers, MenuModifier{ID: mod.ID, Name: mod.Name, PriceDelta: mod.PriceDelta,
				SoldOut: sold[mod.ID]})
			if !sold[mod.ID] {
				left++
			}
		}
		groupsOK = groupsOK && left >= g.MinSelect
		mp.ModifierGroups = append(mp.ModifierGroups, mg)
	}
	mp.Orderable = !mp.SoldOut && mp.InWindow && variantLeft && groupsOK
	return mp
}

// storefrontVersion is the SHA-256 of the storefront's JSON with an empty
// version, the same approach as the roster version (FR-BND-3). The
// storefront is built in a fixed order with UTC times, so equal content
// hashes equally.
func storefrontVersion(sf *Storefront) (string, error) {
	cp := *sf
	cp.Version = ""
	b, err := json.Marshal(&cp)
	if err != nil {
		return "", fmt.Errorf("encode bundle: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16]), nil
}

// cachedBundle is the bundle as cached: its version beside the response
// body, so a 304 needs no decode of the body.
type cachedBundle struct {
	Version string          `json:"version"`
	Body    json.RawMessage `json:"body"`
}

func (s *Service) PublicOutlets(ctx context.Context) ([]PublicOutlet, error) {
	return s.repo.PublicOutlets(ctx)
}

// Menu returns the outlet's web menu evaluated now. The storefront comes
// from the cache when it can; the time-dependent flags never do.
func (s *Service) Menu(ctx context.Context, outletID string) (*Menu, error) {
	b, err := s.cache.fetch(ctx, docMenu, outletID, func(ctx context.Context) ([]byte, error) {
		sf, err := s.repo.Storefront(ctx, outletID, ChannelWeb)
		if err != nil {
			return nil, err
		}
		return json.Marshal(sf)
	})
	if err != nil {
		return nil, err
	}
	var sf Storefront
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, fmt.Errorf("decode cached menu: %w", err)
	}
	return sf.menu(s.now())
}

// Bundle returns the outlet's POS bundle as its JSON body and version.
func (s *Service) Bundle(ctx context.Context, outletID string) ([]byte, string, error) {
	b, err := s.cache.fetch(ctx, docBundle, outletID, func(ctx context.Context) ([]byte, error) {
		sf, err := s.repo.Storefront(ctx, outletID, ChannelPOS)
		if err != nil {
			return nil, err
		}
		if sf.Version, err = storefrontVersion(sf); err != nil {
			return nil, err
		}
		body, err := json.Marshal(sf)
		if err != nil {
			return nil, fmt.Errorf("encode bundle: %w", err)
		}
		return json.Marshal(cachedBundle{Version: sf.Version, Body: body})
	})
	if err != nil {
		return nil, "", err
	}
	var cb cachedBundle
	if err := json.Unmarshal(b, &cb); err != nil {
		return nil, "", fmt.Errorf("decode cached bundle: %w", err)
	}
	return cb.Body, cb.Version, nil
}

// RecordBundleFetch notes which bundle a device holds (FR-BND-5). The
// version is informational: nothing is ever refused for an old one.
func (s *Service) RecordBundleFetch(ctx context.Context, deviceID, version string) error {
	return s.repo.RecordBundleFetch(ctx, deviceID, version)
}
