package order

import (
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

const errUnavailable = "Orders are temporarily unavailable. Try again with the same IDs."

// fail writes err. A database that cannot be reached, or a commit whose
// outcome is unknown, becomes order_unavailable: the client retries with
// the same IDs, which is safe (NFR-AV-1).
func fail(w http.ResponseWriter, r *http.Request, err error) {
	if domain.AsError(err) == nil && (errors.Is(err, postgres.ErrCommitUncertain) || unreachable(err)) {
		err = domain.WrapError(domain.CodeOrderUnavailable, errUnavailable, err)
	}
	httpx.Error(w, r, err)
}

// unreachable reports an error from a database that could not be reached
// or did not answer in time.
func unreachable(err error) bool {
	var connErr *pgconn.ConnectError
	var netErr net.Error
	return errors.As(err, &connErr) || errors.As(err, &netErr) || pgconn.Timeout(err)
}

func respond[T any](w http.ResponseWriter, r *http.Request, status int, v T, err error) {
	if err != nil {
		fail(w, r, err)
		return
	}
	httpx.JSON(w, status, v)
}

// created is 201 for a change that created something, 200 for its retry.
func created(res *Result, err error) int {
	if err == nil && res.Applied {
		return http.StatusCreated
	}
	return http.StatusOK
}

// idParam reads a UUID route parameter; a malformed one is not_found, the
// same as an unknown one.
func idParam(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		httpx.Error(w, r, domain.ErrNotFound)
		return "", false
	}
	return id.String(), true
}

// actor is the device and acting staff member of a POS request.
func actor(r *http.Request) Actor {
	p := middleware.PrincipalFrom(r.Context())
	return Actor{OutletID: p.OutletID, DeviceID: p.DeviceID, Acting: middleware.ActingStaffFrom(r.Context())}
}

type lineInput struct {
	ID              string   `json:"id" validate:"required,uuid"`
	ProductID       string   `json:"product_id" validate:"required,uuid"`
	VariantID       *string  `json:"variant_id" validate:"omitempty,uuid"`
	ModifierIDs     []string `json:"modifier_ids" validate:"dive,uuid"`
	Quantity        int      `json:"quantity" validate:"min=1,max=999"`
	FulfillmentType string   `json:"fulfillment_type" validate:"required,oneof=dine_in takeaway"`
	Note            *string  `json:"note" validate:"omitempty,max=280"`
}

type placeInput struct {
	ID          string      `json:"id" validate:"required,uuid"`
	TableNumber *string     `json:"table_number" validate:"omitempty,max=20"`
	Note        *string     `json:"note" validate:"omitempty,max=280"`
	Lines       []lineInput `json:"lines" validate:"required,min=1,max=100"`
}

type addLinesInput struct {
	Lines []lineInput `json:"lines" validate:"required,min=1,max=100"`
}

type reasonInput struct {
	Reason string `json:"reason" validate:"required,min=3,max=280"`
}

type paymentInput struct {
	ID          string  `json:"id" validate:"required,uuid"`
	Method      string  `json:"method" validate:"required,oneof=qris_static card_edc"`
	Amount      *int64  `json:"amount" validate:"required,min=0"`
	ExternalRef *string `json:"external_ref" validate:"omitempty,max=64"`
}

// validLines validates each line, reporting fields as lines[i].field, and
// converts them (FR-PLC-2).
func validLines(in []lineInput) ([]LineInput, error) {
	bad := map[string]string{}
	out := make([]LineInput, len(in))
	for i := range in {
		l := &in[i]
		if err := httpx.Validate(l); err != nil {
			prefix := "lines[" + strconv.Itoa(i) + "]."
			for f, msg := range domain.AsError(err).Fields {
				bad[prefix+f] = msg
			}
			continue
		}
		out[i] = LineInput{
			ID: canonical(l.ID), ProductID: uuid.MustParse(l.ProductID), Quantity: l.Quantity,
			FulfillmentType: l.FulfillmentType, Note: l.Note, ModifierIDs: make([]uuid.UUID, len(l.ModifierIDs)),
		}
		if l.VariantID != nil {
			v := uuid.MustParse(*l.VariantID)
			out[i].VariantID = &v
		}
		for j, m := range l.ModifierIDs {
			out[i].ModifierIDs[j] = uuid.MustParse(m)
		}
	}
	if len(bad) > 0 {
		return nil, domain.ValidationError(bad)
	}
	return out, nil
}

// canonical lower-cases a validated UUID, so one ID has one spelling.
func canonical(id string) string { return uuid.MustParse(id).String() }

