package catalog

import (
	"context"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
)

// Sold-out scopes (FR-SO-2).
const (
	ScopeToday      = "today"
	ScopeIndefinite = "indefinite"
)

// Where a sold-out change came from.
const (
	ViaDashboard = "dashboard"
	ViaPOS       = "pos"
)

// SoldOutItem names exactly one product, variant or modifier.
type SoldOutItem struct {
	ProductID  *string `json:"product_id,omitempty"`
	VariantID  *string `json:"variant_id,omitempty"`
	ModifierID *string `json:"modifier_id,omitempty"`
}

// kind returns the item's audit entity type and canonical ID, or false
// unless exactly one valid UUID is set (E-17).
func (i *SoldOutItem) kind() (string, string, bool) {
	var entity, id string
	n := 0
	for _, t := range []struct {
		entity string
		id     *string
	}{{entityProduct, i.ProductID}, {entityVariant, i.VariantID}, {entityModifier, i.ModifierID}} {
		if t.id == nil {
			continue
		}
		n++
		parsed, err := uuid.Parse(*t.id)
		if err != nil {
			return "", "", false
		}
		entity, id = t.entity, parsed.String()
	}
	return entity, id, n == 1
}

func itemFor(entity, id string) SoldOutItem {
	switch entity {
	case entityProduct:
		return SoldOutItem{ProductID: &id}
	case entityVariant:
		return SoldOutItem{VariantID: &id}
	default:
		return SoldOutItem{ModifierID: &id}
	}
}

// SoldOutInput is the dashboard body; the POS body adds changed_at.
type SoldOutInput struct {
	Item    SoldOutItem `json:"item"`
	SoldOut *bool       `json:"sold_out" validate:"required"`
	Scope   string      `json:"scope" validate:"omitempty,oneof=today indefinite"`
}

// SoldOutWrite is one validated toggle.
type SoldOutWrite struct {
	OutletID string
	Entity   string
	ItemID   string
	SoldOut  bool
	Scope    string
	ActorID  string
	Via      string
	DeviceID *string
	// ChangedAt orders toggles (FR-SO-6): device time for POS, clamped to
	// server time, and server time for the dashboard.
	ChangedAt time.Time
}

// SoldOutState is an item's stored state at an outlet, with SoldOut
// evaluated now. The change fields are null for an item never toggled.
type SoldOutState struct {
	Item         SoldOutItem `json:"item"`
	SoldOut      bool        `json:"sold_out"`
	SoldOutUntil *time.Time  `json:"sold_out_until"`
	ChangedAt    *time.Time  `json:"changed_at"`
	ChangedBy    *string     `json:"changed_by"`
	ChangedVia   *string     `json:"changed_via"`
}

// SoldOutResult reports whether a toggle changed anything; a repeat or a
// toggle older than the stored one did not (FR-SO-6).
type SoldOutResult struct {
	Applied bool `json:"applied"`
	SoldOutState
}

// StaffRef names a staff member in a listing.
type StaffRef struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
}

// SoldOutEntry is one item currently sold out at an outlet (FR-SO-10).
type SoldOutEntry struct {
	Item SoldOutItem `json:"item"`
	Name string      `json:"name"`
	// ProductName is the parent product's name, for a variant.
	ProductName  *string    `json:"product_name,omitempty"`
	SoldOutUntil *time.Time `json:"sold_out_until"`
	ChangedAt    time.Time  `json:"changed_at"`
	ChangedBy    StaffRef   `json:"changed_by"`
	ChangedVia   string     `json:"changed_via"`
}

// soldOutRow is an outlet_sold_out row; found is false when there is none.
type soldOutRow struct {
	found      bool
	id         string
	isSoldOut  bool
	until      *time.Time
	changedAt  time.Time
	changedBy  string
	changedVia string
	deviceID   *string
}

func (r *soldOutRow) state(entity, id string, now time.Time) SoldOutState {
	st := SoldOutState{Item: itemFor(entity, id)}
	if !r.found {
		return st
	}
	at, by, via := r.changedAt, r.changedBy, r.changedVia
	st.SoldOut = soldOut{set: r.isSoldOut, until: r.until}.at(now)
	st.SoldOutUntil, st.ChangedAt, st.ChangedBy, st.ChangedVia = r.until, &at, &by, &via
	return st
}

func (r *soldOutRow) fields() fields {
	var until any
	if r.until != nil {
		until = r.until.UTC().Format(time.RFC3339)
	}
	return fields{"is_sold_out": r.found && r.isSoldOut, "sold_out_until": until}
}

// decideSoldOut applies FR-SO-2 and FR-SO-6 to the stored row cur. It
// returns the row to store, or false when the toggle changes nothing: it
// is older than the stored one, or the item is already in the requested
// state at now.
func decideSoldOut(w *SoldOutWrite, cur *soldOutRow, now time.Time, loc *time.Location, cutoff int) (*soldOutRow, bool) {
	if cur.found && w.ChangedAt.Before(cur.changedAt) {
		return nil, false
	}
	isNow := soldOut{set: cur.found && cur.isSoldOut, until: cur.until}.at(now)
	next := &soldOutRow{found: true, id: cur.id, isSoldOut: w.SoldOut, changedAt: w.ChangedAt,
		changedBy: w.ActorID, changedVia: w.Via, deviceID: w.DeviceID}
	if !w.SoldOut {
		return next, isNow
	}
	if w.Scope != ScopeIndefinite {
		u := nextCutoff(w.ChangedAt, loc, cutoff).UTC()
		next.until = &u
	}
	if isNow && sameTime(cur.until, next.until) {
		return nil, false
	}
	return next, true
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// SetSoldOut validates and applies a toggle. A POS changed_at in the
// future is clamped to server time (FR-SO-6).
func (s *Service) SetSoldOut(ctx context.Context, w *SoldOutWrite) (*SoldOutResult, error) {
	now := s.now()
	if w.Via == ViaDashboard || w.ChangedAt.After(now) {
		w.ChangedAt = now
	}
	if w.Scope == "" {
		w.Scope = ScopeToday
	}
	res, err := s.repo.SetSoldOut(ctx, w, now)
	s.cache.afterOutletWrite(ctx, w.OutletID, err == nil && res.Applied, err)
	return res, err
}

func (s *Service) ListSoldOut(ctx context.Context, outletID string) ([]SoldOutEntry, error) {
	return s.repo.ListSoldOut(ctx, outletID, s.now())
}

// NewSoldOutWrite validates a toggle body for the outlet.
func NewSoldOutWrite(outletID string, in *SoldOutInput, actorID, via string) (*SoldOutWrite, error) {
	entity, id, ok := in.Item.kind()
	if !ok {
		return nil, domain.ValidationError(map[string]string{
			"item": "must name exactly one of product_id, variant_id, modifier_id as a UUID"})
	}
	return &SoldOutWrite{OutletID: outletID, Entity: entity, ItemID: id, SoldOut: *in.SoldOut, Scope: in.Scope,
		ActorID: actorID, Via: via}, nil
}
