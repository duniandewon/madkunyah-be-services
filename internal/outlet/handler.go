package outlet

import (
	"context"
	"net/http"

	"github.com/duniandewon/madkunyah-be-service/internal/auth"
	"github.com/duniandewon/madkunyah-be-service/internal/catalog"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// body is the create and PATCH body. Every field tells absent from null
// (FR-OTL-7); unknown fields are refused by the decoder.
type body struct {
	Name              catalog.Nullable[string] `json:"name"`
	Address           catalog.Nullable[string] `json:"address"`
	Phone             catalog.Nullable[string] `json:"phone"`
	Timezone          catalog.Nullable[string] `json:"timezone"`
	BusinessDayCutoff catalog.Nullable[string] `json:"business_day_cutoff"`
	OrderPrefix       catalog.Nullable[string] `json:"order_prefix"`
}

func (b *body) fields() (*Fields, error) {
	bad := map[string]string{}
	opt := func(name string, n catalog.Nullable[string]) Opt {
		if n.Bad {
			bad[name] = "expected a string"
		}
		return Opt{Set: n.Set, Null: n.Set && !n.Valid, V: n.V}
	}
	f := &Fields{
		Name: opt("name", b.Name), Address: opt("address", b.Address), Phone: opt("phone", b.Phone),
		Timezone: opt("timezone", b.Timezone), BusinessDayCutoff: opt("business_day_cutoff", b.BusinessDayCutoff),
		OrderPrefix: opt("order_prefix", b.OrderPrefix),
	}
	if len(bad) > 0 {
		return nil, domain.ValidationError(bad)
	}
	return f, nil
}

func decodeFields(w http.ResponseWriter, r *http.Request) (*Fields, error) {
	var b body
	if err := httpx.Decode(w, r, &b); err != nil {
		return nil, err
	}
	return b.fields()
}

// outletID reads a canonical {outletID}; a malformed one is not found.
func outletID(r *http.Request) (string, error) {
	id, err := uuid.Parse(chi.URLParam(r, "outletID"))
	if err != nil {
		return "", domain.ErrNotFound
	}
	return id.String(), nil
}

func respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, status, v)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	f, err := decodeFields(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	o, err := h.svc.Create(r.Context(), middleware.AccessFrom(r.Context()), f)
	respond(w, r, http.StatusCreated, o, err)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := outletID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	o, err := h.svc.Get(r.Context(), id)
	respond(w, r, http.StatusOK, o, err)
}

// Mine lists the outlets the caller reaches (FR-OTL-5).
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	outlets, err := h.svc.ListMine(r.Context(), middleware.AccessFrom(r.Context()))
	respond(w, r, http.StatusOK, map[string][]Outlet{"outlets": outlets}, err)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := outletID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f, err := decodeFields(w, r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	o, err := h.svc.Update(r.Context(), middleware.AccessFrom(r.Context()), id, f)
	respond(w, r, http.StatusOK, o, err)
}

func (h *Handler) Deactivate(w http.ResponseWriter, r *http.Request) {
	h.setActive(w, r, func(ctx context.Context, a *domain.StaffAccess, id string) (*Result, error) {
		return h.svc.Deactivate(ctx, a, id, auth.RequestMeta{IP: httpx.ClientIP(r), UserAgent: r.UserAgent()})
	})
}

func (h *Handler) Activate(w http.ResponseWriter, r *http.Request) {
	h.setActive(w, r, h.svc.Activate)
}

func (h *Handler) setActive(w http.ResponseWriter, r *http.Request,
	fn func(context.Context, *domain.StaffAccess, string) (*Result, error)) {
	id, err := outletID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := fn(r.Context(), middleware.AccessFrom(r.Context()), id)
	respond(w, r, http.StatusOK, res, err)
}