// decodeReason reads a void or cancel reason, trimmed (FR-VOID-1,
// FR-CAN-1).
func decodeReason(w http.ResponseWriter, r *http.Request) (string, error) {
	var in reasonInput
	if err := httpx.Decode(w, r, &in); err != nil {
		return "", err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if err := httpx.Validate(&in); err != nil {
		return "", err
	}
	return in.Reason, nil
}

// POSPlace processes a ticket: 201 when placed, 200 for a retry.
func (h *Handler) POSPlace(w http.ResponseWriter, r *http.Request) {
	var in placeInput
	if err := httpx.DecodeValid(w, r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	lines, err := validLines(in.Lines)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.Place(r.Context(), &PlaceInput{
		Actor: actor(r), ID: canonical(in.ID), TableNumber: in.TableNumber, Note: in.Note, Lines: lines,
	})
	respond(w, r, created(res, err), res, err)
}

// POSAddLines adds lines to an open bill: 201 when added, 200 for a retry.
func (h *Handler) POSAddLines(w http.ResponseWriter, r *http.Request) {
	orderID, ok := idParam(w, r, "orderID")
	if !ok {
		return
	}
	var in addLinesInput
	if err := httpx.DecodeValid(w, r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	lines, err := validLines(in.Lines)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.AddLines(r.Context(), &AddLinesInput{Actor: actor(r), OrderID: orderID, Lines: lines})
	respond(w, r, created(res, err), res, err)
}

func (h *Handler) POSVoidLine(w http.ResponseWriter, r *http.Request) {
	orderID, ok := idParam(w, r, "orderID")
	if !ok {
		return
	}
	lineID, ok := idParam(w, r, "lineID")
	if !ok {
		return
	}
	reason, err := decodeReason(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.VoidLine(r.Context(), &VoidInput{Actor: actor(r), OrderID: orderID, LineID: lineID, Reason: reason})
	respond(w, r, http.StatusOK, res, err)
}

func (h *Handler) POSCancel(w http.ResponseWriter, r *http.Request) {
	orderID, ok := idParam(w, r, "orderID")
	if !ok {
		return
	}
	reason, err := decodeReason(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.Cancel(r.Context(), &CancelInput{Actor: actor(r), OrderID: orderID, Reason: reason})
	respond(w, r, http.StatusOK, res, err)
}

// POSPay records the payment: 201 when recorded, 200 for a retry.
func (h *Handler) POSPay(w http.ResponseWriter, r *http.Request) {
	orderID, ok := idParam(w, r, "orderID")
	if !ok {
		return
	}
	var in paymentInput
	if err := httpx.DecodeValid(w, r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.Pay(r.Context(), &PayInput{
		Actor: actor(r), OrderID: orderID, PaymentID: canonical(in.ID), Method: in.Method,
		Amount: *in.Amount, ExternalRef: in.ExternalRef,
	})
	respond(w, r, created(res, err), res, err)
}

// POSList lists the device's outlet's orders of one shift (FR-RD-1).
func (h *Handler) POSList(w http.ResponseWriter, r *http.Request) {
	f, err := listFilter(r, middleware.PrincipalFrom(r.Context()).OutletID, false)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page, err := h.svc.ListPOS(r.Context(), f)
	respond(w, r, http.StatusOK, page, err)
}

func (h *Handler) POSGet(w http.ResponseWriter, r *http.Request) {
	orderID, ok := idParam(w, r, "orderID")
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), middleware.PrincipalFrom(r.Context()).OutletID, orderID)
	respond(w, r, http.StatusOK, d, err)
}

// List serves the dashboard's order listing for an outlet (FR-RD-3).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	outletID, ok := idParam(w, r, "outletID")
	if !ok {
		return
	}
	f, err := listFilter(r, outletID, true)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page, err := h.svc.ListDashboard(r.Context(), f)
	respond(w, r, http.StatusOK, page, err)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	outletID, ok := idParam(w, r, "outletID")
	if !ok {
		return
	}
	orderID, ok := idParam(w, r, "orderID")
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), outletID, orderID)
	respond(w, r, http.StatusOK, d, err)
}

// posStatuses are the statuses a POS order can have in this slice.
var posStatuses = []string{StatusPlaced, StatusCompleted, StatusCancelled}

// listFilter reads and validates a listing's query parameters. Dates and
// the order number are dashboard filters only.
func listFilter(r *http.Request, outletID string, dashboard bool) (*ListFilter, error) {
	q := r.URL.Query()
	f := &ListFilter{OutletID: outletID, Status: q.Get("status")}
	bad := map[string]string{}
	statuses := posStatuses
	if dashboard {
		statuses = allStatuses
		f.From, f.To, f.OrderNumber = q.Get("from"), q.Get("to"), q.Get("order_number")
		for _, k := range []string{"from", "to"} {
			if v := q.Get(k); v != "" && !isDate(v) {
				bad[k] = "must be a date as YYYY-MM-DD"
			}
		}
	}
	if f.Status != "" && !slices.Contains(statuses, f.Status) {
		bad["status"] = "must be one of: " + strings.Join(statuses, " ")
	}
	if raw := q.Get("shift_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			bad["shift_id"] = "must be a UUID"
		} else {
			f.ShiftID = id.String()
		}
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPageSize {
			bad["limit"] = "must be between 1 and " + strconv.Itoa(maxPageSize)
		}
		f.Limit = n
	}
	if raw := q.Get("cursor"); raw != "" {
		c, ok := parseCursor(raw)
		if !ok {
			bad["cursor"] = "is invalid"
		}
		f.After = c
	}
	if len(bad) > 0 {
		return nil, domain.ValidationError(bad)
	}
	return f, nil
}

func isDate(s string) bool {
	_, err := time.Parse(dateLayout, s)
	return err == nil
}
