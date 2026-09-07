package marketplace

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
)

// Handler exposes the one endpoint that spans modules: opening an auction for invoices.
//
// Everything that happens to the batch afterwards belongs to the auction module's own
// handler, which is registered beside this one.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Routes registers the endpoint.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/auctions", h.open)
	r.Post("/auctions/{auctionID}/clear", h.clear)
}

// openRequest names the invoices to list and the bidding window.
type openRequest struct {
	InvoiceIDs []string `json:"invoice_ids"`
	OpensAt    string   `json:"opens_at"`
	ClosesAt   string   `json:"closes_at"`
}

func (h *Handler) open(w http.ResponseWriter, r *http.Request) {
	var body openRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	params, err := body.toParams()
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	actor := httpserver.ActorFrom(r.Context())
	created, err := h.service.OpenAuction(r.Context(), Actor{
		OrganizationID: actor.OrganizationID,
		Operator:       actor.Operator,
	}, params)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	// The response is the auction module's own shape, so a client parses one auction
	// representation whichever endpoint returned it.
	httpserver.WriteJSON(w, r, http.StatusCreated, auction.Describe(created))
}

func (h *Handler) clear(w http.ResponseWriter, r *http.Request) {
	auctionID, err := uuid.Parse(chi.URLParam(r, "auctionID"))
	if err != nil {
		httpserver.WriteProblem(w, r, apperr.Invalid("auctionID", "must be a UUID"))
		return
	}

	actor := httpserver.ActorFrom(r.Context())
	solution, err := h.service.ClearAuction(r.Context(), Actor{
		OrganizationID: actor.OrganizationID,
		Operator:       actor.Operator,
	}, auctionID)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, auction.DescribeSolution(solution))
}

func (b openRequest) toParams() (OpenParams, error) {
	var violations []error

	if len(b.InvoiceIDs) == 0 {
		violations = append(violations, apperr.Invalid("invoice_ids", "must name at least one invoice"))
	}

	ids := make([]uuid.UUID, 0, len(b.InvoiceIDs))
	for _, raw := range b.InvoiceIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			violations = append(violations, apperr.Invalid("invoice_ids", "%q is not a UUID", raw))
			continue
		}
		ids = append(ids, id)
	}

	opensAt, err := parseTime("opens_at", b.OpensAt)
	if err != nil {
		violations = append(violations, err)
	}
	closesAt, err := parseTime("closes_at", b.ClosesAt)
	if err != nil {
		violations = append(violations, err)
	}

	if err := errors.Join(violations...); err != nil {
		return OpenParams{}, err
	}
	return OpenParams{InvoiceIDs: ids, OpensAt: opensAt, ClosesAt: closesAt}, nil
}

func parseTime(field, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, apperr.Invalid(field, "must be an RFC 3339 timestamp")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, apperr.Invalid(field, "must be an RFC 3339 timestamp")
	}
	return parsed.UTC(), nil
}
