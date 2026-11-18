package order

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Integration tests for POS Controls (pos-controls-spec §6). Tea is 10,000;
// a Large Kopi with both modifiers is 28,000.

var controlPerms = append([]string{domain.PermOrderDiscount, domain.PermOrderPriceOverride}, orderPerms...)

// supervisor holds both control permissions at the env's outlet.
func (e *env) supervisor(name string) *domain.StaffAccess {
	return e.staff(name, e.outlet, domain.RoleSupervisor, controlPerms...)
}

func discount(typ string, value int64, approvedBy string) *DiscountInput {
	return &DiscountInput{Type: typ, Value: value, Reason: "Regular customer", ApprovedBy: approvedBy}
}

func override(price int64, approvedBy string) *OverrideInput {
	return &OverrideInput{UnitPrice: price, Reason: "Wrong price on the board", ApprovedBy: approvedBy}
}

func (e *env) placeAs(a *domain.StaffAccess, d *DiscountInput, lines ...LineInput) (*Result, error) {
	return e.svc.Place(e.ctx, &PlaceInput{Actor: e.actor(a), ID: uuid.NewString(), Lines: lines, Discount: d})
}

func (e *env) ctl(a *domain.StaffAccess, orderID, lineID string) *ControlInput {
	return &ControlInput{Actor: e.actor(a), OrderID: orderID, LineID: lineID}
}

type controlAuditRow struct {
	Action     string
	ActorID    string
	ApprovedBy string
	ValueDelta int64
	Reason     *string
	Before     map[string]any
	After      map[string]any
}

