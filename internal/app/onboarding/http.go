package onboarding

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
)

// Handler exposes registration and the eligibility decisions.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// PublicRoutes registers the endpoints a caller reaches before they have a session.
//
// Registration is one of them by necessity: a wallet with no organization cannot
// authenticate, so it proves itself with a signature instead.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Post("/organizations", h.register)
}

// Routes registers the endpoints that require a session.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/organizations", h.list)
	r.Get("/organizations/{organizationID}", h.get)
	r.Post("/organizations/{organizationID}/approve", h.approve)
	r.Post("/organizations/{organizationID}/reject", h.reject)
}

type registerRequest struct {
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
	Type      string `json:"type"`
	Name      string `json:"name"`
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

type organizationResponse struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Wallet      string `json:"wallet"`
	Eligibility string `json:"eligibility"`
	Reason      string `json:"reason,omitempty"`
	CanIssue    bool   `json:"can_issue"`
	CanInvest   bool   `json:"can_invest"`
	Version     int64  `json:"version"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type listResponse struct {
	Items []organizationResponse `json:"items"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var body registerRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	orgType, err := organization.ParseType(body.Type)
	if err != nil {
		httpserver.WriteProblem(w, r, apperr.Invalid("type", "must be one of ISSUER, INVESTOR, OPERATOR"))
		return
	}

	org, err := h.service.Register(r.Context(), RegisterParams{
		Nonce:     body.Nonce,
		Signature: body.Signature,
		Type:      orgType,
		Name:      body.Name,
	})
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusCreated, toResponse(org))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := organizationIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	org, err := h.service.Get(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toResponse(org))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			httpserver.WriteProblem(w, r, apperr.Invalid("limit", "must be a positive integer"))
			return
		}
		limit = parsed
	}

	organizations, err := h.service.List(r.Context(), actorOf(r), organization.Type(r.URL.Query().Get("type")), limit)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	items := make([]organizationResponse, 0, len(organizations))
	for _, org := range organizations {
		items = append(items, toResponse(org))
	}
	httpserver.WriteJSON(w, r, http.StatusOK, listResponse{Items: items})
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	id, err := organizationIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	org, err := h.service.Approve(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toResponse(org))
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request) {
	id, err := organizationIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	var body reasonRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	org, err := h.service.Reject(r.Context(), actorOf(r), id, body.Reason)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toResponse(org))
}

func toResponse(org *organization.Organization) organizationResponse {
	return organizationResponse{
		ID:          org.ID.String(),
		Type:        org.Type.String(),
		Name:        org.Name,
		Wallet:      org.Wallet,
		Eligibility: org.Eligibility.String(),
		Reason:      org.Reason,
		CanIssue:    org.CanIssue(),
		CanInvest:   org.CanInvest(),
		Version:     org.Version,
		CreatedAt:   org.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   org.UpdatedAt.Format(time.RFC3339),
	}
}

func organizationIDOf(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "organizationID"))
	if err != nil {
		return uuid.Nil, apperr.Invalid("organization_id", "must be a UUID")
	}
	return id, nil
}

func actorOf(r *http.Request) Actor {
	actor := httpserver.ActorFrom(r.Context())
	return Actor{OrganizationID: actor.OrganizationID, Operator: actor.Operator}
}
