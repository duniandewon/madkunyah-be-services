package staff

import (
	"context"
	"net/http"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

var validRoles = map[string]bool{
	domain.RoleCashier: true, domain.RoleSupervisor: true,
	domain.RoleManager: true, domain.RoleOwner: true,
}

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type createBody struct {
	FullName string `json:"full_name" validate:"required,max=200"`
	Phone    string `json:"phone"`
	Email    string `json:"email"`
	Role     string `json:"role" validate:"required,oneof=cashier supervisor manager owner"`
}

type pinBody struct {
	PIN string `json:"pin" validate:"required,len=6,numeric"`
}

func (h *Handler) SetPIN(w http.ResponseWriter, r *http.Request) {
	var body pinBody
	if err := httpx.DecodeValid(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	staffID := middleware.PrincipalFrom(r.Context()).Subject
	if err := h.svc.SetPIN(r.Context(), staffID, body.PIN, meta(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) ResetPIN(w http.ResponseWriter, r *http.Request) {
	staffID, ok := staffIDParam(w, r)
	if !ok {
		return
	}
	if err := h.svc.ResetPIN(r.Context(), middleware.AccessFrom(r.Context()), staffID, meta(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	members, err := h.svc.List(r.Context(), chi.URLParam(r, "outletID"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string][]Member{"staff": members})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var body createBody
	if err := httpx.DecodeValid(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	m, err := h.svc.Create(r.Context(), middleware.AccessFrom(r.Context()), chi.URLParam(r, "outletID"),
		CreateInput(body))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, m)
}

func (h *Handler) GrantRole(w http.ResponseWriter, r *http.Request) {
	h.roleChange(w, r, h.svc.GrantRole)
}

func (h *Handler) RevokeRole(w http.ResponseWriter, r *http.Request) {
	h.roleChange(w, r, h.svc.RevokeRole)
}

func (h *Handler) GrantGlobalRole(w http.ResponseWriter, r *http.Request) {
	h.globalRoleChange(w, r, h.svc.GrantGlobalRole)
}

func (h *Handler) RevokeGlobalRole(w http.ResponseWriter, r *http.Request) {
	h.globalRoleChange(w, r, h.svc.RevokeGlobalRole)
}

func (h *Handler) Deactivate(w http.ResponseWriter, r *http.Request) {
	h.staffAction(w, r, h.svc.Deactivate)
}

func (h *Handler) RevokeSessions(w http.ResponseWriter, r *http.Request) {
	h.staffAction(w, r, h.svc.RevokeSessions)
}

type roleChangeFunc func(ctx context.Context, actor *domain.StaffAccess, staffID string, g grant) error

func (h *Handler) roleChange(w http.ResponseWriter, r *http.Request, fn roleChangeFunc) {
	staffID, role, ok := staffAndRole(w, r)
	if !ok {
		return
	}
	g := grant{OutletID: chi.URLParam(r, "outletID"), Role: role}
	if err := fn(r.Context(), middleware.AccessFrom(r.Context()), staffID, g); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

type globalRoleChangeFunc func(ctx context.Context, actor *domain.StaffAccess, staffID, role string) error

func (h *Handler) globalRoleChange(w http.ResponseWriter, r *http.Request, fn globalRoleChangeFunc) {
	staffID, role, ok := staffAndRole(w, r)
	if !ok {
		return
	}
	if err := fn(r.Context(), middleware.AccessFrom(r.Context()), staffID, role); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

type staffActionFunc func(ctx context.Context, actor *domain.StaffAccess, staffID string) error

func (h *Handler) staffAction(w http.ResponseWriter, r *http.Request, fn staffActionFunc) {
	staffID, ok := staffIDParam(w, r)
	if !ok {
		return
	}
	if err := fn(r.Context(), middleware.AccessFrom(r.Context()), staffID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// staffIDParam rejects a malformed ID as not_found, the same as an unknown one.
func staffIDParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "staffID")
	if _, err := uuid.Parse(id); err != nil {
		httpx.Error(w, r, domain.ErrNotFound)
		return "", false
	}
	return id, true
}

func meta(r *http.Request) RequestMeta {
	return RequestMeta{IP: httpx.ClientIP(r), UserAgent: r.UserAgent()}
}

func staffAndRole(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	staffID, ok := staffIDParam(w, r)
	if !ok {
		return "", "", false
	}
	role := chi.URLParam(r, "role")
	if !validRoles[role] {
		httpx.Error(w, r, domain.ValidationError(map[string]string{
			"role": "must be one of: cashier supervisor manager owner",
		}))
		return "", "", false
	}
	return staffID, role, true
}