// controlAudits are the order's control audit rows, oldest first.
func controlAudits(t *testing.T, orderID string) []controlAuditRow {
	t.Helper()
	rows, err := testDB.Pool.Query(context.Background(),
		`SELECT action, actor_id::text, approved_by::text, value_delta, reason, before, after
		 FROM audit_logs WHERE entity_type = 'order' AND entity_id = $1
		   AND action IN ('line.discount', 'line.discount_removed', 'price.override', 'price.override_removed',
		                  'order.discount', 'order.discount_removed')
		 ORDER BY created_at, action`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[controlAuditRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// wantTotals checks the stored order against computeTotals over its lines.
func wantTotals(t *testing.T, d *Detail) {
	t.Helper()
	var lines []totalsLine
	for _, l := range d.Lines {
		tl := totalsLine{ID: l.ID, UnitPrice: l.UnitPrice, Quantity: l.Quantity, FulfillmentType: l.FulfillmentType,
			Status: l.Status}
		if l.Discount != nil {
			tl.Discount = &discountRule{Type: l.Discount.Type, Value: l.Discount.Value}
		}
		tl.LineNo = len(lines) + 1
		lines = append(lines, tl)
	}
	var rule *discountRule
	if d.Discount != nil {
		rule = &discountRule{Type: d.Discount.Type, Value: d.Discount.Value}
	}
	want, err := computeTotals(lines, rule)
	if err != nil {
		t.Fatal(err)
	}
	if d.Subtotal != want.Subtotal || d.DiscountTotal != want.DiscountTotal || d.GrandTotal != want.GrandTotal {
		t.Fatalf("totals %d/%d/%d, want %+v", d.Subtotal, d.DiscountTotal, d.GrandTotal, want)
	}
	for i, l := range d.Lines {
		a := lineAmounts{l.DiscountAmount, l.LineTotal, l.OrderDiscountShare}
		if a != want.Lines[i] {
			t.Fatalf("line %d amounts %+v, want %+v", i, a, want.Lines[i])
		}
	}
}

// AC-2: a supervisor places an order with an override, a line discount
// and a fixed order discount.
func TestPlaceWithControls(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	kopi, tea := e.kopiLine(2, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn)
	kopi.Override = override(20000, "")
	tea.Discount = discount(DiscountPercent, 10, "")
	res, err := e.placeAs(sup, discount(DiscountFixed, 5000, ""), kopi, tea)
	if err != nil || !res.Applied {
		t.Fatalf("place: %v", err)
	}
	k, te := res.Lines[0], res.Lines[1]
	if k.UnitPrice != 20000 || k.PriceOverride == nil || k.PriceOverride.OriginalUnitPrice != 28000 ||
		k.PriceOverride.By.ID != sup.StaffID || k.PriceOverride.ApprovedBy.ID != sup.StaffID ||
		k.PriceOverride.By.Name != "Dewi" {
		t.Fatalf("kopi = %+v", k)
	}
	if te.Discount == nil || te.Discount.Amount != 1000 || te.LineTotal != 9000 ||
		te.Discount.ApprovedBy.ID != sup.StaffID {
		t.Fatalf("tea = %+v", te)
	}
	if res.Discount == nil || res.Discount.Amount != 5000 || res.DiscountTotal != 5000 || res.GrandTotal != 44000 {
		t.Fatalf("order = %+v", res.Order)
	}
	wantTotals(t, res.Detail)
	if k.OrderDiscountShare+te.OrderDiscountShare != 5000 {
		t.Fatalf("shares %d + %d", k.OrderDiscountShare, te.OrderDiscountShare)
	}
	a := controlAudits(t, res.ID)
	if len(a) != 3 {
		t.Fatalf("audits = %+v", a)
	}
	byAction := map[string]controlAuditRow{}
	for _, r := range a {
		byAction[r.Action] = r
		if r.ActorID != sup.StaffID || r.ApprovedBy != sup.StaffID || r.Before != nil || r.Reason == nil {
			t.Fatalf("audit %+v", r)
		}
	}
	if byAction["price.override"].ValueDelta != -16000 || byAction["line.discount"].ValueDelta != -1000 ||
		byAction["order.discount"].ValueDelta != -5000 || byAction["line.discount"].After["product_name"] != "Teh" ||
		byAction["order.discount"].After["order_number"] != res.OrderNumber {
		t.Fatalf("audits = %+v", a)
	}
	if o := outbox(t, res.ID); len(o) != 1 || o[0].EventType != "order.placed" {
		t.Fatalf("outbox = %+v", o)
	}
}

// AC-3, AC-4, E-27: a cashier places with a supervisor's approval per
// line; one approval naming a cashier refuses the whole order.
func TestPlaceStepUp(t *testing.T) {
	e := newEnv(t)
	sh := e.openShift()
	sup := e.supervisor("Dewi")
	other := e.staff("Budi", e.outlet, domain.RoleCashier, orderPerms...)
	lines := func(approver2 string) []LineInput {
		a, b := e.teaLine(1, FulfillmentDineIn), e.teaLine(2, FulfillmentDineIn)
		a.Discount, b.Discount = discount(DiscountPercent, 10, sup.StaffID), discount(DiscountFixed, 500, approver2)
		return []LineInput{a, b}
	}
	res, err := e.placeAs(e.cashier, nil, lines(sup.StaffID)...)
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	a := controlAudits(t, res.ID)
	if len(a) != 2 || a[0].ActorID != e.cashier.StaffID || a[0].ApprovedBy != sup.StaffID ||
		a[1].ActorID != e.cashier.StaffID || a[1].ApprovedBy != sup.StaffID {
		t.Fatalf("audits = %+v", a)
	}
	if res.Lines[0].Discount.By.ID != e.cashier.StaffID || res.Lines[0].Discount.ApprovedBy.ID != sup.StaffID {
		t.Fatalf("line discount = %+v", res.Lines[0].Discount)
	}
	before := sequence(t, sh.ID)
	_, err = e.placeAs(e.cashier, nil, lines(other.StaffID)...)
	wantCode(t, err, domain.CodeForbidden)
	_, err = e.placeAs(e.cashier, discount(DiscountPercent, 5, ""), e.teaLine(1, FulfillmentDineIn))
	wantCode(t, err, domain.CodeForbidden)
	if n := count(t, `SELECT count(*) FROM orders WHERE outlet_id = $1`, e.outlet); n != 1 {
		t.Fatalf("%d orders", n)
	}
	next := e.place()
	if sequence(t, sh.ID) != before+1 || !strings.HasSuffix(next.OrderNumber, "002") {
		t.Fatalf("next number %s", next.OrderNumber)
	}
}

// AC-5, E-4: a fixed line discount above the gross is refused, nothing
// written and no number used.
func TestPlaceDiscountExceeds(t *testing.T) {
	e := newEnv(t)
	sh := e.openShift()
	sup := e.supervisor("Dewi")
	ok, bad := e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn)
	bad.Discount = discount(DiscountFixed, 10001, "")
	_, err := e.placeAs(sup, nil, ok, bad)
	d := wantErr(t, err, domain.CodeDiscountExceedsTotal).Details.(map[string]any)
	if d["max"] != int64(10000) || d["line_id"] != bad.ID {
		t.Fatalf("details = %v", d)
	}
	_, err = e.placeAs(sup, discount(DiscountFixed, 20001, ""), e.teaLine(2, FulfillmentDineIn))
	d = wantErr(t, err, domain.CodeDiscountExceedsTotal).Details.(map[string]any)
	if d["max"] != int64(20000) || d["line_id"] != nil {
		t.Fatalf("order details = %v", d)
	}
	if n := count(t, `SELECT count(*) FROM orders WHERE outlet_id = $1`, e.outlet); n != 0 || sequence(t, sh.ID) != 0 {
		t.Fatalf("%d orders, sequence %d", n, sequence(t, sh.ID))
	}
}

// AC-6, E-12: add-lines applies a line discount; an order discount on
// add-lines is refused.
func TestAddLinesWithControls(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	o := e.place()
	l := e.teaLine(2, FulfillmentDineIn)
	l.Discount = discount(DiscountFixed, 3000, sup.StaffID)
	res, err := e.add(o.ID, l)
	if err != nil || res.Lines[1].DiscountAmount != 3000 || res.GrandTotal != 27000 {
		t.Fatalf("add: %+v, %v", res, err)
	}
	if a := controlAudits(t, o.ID); len(a) != 1 || a[0].Action != "line.discount" || a[0].ValueDelta != -3000 ||
		a[0].ApprovedBy != sup.StaffID {
		t.Fatalf("audits = %+v", a)
	}
	body := `{"lines":[{"id":"` + uuid.NewString() + `","product_id":"` + e.tea.String() +
		`","quantity":1,"fulfillment_type":"dine_in"}],"discount":{"type":"percent","value":10,"reason":"test"}}`
	rec := call(e.posRouter(e.outlet, true), "POST", "/v1/pos/orders/"+o.ID+"/lines", e.cashier.StaffID, body)
	if rec.Code != 400 || errorOf(t, rec)["fields"].(map[string]any)["discount"] == nil {
		t.Fatalf("order discount on add-lines: %d %s", rec.Code, rec.Body)
	}
}

// AC-7, E-34: a retried placement answers from the stored order without
// checking approvals again.
func TestPlaceRetryNoRecheck(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	l := e.teaLine(1, FulfillmentDineIn)
	l.Discount = discount(DiscountPercent, 50, sup.StaffID)
	in := &PlaceInput{Actor: e.actor(e.cashier), ID: uuid.NewString(), Lines: []LineInput{l}}
	if _, err := e.svc.Place(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	e.access[sup.StaffID] = &domain.StaffAccess{StaffID: sup.StaffID}
	res, err := e.svc.Place(e.ctx, in)
	if err != nil || res.Applied || res.GrandTotal != 5000 {
		t.Fatalf("retry: %+v, %v", res, err)
	}
	if a := controlAudits(t, in.ID); len(a) != 1 {
		t.Fatalf("audits = %+v", a)
	}
}

// AC-8: apply, replace and remove each control on an open bill.
func TestOpenBillControls(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	o := e.place(e.kopiLine(2, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn)) // 56,000 + 10,000
	kopi := o.Lines[0].ID
	steps := []struct {
		name   string
		run    func() (*Result, error)
		action string
		delta  int64
		lines  []any
		grand  int64
	}{
		{"order discount", func() (*Result, error) {
			in := e.ctl(sup, o.ID, "")
			in.Discount = discount(DiscountPercent, 10, "")
			return e.svc.SetOrderDiscount(e.ctx, in)
		}, "order.discount", -6600, []any{}, 59400},
		{"line discount", func() (*Result, error) {
			in := e.ctl(sup, o.ID, kopi)
			in.Discount = discount(DiscountFixed, 6000, "")
			return e.svc.SetLineDiscount(e.ctx, in)
		}, "line.discount", -5400, []any{kopi}, 54000},
		{"replace line discount", func() (*Result, error) {
			in := e.ctl(sup, o.ID, kopi)
			in.Discount = discount(DiscountPercent, 50, "")
			return e.svc.SetLineDiscount(e.ctx, in)
		}, "line.discount", -19800, []any{kopi}, 34200},
		{"override", func() (*Result, error) {
			in := e.ctl(sup, o.ID, kopi)
			in.Override = override(20000, "")
			return e.svc.SetPrice(e.ctx, in)
		}, "price.override", -7200, []any{kopi}, 27000},
		{"replace override", func() (*Result, error) {
			in := e.ctl(sup, o.ID, kopi)
			in.Override = override(30000, "")
			return e.svc.SetPrice(e.ctx, in)
		}, "price.override", 9000, []any{kopi}, 36000},
		{"replace order discount", func() (*Result, error) {
			in := e.ctl(sup, o.ID, "")
			in.Discount = discount(DiscountFixed, 1000, "")
			return e.svc.SetOrderDiscount(e.ctx, in)
		}, "order.discount", 3000, []any{}, 39000},
		{"remove override", func() (*Result, error) {
			return e.svc.RemovePrice(e.ctx, e.ctl(sup, o.ID, kopi))
		}, "price.override_removed", -2000, []any{kopi}, 37000},
		{"remove line discount", func() (*Result, error) {
			return e.svc.RemoveLineDiscount(e.ctx, e.ctl(sup, o.ID, kopi))
		}, "line.discount_removed", 28000, []any{kopi}, 65000},
		{"remove order discount", func() (*Result, error) {
			return e.svc.RemoveOrderDiscount(e.ctx, e.ctl(sup, o.ID, ""))
		}, "order.discount_removed", 1000, []any{}, 66000},
	}
	for i, s := range steps {
		res, err := s.run()
		if err != nil || !res.Applied || res.GrandTotal != s.grand {
			t.Fatalf("%s: %+v, %v", s.name, res, err)
		}
		wantTotals(t, res.Detail)
		a := controlAudits(t, o.ID)
		if len(a) != i+1 {
			t.Fatalf("%s: %d audits", s.name, len(a))
		}
		last := a[i]
		if last.Action != s.action || last.ValueDelta != s.delta || last.ApprovedBy != sup.StaffID {
			t.Fatalf("%s: audit %+v", s.name, last)
		}
		replace := strings.HasPrefix(s.name, "replace")
		removed := strings.HasPrefix(s.name, "remove")
		if (last.Before != nil) != (replace || removed) || (last.After == nil) != removed ||
			(last.Reason == nil) != removed {
			t.Fatalf("%s: before/after %+v", s.name, last)
		}
		ob := outbox(t, o.ID)
		ev := ob[len(ob)-1]
		if len(ob) != i+2 || ev.EventType != "order.updated" || len(ev.Payload["line_ids"].([]any)) != len(s.lines) {
			t.Fatalf("%s: outbox %+v", s.name, ob)
		}
	}
	if a := controlAudits(t, o.ID); a[2].Before["value"] != float64(6000) || a[2].After["value"] != float64(50) {
		t.Fatalf("replace audit = %+v", a[2])
	}
	final, _ := e.svc.Get(e.ctx, e.outlet, o.ID)
	if final.Discount != nil || final.Lines[0].Discount != nil || final.Lines[0].PriceOverride != nil ||
		final.Lines[0].UnitPrice != 28000 {
		t.Fatalf("final = %+v", final)
	}
}

// AC-9, AC-10, E-6..8, E-18: no-ops write nothing; an override to the
// quoted price removes the override.
func TestControlNoOps(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	o := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
	line := o.Lines[0].ID
	set := func(reason string) (*Result, error) {
		in := e.ctl(sup, o.ID, line)
		in.Discount = discount(DiscountPercent, 10, "")
		in.Discount.Reason = reason
		return e.svc.SetLineDiscount(e.ctx, in)
	}
	if res, err := set("first"); err != nil || !res.Applied {
		t.Fatalf("apply: %v", err)
	}
	audits, events := len(controlAudits(t, o.ID)), len(outbox(t, o.ID))
	noops := map[string]func() (*Result, error){
		"same discount": func() (*Result, error) { return set("first") },
		"new reason":    func() (*Result, error) { return set("another reason") },
		"remove none":   func() (*Result, error) { return e.svc.RemoveOrderDiscount(e.ctx, e.ctl(sup, o.ID, "")) },
		"remove no override": func() (*Result, error) {
			return e.svc.RemovePrice(e.ctx, e.ctl(sup, o.ID, line))
		},
		"override to the quoted price": func() (*Result, error) {
			in := e.ctl(sup, o.ID, line)
			in.Override = override(10000, "")
			return e.svc.SetPrice(e.ctx, in)
		},
		// No step-up is checked for a no-op (FR-STP-6).
		"cashier no-op": func() (*Result, error) { return e.svc.RemoveLineDiscount(e.ctx, e.ctl(e.cashier, o.ID, o.Lines[1].ID)) },
	}
	for name, fn := range noops {
		if res, err := fn(); err != nil || res.Applied {
			t.Fatalf("%s: %+v, %v", name, res, err)
		}
	}
	if len(controlAudits(t, o.ID)) != audits || len(outbox(t, o.ID)) != events {
		t.Fatal("a no-op wrote a row")
	}
	if d, _ := e.svc.Get(e.ctx, e.outlet, o.ID); d.Lines[0].Discount.Reason != "first" {
		t.Fatalf("reason = %q", d.Lines[0].Discount.Reason)
	}

	in := e.ctl(sup, o.ID, line)
	in.Override = override(8000, "")
	if _, err := e.svc.SetPrice(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Override = override(10000, "")
	res, err := e.svc.SetPrice(e.ctx, in)
	if err != nil || !res.Applied || res.Lines[0].PriceOverride != nil || res.Lines[0].UnitPrice != 10000 {
		t.Fatalf("override to original: %+v, %v", res, err)
	}
	a := controlAudits(t, o.ID)
	if last := a[len(a)-1]; last.Action != "price.override_removed" || last.Before["unit_price"] != float64(8000) {
		t.Fatalf("audit = %+v", last)
	}
}

// AC-11, E-17, FR-TOT-15: an override below a fixed line discount is
// refused and changes nothing.
func TestOverrideBelowFixedDiscount(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	l := e.teaLine(1, FulfillmentDineIn)
	l.Discount = discount(DiscountFixed, 5000, "")
	o, err := e.placeAs(sup, nil, l, e.teaLine(1, FulfillmentDineIn))
	if err != nil {
		t.Fatal(err)
	}
	in := e.ctl(sup, o.ID, l.ID)
	in.Override = override(4000, "")
	_, err = e.svc.SetPrice(e.ctx, in)
	d := wantErr(t, err, domain.CodeDiscountExceedsTotal).Details.(map[string]any)
	if d["max"] != int64(4000) || d["line_id"] != l.ID {
		t.Fatalf("details = %v", d)
	}
	after, _ := e.svc.Get(e.ctx, e.outlet, o.ID)
	if after.Lines[0].UnitPrice != 10000 || after.GrandTotal != o.GrandTotal || len(controlAudits(t, o.ID)) != 1 {
		t.Fatalf("changed: %+v", after)
	}
	// A fixed order discount above the subtotal is refused when applied.
	od := e.ctl(sup, o.ID, "")
	od.Discount = discount(DiscountFixed, 15001, "")
	_, err = e.svc.SetOrderDiscount(e.ctx, od)
	if d := wantErr(t, err, domain.CodeDiscountExceedsTotal).Details.(map[string]any); d["max"] != int64(15000) {
		t.Fatalf("order details = %v", d)
	}
}

// allControls runs every control route on one order (and line) as sup,
// over HTTP.
func allControls(e *env, h http.Handler, staffID, orderID, lineID, approver string) map[string]int {
	ap := ""
	if approver != "" {
		ap = `,"approved_by":"` + approver + `"`
	}
	disc := `{"type":"percent","value":10,"reason":"Regular customer"` + ap + `}`
	price := `{"unit_price":7000,"reason":"Wrong price"` + ap + `}`
	rm := `{` + strings.TrimPrefix(ap, ",") + `}`
	base := "/v1/pos/orders/" + orderID
	line := base + "/lines/" + lineID
	out := map[string]int{}
	for _, r := range []struct{ method, path, body string }{
		{"PUT", base + "/discount", disc}, {"DELETE", base + "/discount", rm},
		{"PUT", line + "/discount", disc}, {"DELETE", line + "/discount", rm},
		{"PUT", line + "/price", price}, {"DELETE", line + "/price", rm},
	} {
		out[r.method+" "+strings.TrimPrefix(r.path, base)] = call(h, r.method, r.path, staffID, r.body).Code
	}
	return out
}

// AC-12, E-30, E-31: controls on completed and cancelled orders, and on a
// voided line.
func TestControlsNotOpen(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	h := e.posRouter(e.outlet, true)
	paid := e.place(e.teaLine(1, FulfillmentDineIn))
	if _, err := e.pay(paid.ID, uuid.NewString(), 10000); err != nil {
		t.Fatal(err)
	}
	cancelled := e.place(e.teaLine(1, FulfillmentDineIn))
	if _, err := e.cancel(cancelled.ID, "left"); err != nil {
		t.Fatal(err)
	}
	for _, o := range []*Result{paid, cancelled} {
		for route, code := range allControls(e, h, sup.StaffID, o.ID, o.Lines[0].ID, "") {
			if code != 409 {
				t.Errorf("%s on %s: %d", route, o.Status, code)
			}
		}
	}
	open := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
	if _, err := e.void(open.ID, open.Lines[0].ID, "not wanted"); err != nil {
		t.Fatal(err)
	}
	for route, code := range allControls(e, h, sup.StaffID, open.ID, open.Lines[0].ID, "") {
		if strings.Contains(route, "/lines/") && code != 409 {
			t.Errorf("%s on a voided line: %d", route, code)
		}
	}
	in := e.ctl(sup, open.ID, open.Lines[0].ID)
	in.Discount = discount(DiscountPercent, 10, "")
	_, err := e.svc.SetLineDiscount(e.ctx, in)
	wantCode(t, err, domain.CodeLineNotActive)
	_, err = e.svc.SetOrderDiscount(e.ctx, &ControlInput{Actor: e.actor(sup), OrderID: paid.ID,
		Discount: discount(DiscountPercent, 10, "")})
	if d := wantErr(t, err, domain.CodeOrderNotOpen).Details.(map[string]string); d["status"] != StatusCompleted {
		t.Fatalf("details = %v", d)
	}
	if n := len(controlAudits(t, paid.ID)) + len(controlAudits(t, cancelled.ID)); n != 0 {
		t.Fatalf("%d audits on finished orders", n)
	}
	// Only the order discount routes reached the open order: applied, then
	// removed.
	if a := controlAudits(t, open.ID); len(a) != 2 || a[0].Action != "order.discount" ||
		a[1].Action != "order.discount_removed" {
		t.Fatalf("audits on the open order = %+v", a)
	}
}

// AC-13, E-21..26, E-28: the step-up table on the open-bill routes.
func TestControlStepUp(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	other := e.staff("Budi", e.outlet, domain.RoleCashier, orderPerms...)
	elsewhere := e.staff("Sari", e.newOutlet(), domain.RoleSupervisor, controlPerms...)
	gone := e.supervisor("Gone")
	gone.Active = false
	h := e.posRouter(e.outlet, true)
	for _, tt := range []struct {
		name            string
		actor, approver string
		want            int
	}{
		{"cashier alone", e.cashier.StaffID, "", 403},
		{"cashier approver", e.cashier.StaffID, other.StaffID, 403},
		{"approver at another outlet", e.cashier.StaffID, elsewhere.StaffID, 403},
		{"deactivated approver", e.cashier.StaffID, gone.StaffID, 403},
		{"unknown approver", e.cashier.StaffID, uuid.NewString(), 403},
		{"supervisor alone", sup.StaffID, "", 200},
		{"cashier with supervisor", e.cashier.StaffID, sup.StaffID, 200},
	} {
		o := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
		for route, code := range allControls(e, h, tt.actor, o.ID, o.Lines[0].ID, tt.approver) {
			want := tt.want
			if want == 403 && strings.HasPrefix(route, "DELETE") {
				want = 200 // nothing to remove: a no-op, checked without step-up (FR-STP-6)
			}
			if code != want {
				t.Errorf("%s: %s = %d", tt.name, route, code)
			}
		}
		if n := len(controlAudits(t, o.ID)); (n == 0) != (tt.want == 403) {
			t.Errorf("%s: %d audits", tt.name, n)
		}
		if tt.name == "cashier with supervisor" {
			for _, a := range controlAudits(t, o.ID) {
				if a.ActorID != e.cashier.StaffID || a.ApprovedBy != sup.StaffID {
					t.Errorf("audit %+v", a)
				}
			}
		}
	}

	// E-28: a cashier cannot remove a supervisor's discount alone.
	l := e.teaLine(1, FulfillmentDineIn)
	l.Discount = discount(DiscountPercent, 10, "")
	o, err := e.placeAs(sup, nil, l, e.teaLine(1, FulfillmentDineIn))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.RemoveLineDiscount(e.ctx, e.ctl(e.cashier, o.ID, l.ID))
	wantCode(t, err, domain.CodeForbidden)
	in := e.ctl(e.cashier, o.ID, l.ID)
	in.ApprovedBy = sup.StaffID
	if res, err := e.svc.RemoveLineDiscount(e.ctx, in); err != nil || !res.Applied {
		t.Fatalf("removal with approver: %v", err)
	}
}

// AC-14, E-10, E-32: a void reallocates the order discount, keeps the
// voided line's controls, and writes no control audit row.
func TestVoidWithControls(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	a, b, c := e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn)
	a.Discount = discount(DiscountFixed, 2000, "")
	o, err := e.placeAs(sup, discount(DiscountPercent, 10, ""), a, b, c)
	if err != nil || o.DiscountTotal != 2800 {
		t.Fatalf("place: %+v, %v", o, err)
	}
	before := len(controlAudits(t, o.ID))
	res, err := e.void(o.ID, a.ID, "not wanted")
	if err != nil || res.Subtotal != 20000 || res.DiscountTotal != 2000 || res.GrandTotal != 18000 {
		t.Fatalf("void: %+v, %v", res, err)
	}
	wantTotals(t, res.Detail)
	v := res.Lines[0]
	if v.Status != LineVoided || v.Discount == nil || v.DiscountAmount != 2000 || v.LineTotal != 8000 ||
		v.OrderDiscountShare != 0 {
		t.Fatalf("voided line = %+v", v)
	}
	if len(controlAudits(t, o.ID)) != before {
		t.Fatal("void wrote a control audit row")
	}
	if all := audits(t, o.ID); all[len(all)-1].Action != "order.line_voided" || all[len(all)-1].ValueDelta != -8000 {
		t.Fatalf("void audit = %+v", all[len(all)-1])
	}

	// E-10: a fixed order discount above the new subtotal is clamped.
	o2, err := e.placeAs(sup, discount(DiscountFixed, 20000, ""), e.teaLine(1, FulfillmentDineIn),
		e.teaLine(2, FulfillmentDineIn))
	if err != nil {
		t.Fatal(err)
	}
	res, err = e.void(o2.ID, o2.Lines[1].ID, "not wanted")
	if err != nil || res.DiscountTotal != 10000 || res.GrandTotal != 0 || res.Discount.Value != 20000 {
		t.Fatalf("clamp: %+v, %v", res, err)
	}
	wantTotals(t, res.Detail)
}

// AC-15, E-11: added lines raise a percentage order discount, with no
// audit row for it.
func TestAddLinesRecompute(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	o, err := e.placeAs(sup, discount(DiscountPercent, 10, ""), e.teaLine(1, FulfillmentDineIn))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.add(o.ID, e.kopiLine(1, FulfillmentTakeaway))
	if err != nil || res.DiscountTotal != 3800 || res.GrandTotal != 34200 {
		t.Fatalf("add: %+v, %v", res, err)
	}
	wantTotals(t, res.Detail)
	if a := controlAudits(t, o.ID); len(a) != 1 {
		t.Fatalf("audits = %+v", a)
	}
}

// AC-16, E-29: a discount and a payment racing on one order never leave a
// completed order whose payment differs from its grand total.
func TestDiscountRacesPayment(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	for range 10 {
		o := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
		var wg sync.WaitGroup
		var discErr, payErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			in := e.ctl(sup, o.ID, "")
			in.Discount = discount(DiscountPercent, 10, "")
			_, discErr = e.svc.SetOrderDiscount(e.ctx, in)
		}()
		go func() {
			defer wg.Done()
			_, payErr = e.pay(o.ID, uuid.NewString(), 20000)
		}()
		wg.Wait()
		switch {
		case discErr == nil && payErr != nil:
			wantCode(t, payErr, domain.CodeAmountMismatch)
		case payErr == nil && discErr != nil:
			wantCode(t, discErr, domain.CodeOrderNotOpen)
		default:
			t.Fatalf("discount %v, payment %v", discErr, payErr)
		}
		if n := count(t, `SELECT count(*) FROM orders o JOIN payments p ON p.order_id = o.id
			WHERE o.id = $1 AND p.amount <> o.grand_total`, o.ID); n != 0 {
			t.Fatal("payment differs from grand total")
		}
	}
}

// AC-17, E-33: cancelling keeps every control and total.
func TestCancelKeepsControls(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	l := e.kopiLine(1, FulfillmentDineIn)
	l.Override, l.Discount = override(25000, ""), discount(DiscountPercent, 20, "")
	o, err := e.placeAs(sup, discount(DiscountFixed, 1000, ""), l, e.teaLine(1, FulfillmentDineIn))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.cancel(o.ID, "left")
	if err != nil || res.Status != StatusCancelled || res.GrandTotal != o.GrandTotal ||
		res.DiscountTotal != o.DiscountTotal || res.Discount == nil || res.Lines[0].Discount == nil ||
		res.Lines[0].PriceOverride == nil || res.Lines[0].OrderDiscountShare != o.Lines[0].OrderDiscountShare {
		t.Fatalf("cancel: %+v, %v", res, err)
	}
	all := audits(t, o.ID)
	if last := all[len(all)-1]; last.Action != "order.cancelled" || last.ValueDelta != -o.GrandTotal {
		t.Fatalf("cancel audit = %+v", last)
	}
}

// countingQuerier counts the queries of a read.
type countingQuerier struct {
	q interface {
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
	n int
}

func (c *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	return c.q.Query(ctx, sql, args...)
}

func (c *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	return c.q.QueryRow(ctx, sql, args...)
}

// AC-18: reads return the controls with staff names, no unit_cost, and a
// fixed query count.
func TestReadControls(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	l := e.kopiLine(1, FulfillmentDineIn)
	l.Override, l.Discount = override(25000, ""), discount(DiscountFixed, 2000, sup.StaffID)
	o, err := e.placeAs(e.cashier, nil, e.teaLine(1, FulfillmentDineIn))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddLines(e.ctx, &AddLinesInput{Actor: e.actor(sup), OrderID: o.ID, Lines: []LineInput{l}}); err != nil {
		t.Fatal(err)
	}
	in := e.ctl(e.cashier, o.ID, "")
	in.Discount = discount(DiscountPercent, 10, sup.StaffID)
	if _, err := e.svc.SetOrderDiscount(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]http.Handler{
		"pos":       e.posRouter(e.outlet, true),
		"dashboard": e.dashRouter(e.cashier.StaffID),
	} {
		path, list := "/v1/pos/orders/"+o.ID, "/v1/pos/orders"
		if name == "dashboard" {
			path, list = "/v1/outlets/"+e.outlet+"/orders/"+o.ID, "/v1/outlets/"+e.outlet+"/orders"
		}
		rec := call(h, "GET", path, e.cashier.StaffID, "")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "unit_cost") {
			t.Fatalf("%s read: %d %s", name, rec.Code, rec.Body)
		}
		b := decodeBody(t, rec)
		od := b["discount"].(map[string]any)
		if od["type"] != "percent" || od["amount"] != b["discount_total"] ||
			od["by"].(map[string]any)["name"] != "Rina" || od["approved_by"].(map[string]any)["name"] != "Dewi" {
			t.Fatalf("%s order discount = %v", name, od)
		}
		lines := b["lines"].([]any)
		tea, kopi := lines[0].(map[string]any), lines[1].(map[string]any)
		if tea["discount"] != nil || tea["price_override"] != nil || tea["order_discount_share"] == float64(0) {
			t.Fatalf("%s tea = %v", name, tea)
		}
		kd, ko := kopi["discount"].(map[string]any), kopi["price_override"].(map[string]any)
		if kd["amount"] != float64(2000) || kd["by"].(map[string]any)["name"] != "Dewi" ||
			ko["original_unit_price"] != float64(28000) || kopi["unit_price"] != float64(25000) {
			t.Fatalf("%s kopi = %v", name, kopi)
		}
		rec = call(h, "GET", list, e.cashier.StaffID, "")
		first := decodeBody(t, rec)["orders"].([]any)[0].(map[string]any)
		if rec.Code != 200 || first["discount"].(map[string]any)["value"] != float64(10) {
			t.Fatalf("%s list: %d %s", name, rec.Code, rec.Body)
		}
	}
	queries := func(orderID string) int {
		c := &countingQuerier{q: testDB.Pool}
		if _, err := loadDetail(e.ctx, c, orderID, e.outlet); err != nil {
			t.Fatal(err)
		}
		return c.n
	}
	big := make([]LineInput, 20)
	for i := range big {
		big[i] = e.teaLine(1, FulfillmentDineIn)
		big[i].Discount = discount(DiscountPercent, 10, "")
	}
	bigOrder, err := e.placeAs(sup, nil, big...)
	if err != nil {
		t.Fatal(err)
	}
	if small, many := queries(o.ID), queries(bigOrder.ID); small != many {
		t.Fatalf("queries: %d for 2 lines, %d for 20", small, many)
	}
}

// AC-19 (handler side): validation, other outlets and revoked devices.
func TestControlRoutesAccess(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	o := e.place(e.teaLine(1, FulfillmentDineIn), e.teaLine(1, FulfillmentDineIn))
	h := e.posRouter(e.outlet, true)
	base := "/v1/pos/orders/" + o.ID
	for _, tt := range []struct{ method, path, body, field string }{
		{"PUT", base + "/discount", `{"type":"percent","value":101,"reason":"abc"}`, "value"},
		{"PUT", base + "/discount", `{"type":"fixed","value":100000001,"reason":"abc"}`, "value"},
		{"PUT", base + "/discount", `{"type":"half","value":1,"reason":"abc"}`, "type"},
		{"PUT", base + "/discount", `{"type":"percent","value":10,"reason":"  a  "}`, "reason"},
		{"PUT", base + "/discount", `{"type":"percent","value":10,"reason":"abc","approved_by":"x"}`, "approved_by"},
		{"PUT", base + "/lines/" + o.Lines[0].ID + "/price", `{"reason":"abc"}`, "unit_price"},
		{"PUT", base + "/lines/" + o.Lines[0].ID + "/price", `{"unit_price":-1,"reason":"abc"}`, "unit_price"},
		{"DELETE", base + "/discount", `{"approved_by":"x"}`, "approved_by"},
	} {
		rec := call(h, tt.method, tt.path, sup.StaffID, tt.body)
		if rec.Code != 400 || errorOf(t, rec)["fields"].(map[string]any)[tt.field] == nil {
			t.Errorf("%s %s: %d %s", tt.method, tt.body, rec.Code, rec.Body)
		}
	}
	body := `{"id":"` + uuid.NewString() + `","lines":[{"id":"` + uuid.NewString() + `","product_id":"` +
		e.tea.String() + `","quantity":1,"fulfillment_type":"dine_in","discount":{"type":"percent","value":0,"reason":"abc"}}]}`
	rec := call(h, "POST", "/v1/pos/orders", sup.StaffID, body)
	if rec.Code != 400 || errorOf(t, rec)["fields"].(map[string]any)["lines[0].discount.value"] == nil {
		t.Fatalf("placement validation: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "DELETE", base+"/discount", sup.StaffID, ""); rec.Code != 200 {
		t.Fatalf("DELETE with no body: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "PUT", base+"/lines/"+uuid.NewString()+"/discount", sup.StaffID,
		`{"type":"percent","value":10,"reason":"abc"}`); rec.Code != 404 {
		t.Fatalf("unknown line: %d", rec.Code)
	}
	big := `{"type":"percent","value":10,"reason":"` + strings.Repeat("a", 5000) + `"}`
	small := http.Handler(e.posRouter(e.outlet, true))
	if rec := call(small, "PUT", base+"/discount", sup.StaffID, big); rec.Code != 413 {
		t.Fatalf("oversized body: %d", rec.Code)
	}

	other := newEnv(t)
	other.openShift()
	osup := other.supervisor("Other")
	for route, code := range allControls(other, other.posRouter(other.outlet, true), osup.StaffID, o.ID, o.Lines[0].ID, "") {
		if code != 404 {
			t.Errorf("%s from another outlet: %d", route, code)
		}
	}
	revoked := e.posRouter(e.outlet, false)
	rec = call(revoked, "PUT", base+"/discount", sup.StaffID, `{"type":"percent","value":10,"reason":"abc"}`)
	if rec.Code != 401 || errorOf(t, rec)["code"] != "device_revoked" {
		t.Fatalf("revoked device: %d %s", rec.Code, rec.Body)
	}
	stranger := e.staff("Stranger", e.newOutlet(), domain.RoleSupervisor, controlPerms...)
	for route, code := range allControls(e, h, stranger.StaffID, o.ID, o.Lines[0].ID, "") {
		if code != 403 {
			t.Errorf("%s with no role at the outlet: %d", route, code)
		}
	}
}

// AC-20: an uncertain commit is 503, and the retried PUT applies the
// control exactly once.
func TestControlCommitUncertain(t *testing.T) {
	e := newEnv(t)
	e.openShift()
	sup := e.supervisor("Dewi")
	normal := e.svc
	for _, committed := range []bool{true, false} {
		o := e.place(e.teaLine(1, FulfillmentDineIn))
		e.svc = &Service{repo: &Repository{tx: uncertainRunner{commit: committed}, pool: testDB.Pool},
			access: e.access, now: normal.now}
		path, body := "/v1/pos/orders/"+o.ID+"/discount", `{"type":"percent","value":10,"reason":"Regular"}`
		rec := call(e.posRouter(e.outlet, true), "PUT", path, sup.StaffID, body)
		if rec.Code != 503 || errorOf(t, rec)["code"] != "order_unavailable" {
			t.Fatalf("committed=%v: %d %s", committed, rec.Code, rec.Body)
		}
		e.svc = normal
		rec = call(e.posRouter(e.outlet, true), "PUT", path, sup.StaffID, body)
		b := decodeBody(t, rec)
		if rec.Code != 200 || b["applied"] != !committed || b["grand_total"] != float64(9000) {
			t.Fatalf("retry, committed=%v: %d %s", committed, rec.Code, rec.Body)
		}
		if n := len(controlAudits(t, o.ID)); n != 1 {
			t.Fatalf("committed=%v: %d audits", committed, n)
		}
	}
}
