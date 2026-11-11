package catalog

import (
	"context"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

// ChannelSettings is an outlet's override for one product on one channel.
// A null field inherits the product default (FR-OUT-1).
type ChannelSettings struct {
	Price         *int64    `json:"price"`
	IsVisible     *bool     `json:"is_visible"`
	AvailableFrom *string   `json:"available_from"`
	AvailableTo   *string   `json:"available_to"`
	UpdatedBy     string    `json:"updated_by"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (s *ChannelSettings) fields() fields {
	return fields{"price": val(s.Price), "is_visible": val(s.IsVisible),
		"available_from": val(s.AvailableFrom), "available_to": val(s.AvailableTo)}
}

// ChannelView is a product as one outlet sells it on one channel: the
// override row, if any, and the resolved values (FR-OUT-9).
type ChannelView struct {
	Override *ChannelSettings `json:"override"`
	Visible  bool             `json:"visible"`
	Price    int64            `json:"price"`
	Window   *Window          `json:"window"`
}

type OutletChannels struct {
	Web ChannelView `json:"web"`
	POS ChannelView `json:"pos"`
}

// OutletProduct is a product with its settings at one outlet.
type OutletProduct struct {
	Product
	Channels OutletChannels `json:"channels"`
}

// ChannelResult answers a settings write with the product's resolved state
// on that channel.
type ChannelResult struct {
	ProductID string  `json:"product_id"`
	OutletID  string  `json:"outlet_id"`
	Channel   Channel `json:"channel"`
	ChannelView
	// changed reports whether the write changed the stored row.
	changed bool
}

// ChannelSettingsInput sets the fields present in the body; null returns a
// field to inheriting, and an absent field keeps its value. A field counts
// as touched for FR-OUT-4 when it is present.
type ChannelSettingsInput struct {
	Price         Nullable[int64]  `json:"price"`
	IsVisible     Nullable[bool]   `json:"is_visible"`
	AvailableFrom Nullable[string] `json:"available_from"`
	AvailableTo   Nullable[string] `json:"available_to"`
}

type settingsKey struct {
	outletID, productID string
	channel             Channel
}

// outletRow is a product with what the outlet view resolves it from.
type outletRow struct {
	product   Product
	catActive bool
	web, pos  *ChannelSettings
}

var (
	errPriceForbidden = domain.NewError(domain.CodeForbidden,
		"Outlet prices can only be changed with catalog.manage.")
	errAvailabilityForbidden = domain.NewError(domain.CodeForbidden,
		"Changing visibility or windows requires product.availability.manage at this outlet.")
)

// channelView resolves p on ch at an outlet with settings s (FR-OUT-2).
func channelView(p *Product, catActive bool, ch Channel, s *ChannelSettings) ChannelView {
	d := productDefaults{active: p.IsActive, catActive: catActive, webVisible: p.IsWebVisible,
		posVisible: p.IsPOSVisible, basePrice: p.BasePrice, window: newWindow(p.AvailableFrom, p.AvailableTo)}
	var o *override
	if s != nil {
		o = &override{price: s.Price, visible: s.IsVisible, window: newWindow(s.AvailableFrom, s.AvailableTo)}
	}
	r := resolveChannel(&d, ch, o)
	return ChannelView{Override: s, Visible: r.visible, Price: r.price, Window: r.window.public()}
}

// effectivePrice is the price a settings row resolves to.
func effectivePrice(s *ChannelSettings, basePrice int64) int64 {
	if s != nil && s.Price != nil {
		return *s.Price
	}
	return basePrice
}

func (s *Service) OutletCatalog(ctx context.Context, actor *domain.StaffAccess, outletID string,
	includeArchived bool) ([]OutletProduct, error) {
	rows, err := s.repo.OutletCatalog(ctx, outletID, includeArchived)
	if err != nil {
		return nil, err
	}
	out := make([]OutletProduct, len(rows))
	for i := range rows {
		r := &rows[i]
		showCost(&r.product, actor)
		out[i] = OutletProduct{Product: r.product, Channels: OutletChannels{
			Web: channelView(&r.product, r.catActive, ChannelWeb, r.web),
			POS: channelView(&r.product, r.catActive, ChannelPOS, r.pos),
		}}
	}
	return out, nil
}

// SetChannelSettings applies FR-OUT-4 field by field: price needs
// catalog.manage, visibility and windows need product.availability.manage
// at the outlet, and a request touching a field the caller may not change
// writes nothing.
func (s *Service) SetChannelSettings(ctx context.Context, actor *domain.StaffAccess, outletID, productID string,
	ch Channel, in *ChannelSettingsInput) (*ChannelResult, error) {
	touchesAvailability := in.IsVisible.Set || in.AvailableFrom.Set || in.AvailableTo.Set
	if !in.Price.Set && !touchesAvailability {
		return nil, domain.ValidationError(map[string]string{
			"body": "must set at least one of price, is_visible, available_from, available_to"})
	}
	if in.Price.Set && !actor.CanGlobal(domain.PermCatalogManage) {
		return nil, errPriceForbidden
	}
	if touchesAvailability && !actor.Can(outletID, domain.PermAvailabilityManage) {
		return nil, errAvailabilityForbidden
	}
	k := settingsKey{outletID: outletID, productID: productID, channel: ch}
	res, err := s.repo.UpdateChannelSettings(ctx, actor.StaffID, k, func(cs *ChannelSettings) error {
		return s.applySettings(cs, in)
	})
	s.cache.afterOutletWrite(ctx, outletID, err == nil && res.changed, err)
	return res, err
}

func (s *Service) applySettings(cs *ChannelSettings, in *ChannelSettingsInput) error {
	c := checks{}
	if in.Price.Set && !c.badType("price", in.Price.Bad, "an integer or null") {
		cs.Price = in.Price.Ptr()
		if cs.Price != nil {
			c.price("price", *cs.Price, s.priceMax)
		}
	}
	if in.IsVisible.Set && !c.badType("is_visible", in.IsVisible.Bad, "a boolean or null") {
		cs.IsVisible = in.IsVisible.Ptr()
	}
	from, to := cs.AvailableFrom, cs.AvailableTo
	if in.AvailableFrom.Set && !c.badType("available_from", in.AvailableFrom.Bad, "a time or null") {
		from = in.AvailableFrom.Ptr()
	}
	if in.AvailableTo.Set && !c.badType("available_to", in.AvailableTo.Bad, "a time or null") {
		to = in.AvailableTo.Ptr()
	}
	cs.AvailableFrom, cs.AvailableTo = c.window(from, to)
	return c.err()
}

// ResetChannelSettings removes the row, so every field inherits again
// (FR-OUT-5). Clearing a field counts as changing it, so a manager cannot
// reset a row that carries an owner's price.
func (s *Service) ResetChannelSettings(ctx context.Context, actor *domain.StaffAccess, outletID, productID string,
	ch Channel) error {
	canPrice := actor.CanGlobal(domain.PermCatalogManage)
	canAvailability := actor.Can(outletID, domain.PermAvailabilityManage)
	if !canPrice && !canAvailability {
		return errAvailabilityForbidden
	}
	k := settingsKey{outletID: outletID, productID: productID, channel: ch}
	changed, err := s.repo.ResetChannelSettings(ctx, actor.StaffID, k, func(cs *ChannelSettings) error {
		if cs.Price != nil && !canPrice {
			return errPriceForbidden
		}
		if (cs.IsVisible != nil || cs.AvailableFrom != nil) && !canAvailability {
			return errAvailabilityForbidden
		}
		return nil
	})
	s.cache.afterOutletWrite(ctx, outletID, changed, err)
	return err
}
