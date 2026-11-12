package shift

import (
	"errors"
	"net"
	"net/http"
	"strconv"
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

const errUnavailable = "Shifts are temporarily unavailable. Try again."

// fail writes err. A database that cannot be reached, or a commit whose
// outcome is unknown, becomes shift_unavailable: the client retries with
// the same shift ID, which is safe (NFR-AV-1).
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var connErr *pgconn.ConnectError
	var netErr net.Error
	if domain.AsError(err) == nil && (errors.Is(err, postgres.ErrCommitUncertain) ||
		errors.As(err, &connErr) || errors.As(err, &netErr) || pgconn.Timeout(err)) {
		err = domain.WrapError(domain.CodeShiftUnavailable, errUnavailable, err)
	}
	httpx.Error(w, r, err)
}

func respond[T any](w http.ResponseWriter, r *http.Request, status int, v T, err error) {
	if err != nil {
		fail(w, r, err)
		return
	}
	httpx.JSON(w, status, v)
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

type openInput struct {
	ID         string `json:"id" validate:"required,uuid"`
	ApprovedBy string `json:"approved_by" validate:"omitempty,uuid"`
}

type closeInput struct {
	ApprovedBy string `json:"approved_by" validate:"omitempty,uuid"`
}

// canonical lower-cases a validated UUID, so one ID has one spelling.
func canonical(id string) string {
	if id == "" {
		return ""
	}
	return uuid.MustParse(id).String()
}

// POSCurrent tells the device whether its outlet has an open shift
// (FR-RD-1).
func (h *Handler) POSCurrent(w http.ResponseWriter, r *http.Request) {
	p := middleware.PrincipalFrom(r.Context())
	s, err := h.svc.Current(r.Context(), p.OutletID)
	respond(w, r, http.StatusOK, map[string]*Shift{"shift": s}, err)
}

// POSOpen opens a shift at the device's outlet on behalf of the acting
// staff member: 201 when created, 200 for a retry of the open shift.
func (h *Handler) POSOpen(w http.ResponseWriter, r *http.Request) {
	var in openInput
	if err := httpx.DecodeValid(w, r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.PrincipalFrom(r.Context())
	res, err := h.svc.Open(r.Context(), &OpenInput{
		ID: canonical(in.ID), OutletID: p.OutletID, DeviceID: p.DeviceID,
		Acting: middleware.ActingStaffFrom(r.Context()), ApprovedBy: canonical(in.ApprovedBy),
	})
	status := http.StatusOK
	if err == nil && res.Applied {
		status = http.StatusCreated
	}
	respond(w, r, status, res, err)
}

// POSClose closes a shift of the device's outlet. The body is optional: it
// only carries the approver.
func (h *Handler) POSClose(w http.ResponseWriter, r *http.Request) {
	shiftID, ok := idParam(w, r, "shiftID")
	if !ok {
		return
	}
	var in closeInput
	if r.ContentLength != 0 {
		if err := httpx.DecodeValid(w, r, &in); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	p := middleware.PrincipalFrom(r.Context())
	res, err := h.svc.Close(r.Context(), &CloseInput{
		ShiftID: shiftID, OutletID: p.OutletID, DeviceID: p.DeviceID,
		Acting: middleware.ActingStaffFrom(r.Context()), ApprovedBy: canonical(in.ApprovedBy),
	})
	respond(w, r, http.StatusOK, res, err)
}

// List serves the dashboard's shift listing for an outlet (FR-RD-2).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	outletID, ok := idParam(w, r, "outletID")
	if !ok {
		return
	}
	f, err := listFilter(r, outletID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page, err := h.svc.List(r.Context(), f)
	respond(w, r, http.StatusOK, page, err)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	outletID, ok := idParam(w, r, "outletID")
	if !ok {
		return
	}
	shiftID, ok := idParam(w, r, "shiftID")
	if !ok {
		return
	}
	s, err := h.svc.Get(r.Context(), outletID, shiftID)
	respond(w, r, http.StatusOK, s, err)
}

// listFilter reads and validates the listing's query parameters.
func listFilter(r *http.Request, outletID string) (*ListFilter, error) {
	q := r.URL.Query()
	f := &ListFilter{OutletID: outletID, From: q.Get("from"), To: q.Get("to"), Status: q.Get("status")}
	bad := map[string]string{}

	from, fromOK := parseDate(f.From)
	to, toOK := parseDate(f.To)
	if !fromOK {
		bad["from"] = "must be a date as YYYY-MM-DD"
	}
	if !toOK {
		bad["to"] = "must be a date as YYYY-MM-DD"
	}
	if fromOK && toOK && f.From != "" && f.To != "" {
		switch days := int(to.Sub(from).Hours() / 24); {
		case days < 0:
			bad["to"] = "must not be before from"
		case days > maxRangeDays:
			bad["to"] = "must be at most " + strconv.Itoa(maxRangeDays) + " days after from"
		}
	}
	if f.Status != "" && f.Status != StatusOpen && f.Status != StatusClosed {
		bad["status"] = "must be one of: open closed"
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

// parseDate accepts an empty value as "no bound".
func parseDate(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(dateLayout, s)
	return t, err == nil
}
