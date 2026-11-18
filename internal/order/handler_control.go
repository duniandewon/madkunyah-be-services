package order

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
)

// discountInput is a line or order discount (pos-controls FR-CTL-1). The
// value's range depends on the type, so it is checked in validDiscount.
type discountInput struct {
	Type       string  `json:"type" validate:"required,oneof=percent fixed"`
	Value      int64   `json:"value"`
	Reason     string  `json:"reason" validate:"required,min=3,max=280"`
	ApprovedBy *string `json:"approved_by" validate:"omitempty,uuid"`
}

// overrideInput is a price override (FR-CTL-2).
type overrideInput struct {
	UnitPrice  *int64  `json:"unit_price" validate:"required,min=0"`
	Reason     string  `json:"reason" validate:"required,min=3,max=280"`
	ApprovedBy *string `json:"approved_by" validate:"omitempty,uuid"`
}

// removeInput is the optional body of a DELETE: only the approver
// (FR-OPN-1).
type removeInput struct {
	ApprovedBy *string `json:"approved_by" validate:"omitempty,uuid"`
}

// validDiscount validates a discount, reporting fields as prefix.field
// into bad, and converts it. A nil discount is none.
func (h *Handler) validDiscount(prefix string, d *discountInput, bad map[string]string) *DiscountInput {
	if d == nil {
		return nil
	}
	d.Reason = strings.TrimSpace(d.Reason)
	ok := collect(prefix, httpx.Validate(d), bad)
	upper := map[string]int64{DiscountPercent: 100, DiscountFixed: h.priceMax}[d.Type]
	if upper > 0 && (d.Value < 1 || d.Value > upper) {
		bad[field(prefix, "value")] = "must be between 1 and " + strconv.FormatInt(upper, 10)
		ok = false
	}
	if !ok {
		return nil
	}
	return &DiscountInput{Type: d.Type, Value: d.Value, Reason: d.Reason, ApprovedBy: approverID(d.ApprovedBy)}
}

// validOverride validates a price override like validDiscount.
func (h *Handler) validOverride(prefix string, o *overrideInput, bad map[string]string) *OverrideInput {
	if o == nil {
		return nil
	}
	o.Reason = strings.TrimSpace(o.Reason)
	ok := collect(prefix, httpx.Validate(o), bad)
	if o.UnitPrice != nil && *o.UnitPrice > h.priceMax {
		bad[field(prefix, "unit_price")] = "must be at most " + strconv.FormatInt(h.priceMax, 10)
		ok = false
	}
	if !ok {
		return nil
	}
	return &OverrideInput{UnitPrice: *o.UnitPrice, Reason: o.Reason, ApprovedBy: approverID(o.ApprovedBy)}
}

// collect adds a validation error's fields to bad under prefix and
// reports whether there were none.
func collect(prefix string, err error, bad map[string]string) bool {
	if err == nil {
		return true
	}
	de := domain.AsError(err)
	if de == nil || len(de.Fields) == 0 {
		bad[field(prefix, "")] = "is invalid"
		return false
	}
	for f, msg := range de.Fields {
		bad[field(prefix, f)] = msg
	}
	return false
}

// field names a control's field under its prefix; a route body is the
// control itself, with no prefix.
func field(prefix, name string) string {
	switch {
	case prefix == "":
		return name
	case name == "":
		return prefix
	}
	return prefix + "." + name
}

// approverID is a validated approved_by in canonical form, or "".
func approverID(id *string) string {
	if id == nil {
		return ""
	}
	return canonical(*id)
}

// controlRoute reads a control route's IDs and runs fn; every outcome,
// applied or not, is 200 (FR-OPN-8).
func (h *Handler) controlRoute(w http.ResponseWriter, r *http.Request, line bool,
	body func(*ControlInput) error, fn func(context.Context, *ControlInput) (*Result, error)) {
	orderID, ok := idParam(w, r, "orderID")
	if !ok {
		return
	}
	in := &ControlInput{Actor: actor(r), OrderID: orderID}
	if line {
		if in.LineID, ok = idParam(w, r, "lineID"); !ok {
			return
		}
	}
	if err := body(in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := fn(r.Context(), in)
	respond(w, r, http.StatusOK, res, err)
}

// discountBody reads a discount PUT body.
func (h *Handler) discountBody(w http.ResponseWriter, r *http.Request) func(*ControlInput) error {
	return func(in *ControlInput) error {
		var d discountInput
		if err := httpx.Decode(w, r, &d); err != nil {
			return err
		}
		bad := map[string]string{}
		in.Discount = h.validDiscount("", &d, bad)
		return invalid(bad)
	}
}

// priceBody reads an override PUT body.
func (h *Handler) priceBody(w http.ResponseWriter, r *http.Request) func(*ControlInput) error {
	return func(in *ControlInput) error {
		var o overrideInput
		if err := httpx.Decode(w, r, &o); err != nil {
			return err
		}
		bad := map[string]string{}
		in.Override = h.validOverride("", &o, bad)
		return invalid(bad)
	}
}

// removeBody reads a DELETE body, which is optional and carries only the
// approver.
func removeBody(w http.ResponseWriter, r *http.Request) func(*ControlInput) error {
	return func(in *ControlInput) error {
		var rm removeInput
		if r.ContentLength != 0 {
			if err := httpx.DecodeValid(w, r, &rm); err != nil {
				return err
			}
		}
		in.ApprovedBy = approverID(rm.ApprovedBy)
		return nil
	}
}

func (h *Handler) POSSetOrderDiscount(w http.ResponseWriter, r *http.Request) {
	h.controlRoute(w, r, false, h.discountBody(w, r), h.svc.SetOrderDiscount)
}

func (h *Handler) POSRemoveOrderDiscount(w http.ResponseWriter, r *http.Request) {
	h.controlRoute(w, r, false, removeBody(w, r), h.svc.RemoveOrderDiscount)
}

func (h *Handler) POSSetLineDiscount(w http.ResponseWriter, r *http.Request) {
	h.controlRoute(w, r, true, h.discountBody(w, r), h.svc.SetLineDiscount)
}

func (h *Handler) POSRemoveLineDiscount(w http.ResponseWriter, r *http.Request) {
	h.controlRoute(w, r, true, removeBody(w, r), h.svc.RemoveLineDiscount)
}

func (h *Handler) POSSetPrice(w http.ResponseWriter, r *http.Request) {
	h.controlRoute(w, r, true, h.priceBody(w, r), h.svc.SetPrice)
}

func (h *Handler) POSRemovePrice(w http.ResponseWriter, r *http.Request) {
	h.controlRoute(w, r, true, removeBody(w, r), h.svc.RemovePrice)
}
