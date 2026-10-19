package pos

import (
	"net/http"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type pinFailure struct {
	ID         string    `json:"id" validate:"required,uuid"`
	StaffID    string    `json:"staff_id" validate:"required,uuid"`
	OccurredAt time.Time `json:"occurred_at" validate:"required"`
}

type pinFailuresBody struct {
	Failures []pinFailure `json:"failures" validate:"required,min=1,max=500,dive"`
}

// Roster serves the roster of the device's own outlet, taken from its token.
func (h *Handler) Roster(w http.ResponseWriter, r *http.Request) {
	p := middleware.PrincipalFrom(r.Context())
	roster, err := h.svc.Roster(r.Context(), p.OutletID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// The roster carries PIN hashes: keep it out of every cache.
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, roster)
}

func (h *Handler) PINFailures(w http.ResponseWriter, r *http.Request) {
	var body pinFailuresBody
	if err := httpx.DecodeValid(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	failures := make([]PINFailure, len(body.Failures))
	for i, f := range body.Failures {
		failures[i] = PINFailure(f)
	}
	p := middleware.PrincipalFrom(r.Context())
	if err := h.svc.RecordPINFailures(r.Context(), p.DeviceID, failures); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}
