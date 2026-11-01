package catalog

import "time"

// productDefaults is what a product carries before any outlet override.
type productDefaults struct {
	active, catActive bool
	webVisible        bool
	posVisible        bool
	basePrice         int64
	window            *window
}

// override is a product_channel_settings row for one outlet and channel.
// A nil field inherits the product default; a nil *override inherits all.
type override struct {
	price   *int64
	visible *bool
	window  *window
}

// resolved is a product as one outlet sells it on one channel.
type resolved struct {
	visible bool
	price   int64
	window  *window
}

// resolveChannel applies FR-OUT-2. It is the only place the outlet
// resolution rules live; Quote, the menu, the bundle and the dashboard view
// all go through it.
func resolveChannel(p *productDefaults, ch Channel, o *override) resolved {
	if o == nil {
		o = &override{}
	}
	visible := false
	switch ch {
	case ChannelWeb:
		visible = p.webVisible
	case ChannelPOS:
		visible = p.posVisible
	}
	if o.visible != nil {
		visible = *o.visible
	}
	r := resolved{visible: p.active && p.catActive && visible, price: p.basePrice, window: p.window}
	if o.price != nil {
		r.price = *o.price
	}
	// An outlet window replaces the product window; it cannot remove it
	// (FR-OUT-6).
	if o.window != nil {
		r.window = o.window
	}
	return r
}

// soldOut is an item's outlet_sold_out state. until nil means sold out
// until cleared.
type soldOut struct {
	set   bool
	until *time.Time
}

// at reports whether the item is sold out at t (FR-SO-3). Expiry needs no
// job: a row past its until simply stops counting.
func (s soldOut) at(t time.Time) bool {
	return s.set && (s.until == nil || s.until.After(t))
}

// nextCutoff is the outlet's next business-day cutoff strictly after at,
// in the outlet's timezone (FR-SO-2). cutoff is minutes after midnight.
func nextCutoff(at time.Time, loc *time.Location, cutoff int) time.Time {
	local := at.In(loc)
	c := time.Date(local.Year(), local.Month(), local.Day(), cutoff/60, cutoff%60, 0, 0, loc)
	if !c.After(local) {
		c = time.Date(local.Year(), local.Month(), local.Day()+1, cutoff/60, cutoff%60, 0, 0, loc)
	}
	return c
}

// Window is a daily "HH:MM" window as clients see it.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (w *window) public() *Window {
	if w == nil {
		return nil
	}
	return &Window{From: formatClock(w.from), To: formatClock(w.to)}
}
