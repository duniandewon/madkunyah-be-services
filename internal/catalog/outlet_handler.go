package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/go-chi/chi/v5"
)

// posSoldOutInput carries the device time of the tap (FR-SO-6).
type posSoldOutInput struct {
	SoldOutInput
	ChangedAt *time.Time `json:"changed_at" validate:"required"`
}

func channelParam(w http.ResponseWriter, r *http.Request) (Channel, bool) {
	switch c := Channel(chi.URLParam(r, "channel")); c {
	case ChannelWeb, ChannelPOS:
		return c, true
	}
	httpx.Error(w, r, domain.ValidationError(map[string]string{"channel": "must be web or pos"}))
	return "", false
}

// etagMatches reports whether an If-None-Match header names etag, weak or
// strong, or is "*".
func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// writeTagged writes a JSON body under etag, or 304 when the client
// already holds it (FR-MENU-6, FR-BND-4). Clients revalidate every time.
func writeTagged(w http.ResponseWriter, r *http.Request, etag string, body []byte) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.ErrorContext(r.Context(), "write response", "error", err)
	}
}

func (h *Handler) PublicOutlets(w http.ResponseWriter, r *http.Request) {
	outlets, err := h.svc.PublicOutlets(r.Context())
	respond(w, r, http.StatusOK, map[string][]PublicOutlet{"outlets": outlets}, err)
}

func (h *Handler) Menu(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "outletID")
	if !ok {
		return
	}
	m, err := h.svc.Menu(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	body, err := json.Marshal(m)
	if err != nil {
		fail(w, r, err)
		return
	}
	sum := sha256.Sum256(body)
	writeTagged(w, r, `"`+hex.EncodeToString(sum[:16])+`"`, body)
}

// Bundle serves the device's own outlet, taken from its token, and records
// the version it now holds, on 304 as well as 200.
func (h *Handler) Bundle(w http.ResponseWriter, r *http.Request) {
	p := middleware.PrincipalFrom(r.Context())
	body, version, err := h.svc.Bundle(r.Context(), p.OutletID)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := h.svc.RecordBundleFetch(r.Context(), p.DeviceID, version); err != nil {
		slog.WarnContext(r.Context(), "record bundle fetch failed", "device_id", p.DeviceID, "error", err)
	}
	writeTagged(w, r, `"`+version+`"`, body)
}

func (h *Handler) OutletCatalog(w http.ResponseWriter, r *http.Request) {
	outletID, ok := idParam(w, r, "outletID")
	if !ok {
		return
	}
	archived, ok := includeArchived(w, r)
	if !ok {
		return
	}
	products, err := h.svc.OutletCatalog(r.Context(), access(r), outletID, archived)
	respond(w, r, http.StatusOK, map[string]any{"outlet_id": outletID, "products": products}, err)
}

// settingsTarget reads the outlet, product and channel of a settings route.
func settingsTarget(w http.ResponseWriter, r *http.Request) (string, string, Channel, bool) {
	outletID, ok := idParam(w, r, "outletID")
	if !ok {
		return "", "", "", false
	}
	productID, ok := idParam(w, r, "productID")
	if !ok {
		return "", "", "", false
	}
	ch, ok := channelParam(w, r)
	return outletID, productID, ch, ok
}

func (h *Handler) SetChannelSettings(w http.ResponseWriter, r *http.Request) {
	outletID, productID, ch, ok := settingsTarget(w, r)
	if !ok {
		return
	}
	var in ChannelSettingsInput
	if !decode(w, r, &in) {
		return
	}
	res, err := h.svc.SetChannelSettings(r.Context(), access(r), outletID, productID, ch, &in)
	respond(w, r, http.StatusOK, res, err)
}

func (h *Handler) ResetChannelSettings(w http.ResponseWriter, r *http.Request) {
	outletID, productID, ch, ok := settingsTarget(w, r)
	if !ok {
		return
	}
	if err := h.svc.ResetChannelSettings(r.Context(), access(r), outletID, productID, ch); err != nil {
		fail(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) ListSoldOut(w http.ResponseWriter, r *http.Request) {
	outletID, ok := idParam(w, r, "outletID")
	if !ok {
		return
	}
	items, err := h.svc.ListSoldOut(r.Context(), outletID)
	respond(w, r, http.StatusOK, map[string][]SoldOutEntry{"items": items}, err)
}

// SetSoldOut is the dashboard toggle; it is ordered by server time.
func (h *Handler) SetSoldOut(w http.ResponseWriter, r *http.Request) {
	outletID, ok := idParam(w, r, "outletID")
	if !ok {
		return
	}
	var in SoldOutInput
	if !decode(w, r, &in) {
		return
	}
	sw, err := NewSoldOutWrite(outletID, &in, access(r).StaffID, ViaDashboard)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.SetSoldOut(r.Context(), sw)
	respond(w, r, http.StatusOK, res, err)
}

// POSSetSoldOut toggles at the device's outlet on behalf of the acting
// staff member, whose permission is checked here, server side, not from
// the roster (FR-SO-5).
func (h *Handler) POSSetSoldOut(w http.ResponseWriter, r *http.Request) {
	p := middleware.PrincipalFrom(r.Context())
	acting := middleware.ActingStaffFrom(r.Context())
	if !acting.Can(p.OutletID, domain.PermSoldOutToggle) {
		httpx.Error(w, r, domain.ErrForbidden)
		return
	}
	var in posSoldOutInput
	if !decode(w, r, &in) {
		return
	}
	sw, err := NewSoldOutWrite(p.OutletID, &in.SoldOutInput, acting.StaffID, ViaPOS)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	deviceID := p.DeviceID
	sw.DeviceID, sw.ChangedAt = &deviceID, *in.ChangedAt
	res, err := h.svc.SetSoldOut(r.Context(), sw)
	respond(w, r, http.StatusOK, res, err)
}
