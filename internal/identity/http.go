package identity

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
)

// SessionCookie is the cookie a browser session is carried in.
const SessionCookie = "factorflow_session"

// Handler exposes the login flow.
type Handler struct {
	service *Service
	// Secure marks the session cookie as HTTPS-only. It is false in local development,
	// where there is no TLS, and true everywhere else.
	Secure bool
}

// NewHandler returns the handler.
func NewHandler(service *Service, secure bool) *Handler {
	return &Handler{service: service, Secure: secure}
}

// Routes registers the endpoints. They are public: proving who you are cannot require
// already being authenticated.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/auth/challenge", h.challenge)
	r.Post("/auth/verify", h.verify)
	r.Post("/auth/logout", h.logout)
	r.Get("/auth/me", h.me)
}

type challengeRequest struct {
	Wallet string `json:"wallet"`
}

type challengeResponse struct {
	Nonce     string `json:"nonce"`
	Message   string `json:"message"`
	ExpiresAt string `json:"expires_at"`
}

type verifyRequest struct {
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

type sessionResponse struct {
	// Token is returned as well as set as a cookie, so a machine client can hold it and a
	// browser client can ignore it.
	Token          string `json:"token"`
	OrganizationID string `json:"organization_id"`
	Wallet         string `json:"wallet"`
	ExpiresAt      string `json:"expires_at"`
}

type meResponse struct {
	OrganizationID string `json:"organization_id"`
	Wallet         string `json:"wallet"`
	Role           string `json:"role"`
	Eligible       bool   `json:"eligible"`
	Operator       bool   `json:"operator"`
}

func (h *Handler) challenge(w http.ResponseWriter, r *http.Request) {
	var body challengeRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	result, err := h.service.Challenge(r.Context(), body.Wallet)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	httpserver.WriteJSON(w, r, http.StatusCreated, challengeResponse{
		Nonce:     result.Nonce,
		Message:   result.Message,
		ExpiresAt: result.ExpiresAt.Format(time.RFC3339),
	})
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	var body verifyRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	session, err := h.service.Verify(r.Context(), body.Nonce, body.Signature)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    session.Token,
		Path:     "/",
		Expires:  session.ExpiresAt,
		HttpOnly: true,
		Secure:   h.Secure,
		// Strict rather than Lax: nothing in this API is meant to be reached by following a
		// link from another site, and it closes the cross-site request forgery hole that a
		// cookie session would otherwise open.
		SameSite: http.SameSiteStrictMode,
	})

	httpserver.WriteJSON(w, r, http.StatusCreated, sessionResponse{
		Token:          session.Token,
		OrganizationID: session.OrganizationID.String(),
		Wallet:         session.Wallet,
		ExpiresAt:      session.ExpiresAt.Format(time.RFC3339),
	})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		if cookie, err := r.Cookie(SessionCookie); err == nil {
			token = cookie.Value
		}
	}

	if err := h.service.Logout(r.Context(), token); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteStrictMode,
	})
	httpserver.WriteJSON(w, r, http.StatusNoContent, nil)
}

// me reports who the caller is, which is how a freshly loaded page finds out whether its
// cookie is still good.
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	actor := httpserver.ActorFrom(r.Context())
	if actor.IsZero() {
		httpserver.WriteProblem(w, r, httpserver.ErrUnauthorized)
		return
	}

	httpserver.WriteJSON(w, r, http.StatusOK, meResponse{
		OrganizationID: actor.OrganizationID.String(),
		Wallet:         actor.Wallet,
		Role:           actor.Role,
		Eligible:       actor.Eligible,
		Operator:       actor.Operator,
	})
}

// bearerToken extracts a token from the Authorization header.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
