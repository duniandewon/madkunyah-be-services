package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/catalog"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
)

// pricedLine is a requested line with its catalog snapshot.
type pricedLine struct {
	in        *LineInput
	snap      *catalog.LineSnapshot
	lineTotal int64
}

// priceLines prices lines with catalog.Quote in the caller's transaction:
// the POS channel, Strict, at the current time (FR-PLC-4, FR-ADD-2). Any
// problem refuses every line with lines_unavailable (FR-PLC-5); a catalog
// that cannot be read is catalog_unavailable (§5.1).
func priceLines(ctx context.Context, q catalog.Querier, outletID string, at time.Time,
	lines []LineInput) ([]pricedLine, error) {
	req := catalog.QuoteRequest{
		OutletID: uuid.MustParse(outletID),
		Channel:  catalog.ChannelPOS,
		At:       at,
		Mode:     catalog.Strict,
		Lines:    make([]catalog.QuoteLine, len(lines)),
	}
	for i, l := range lines {
		req.Lines[i] = catalog.QuoteLine{ProductID: l.ProductID, VariantID: l.VariantID, ModifierIDs: l.ModifierIDs}
	}
	res, err := catalog.Quote(ctx, q, req)
	if errors.Is(err, catalog.ErrOutletUnavailable) {
		return nil, domain.ErrNotFound
	}
	if err != nil && unreachable(err) {
		return nil, domain.WrapError(domain.CodeCatalogUnavailable, "The menu cannot be read right now. Try again.", err)
	}
	if err != nil {
		return nil, fmt.Errorf("quote lines: %w", err)
	}
	if !res.Valid() {
		return nil, linesUnavailable(lines, res.Lines)
	}
	priced := make([]pricedLine, len(lines))
	for i := range lines {
		snap := res.Lines[i].Snapshot
		priced[i] = pricedLine{in: &lines[i], snap: snap, lineTotal: lineTotal(snap.UnitPrice, lines[i].Quantity, 0)}
	}
	return priced, nil
}

// linesUnavailable lists every problem of every refused line, so the POS
// can show which lines to fix.
func linesUnavailable(lines []LineInput, results []catalog.LineResult) error {
	var problems []LineProblem
	for i, r := range results {
		for _, p := range r.Problems {
			lp := LineProblem{ID: lines[i].ID, Kind: string(p.Kind)}
			if p.ID != uuid.Nil {
				target := p.ID.String()
				lp.TargetID = &target
			}
			problems = append(problems, lp)
		}
	}
	return &domain.Error{
		Code:    domain.CodeLinesUnavailable,
		Message: "Some lines cannot be sold right now.",
		Details: LinesUnavailable{Lines: problems},
	}
}
