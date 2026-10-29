package catalog

import (
	"errors"
	"net"
	"net/http"
	"strconv"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

const errUnavailable = "The catalog is temporarily unavailable. Try again shortly."

// fail writes err, reporting a lost Postgres connection as
// catalog_unavailable (NFR-AV-1) rather than an internal error.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var connErr *pgconn.ConnectError
	var netErr net.Error
	if domain.AsError(err) == nil &&
		(errors.As(err, &connErr) || errors.As(err, &netErr) || pgconn.Timeout(err)) {
		err = domain.WrapError(domain.CodeCatalogUnavailable, errUnavailable, err)
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

func includeArchived(w http.ResponseWriter, r *http.Request) (bool, bool) {
	raw := r.URL.Query().Get("include_archived")
	if raw == "" {
		return false, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		httpx.Error(w, r, domain.ValidationError(map[string]string{"include_archived": "must be true or false"}))
		return false, false
	}
	return v, true
}

func access(r *http.Request) *domain.StaffAccess { return middleware.AccessFrom(r.Context()) }

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := httpx.DecodeValid(w, r, dst); err != nil {
		httpx.Error(w, r, err)
		return false
	}
	return true
}

func (h *Handler) ListCategories(w http.ResponseWriter, r *http.Request) {
	archived, ok := includeArchived(w, r)
	if !ok {
		return
	}
	cats, err := h.svc.ListCategories(r.Context(), archived)
	respond(w, r, http.StatusOK, map[string][]Category{"categories": cats}, err)
}

func (h *Handler) GetCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "categoryID")
	if !ok {
		return
	}
	c, err := h.svc.GetCategory(r.Context(), id)
	respond(w, r, http.StatusOK, c, err)
}

func (h *Handler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	var in CategoryInput
	if !decode(w, r, &in) {
		return
	}
	c, err := h.svc.CreateCategory(r.Context(), access(r), &in)
	respond(w, r, http.StatusCreated, c, err)
}

func (h *Handler) UpdateCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "categoryID")
	if !ok {
		return
	}
	var in CategoryPatch
	if !decode(w, r, &in) {
		return
	}
	c, err := h.svc.UpdateCategory(r.Context(), access(r), id, &in)
	respond(w, r, http.StatusOK, c, err)
}

func (h *Handler) ListProducts(w http.ResponseWriter, r *http.Request) {
	archived, ok := includeArchived(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := ProductFilter{Query: q.Get("q"), IncludeArchived: archived}
	if len([]rune(f.Query)) > maxSearchLen {
		httpx.Error(w, r, domain.ValidationError(map[string]string{"q": "must be at most 100 characters"}))
		return
	}
	if raw := q.Get("category_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, r, domain.ValidationError(map[string]string{"category_id": "must be a UUID"}))
			return
		}
		s := id.String()
		f.CategoryID = &s
	}
	products, err := h.svc.ListProducts(r.Context(), access(r), f)
	respond(w, r, http.StatusOK, map[string][]Product{"products": products}, err)
}

func (h *Handler) GetProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "productID")
	if !ok {
		return
	}
	p, err := h.svc.GetProduct(r.Context(), access(r), id)
	respond(w, r, http.StatusOK, p, err)
}

func (h *Handler) CreateProduct(w http.ResponseWriter, r *http.Request) {
	var in ProductInput
	if !decode(w, r, &in) {
		return
	}
	p, err := h.svc.CreateProduct(r.Context(), access(r), &in)
	respond(w, r, http.StatusCreated, p, err)
}

func (h *Handler) UpdateProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "productID")
	if !ok {
		return
	}
	var in ProductPatch
	if !decode(w, r, &in) {
		return
	}
	p, err := h.svc.UpdateProduct(r.Context(), access(r), id, &in)
	respond(w, r, http.StatusOK, p, err)
}

func (h *Handler) SetProductGroups(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "productID")
	if !ok {
		return
	}
	var in AttachInput
	if !decode(w, r, &in) {
		return
	}
	groups, err := h.svc.SetProductGroups(r.Context(), access(r), id, &in)
	respond(w, r, http.StatusOK, map[string][]AttachedGroup{"modifier_groups": groups}, err)
}

func (h *Handler) CreateVariant(w http.ResponseWriter, r *http.Request) {
	productID, ok := idParam(w, r, "productID")
	if !ok {
		return
	}
	var in VariantInput
	if !decode(w, r, &in) {
		return
	}
	v, err := h.svc.CreateVariant(r.Context(), access(r), productID, &in)
	respond(w, r, http.StatusCreated, v, err)
}

func (h *Handler) UpdateVariant(w http.ResponseWriter, r *http.Request) {
	productID, ok := idParam(w, r, "productID")
	if !ok {
		return
	}
	id, ok := idParam(w, r, "variantID")
	if !ok {
		return
	}
	var in VariantPatch
	if !decode(w, r, &in) {
		return
	}
	v, err := h.svc.UpdateVariant(r.Context(), access(r), productID, id, &in)
	respond(w, r, http.StatusOK, v, err)
}

func (h *Handler) ListGroups(w http.ResponseWriter, r *http.Request) {
	archived, ok := includeArchived(w, r)
	if !ok {
		return
	}
	groups, err := h.svc.ListGroups(r.Context(), archived)
	respond(w, r, http.StatusOK, map[string][]ModifierGroup{"modifier_groups": groups}, err)
}

func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "groupID")
	if !ok {
		return
	}
	g, err := h.svc.GetGroup(r.Context(), id)
	respond(w, r, http.StatusOK, g, err)
}

func (h *Handler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var in GroupInput
	if !decode(w, r, &in) {
		return
	}
	g, err := h.svc.CreateGroup(r.Context(), access(r), &in)
	respond(w, r, http.StatusCreated, g, err)
}

func (h *Handler) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "groupID")
	if !ok {
		return
	}
	var in GroupPatch
	if !decode(w, r, &in) {
		return
	}
	g, err := h.svc.UpdateGroup(r.Context(), access(r), id, &in)
	respond(w, r, http.StatusOK, g, err)
}

func (h *Handler) CreateModifier(w http.ResponseWriter, r *http.Request) {
	groupID, ok := idParam(w, r, "groupID")
	if !ok {
		return
	}
	var in ModifierInput
	if !decode(w, r, &in) {
		return
	}
	m, err := h.svc.CreateModifier(r.Context(), access(r), groupID, &in)
	respond(w, r, http.StatusCreated, m, err)
}

func (h *Handler) UpdateModifier(w http.ResponseWriter, r *http.Request) {
	groupID, ok := idParam(w, r, "groupID")
	if !ok {
		return
	}
	id, ok := idParam(w, r, "modifierID")
	if !ok {
		return
	}
	var in ModifierPatch
	if !decode(w, r, &in) {
		return
	}
	m, err := h.svc.UpdateModifier(r.Context(), access(r), groupID, id, &in)
	respond(w, r, http.StatusOK, m, err)
}
