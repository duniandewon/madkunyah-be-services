package catalog

import (
	"time"

	"github.com/google/uuid"
)

// resolve applies FR-QTE-3 and FR-MOD-4 to one line. It is the only place
// the resolution rules live.
func (d *quoteData) resolve(line QuoteLine, ch Channel, at time.Time, loc *time.Location, mode QuoteMode) LineResult {
	p := d.products[line.ProductID]
	if p == nil {
		return LineResult{Problems: []Problem{{Kind: ProblemProductUnavailable}}}
	}
	var problems []Problem
	if !p.active || !p.catActive || !visibleOn(p, ch) {
		problems = append(problems, Problem{Kind: ProblemProductUnavailable})
	}
	if !inWindow(p.window, at, loc) {
		problems = append(problems, Problem{Kind: ProblemOutsideWindow})
	}

	snap := &LineSnapshot{
		ProductID:    p.id,
		ProductName:  p.name,
		CategoryID:   p.categoryID,
		CategoryName: p.categoryName,
		BasePrice:    p.basePrice,
		UnitCost:     p.costPrice,
		Modifiers:    []ModifierSnapshot{},
	}
	problems = append(problems, d.resolveVariant(line, snap)...)
	problems = append(problems, d.resolveModifiers(line, snap)...)
	snap.UnitPrice = snap.BasePrice + snap.VariantDelta + snap.ModifiersDelta

	if len(problems) > 0 && mode == Strict {
		return LineResult{Problems: problems}
	}
	return LineResult{Snapshot: snap, Problems: problems}
}

func visibleOn(p *quoteProduct, ch Channel) bool {
	switch ch {
	case ChannelWeb:
		return p.webVisible
	case ChannelPOS:
		return p.posVisible
	default:
		return false
	}
}

// resolveVariant enforces FR-VAR-1: exactly one active variant when the
// product has any, none otherwise. An archived variant of this product
// still prices the snapshot, for Record mode.
func (d *quoteData) resolveVariant(line QuoteLine, snap *LineSnapshot) []Problem {
	variants := d.variants[line.ProductID]
	if line.VariantID == nil {
		for _, v := range variants {
			if v.active {
				return []Problem{{Kind: ProblemVariantRequired}}
			}
		}
		return nil
	}
	for _, v := range variants {
		if v.id != *line.VariantID {
			continue
		}
		id, name := v.id, v.name
		snap.VariantID, snap.VariantName, snap.VariantDelta = &id, &name, v.priceDelta
		if !v.active {
			return []Problem{{Kind: ProblemVariantInvalid, ID: v.id}}
		}
		return nil
	}
	return []Problem{{Kind: ProblemVariantInvalid, ID: *line.VariantID}}
}

// resolveModifiers enforces FR-MOD-4. Every modifier that exists is priced
// into the snapshot, valid or not, so Record mode reflects what was sold;
// only valid ones count towards a group's min/max.
func (d *quoteData) resolveModifiers(line QuoteLine, snap *LineSnapshot) []Problem {
	attached := map[uuid.UUID]quoteGroup{}
	for _, g := range d.groups[line.ProductID] {
		attached[g.id] = g
	}
	var problems []Problem
	counts := map[uuid.UUID]int32{}
	seen := map[uuid.UUID]bool{}
	for _, id := range line.ModifierIDs {
		if seen[id] {
			problems = append(problems, Problem{Kind: ProblemModifierInvalid, ID: id})
			continue
		}
		seen[id] = true
		m := d.modifiers[id]
		if m == nil {
			problems = append(problems, Problem{Kind: ProblemModifierInvalid, ID: id})
			continue
		}
		snap.Modifiers = append(snap.Modifiers, ModifierSnapshot{ID: m.id, Name: m.name, PriceDelta: m.priceDelta})
		snap.ModifiersDelta += m.priceDelta
		g, ok := attached[m.groupID]
		if !ok || !g.active || !m.active {
			problems = append(problems, Problem{Kind: ProblemModifierInvalid, ID: id})
			continue
		}
		counts[g.id]++
	}
	for _, g := range d.groups[line.ProductID] {
		if !g.active {
			continue
		}
		if n := counts[g.id]; n < g.minSelect || n > g.maxSelect {
			problems = append(problems, Problem{Kind: ProblemModifierCountInvalid, ID: g.id})
		}
	}
	return problems
}
