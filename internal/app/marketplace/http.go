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
	"github.com/GoldFridge/factorflow/internal/settlement"
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
	r.Post("/auctions/{auctionID}/cancel", h.cancel)
	r.Post("/auctions/{auctionID}/settle", h.settle)
	r.Get("/auctions/{auctionID}/settlements", h.settlements)
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

// cancelRequest carries why a batch was withdrawn.
type cancelRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	auctionID, err := uuid.Parse(chi.URLParam(r, "auctionID"))
	if err != nil {
		httpserver.WriteProblem(w, r, apperr.Invalid("auctionID", "must be a UUID"))
		return
	}

	var body cancelRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	actor := httpserver.ActorFrom(r.Context())
	cancelled, err := h.service.CancelAuction(r.Context(), Actor{
		OrganizationID: actor.OrganizationID,
		Operator:       actor.Operator,
	}, auctionID, body.Reason)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, auction.Describe(cancelled))
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

// settlementResponse is the wire shape of one transfer.
//
// The saga state is published rather than reduced to done/not-done: a participant waiting
// on a transfer is entitled to know whether it has been submitted, confirmed, or is
// waiting for the chain to answer.
type settlementResponse struct {
	ID          string `json:"id"`
	AuctionID   string `json:"auction_id"`
	InvoiceID   string `json:"invoice_id"`
	AssetID     string `json:"asset_id"`
	InvestorID  string `json:"investor_id"`
	FromWallet  string `json:"from_wallet"`
	ToWallet    string `json:"to_wallet"`
	Notional    string `json:"notional"`
	Price       string `json:"price"`
	Currency    string `json:"currency"`
	OperationID string `json:"operation_id"`
	TxID        string `json:"tx_id,omitempty"`
	State       string `json:"state"`
	Attempts    int    `json:"attempts"`
	// LastError is an operator-facing cause, published to the batch's own issuer so a
	// stuck transfer can be understood without reading the logs.
	LastError string `json:"last_error,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

type settlementsResponse struct {
	Items []settlementResponse `json:"items"`
}

func (h *Handler) settle(w http.ResponseWriter, r *http.Request) {
	auctionID, err := uuid.Parse(chi.URLParam(r, "auctionID"))
	if err != nil {
		httpserver.WriteProblem(w, r, apperr.Invalid("auctionID", "must be a UUID"))
		return
	}

	actor := httpserver.ActorFrom(r.Context())
	planned, err := h.service.SettleAuction(r.Context(), Actor{
		OrganizationID: actor.OrganizationID,
		Operator:       actor.Operator,
	}, auctionID, httpserver.TraceIDFrom(r.Context()))
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	// 202: the transfers are planned and queued, not finished. Reporting 200 would claim
	// the assets had moved while the chain had not yet been asked.
	httpserver.WriteJSON(w, r, http.StatusAccepted, toSettlementsResponse(planned))
}

func (h *Handler) settlements(w http.ResponseWriter, r *http.Request) {
	auctionID, err := uuid.Parse(chi.URLParam(r, "auctionID"))
	if err != nil {
		httpserver.WriteProblem(w, r, apperr.Invalid("auctionID", "must be a UUID"))
		return
	}

	actor := httpserver.ActorFrom(r.Context())
	planned, err := h.service.Settlements(r.Context(), Actor{
		OrganizationID: actor.OrganizationID,
		Operator:       actor.Operator,
	}, auctionID)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toSettlementsResponse(planned))
}

func toSettlementsResponse(plans []*settlement.Settlement) settlementsResponse {
	items := make([]settlementResponse, 0, len(plans))
	for _, p := range plans {
		items = append(items, settlementResponse{
			ID:          p.ID.String(),
			AuctionID:   p.AuctionID.String(),
			InvoiceID:   p.InvoiceID.String(),
			AssetID:     p.AssetID.String(),
			InvestorID:  p.InvestorID.String(),
			FromWallet:  p.FromWallet,
			ToWallet:    p.ToWallet,
			Notional:    p.Notional.String(),
			Price:       p.Price.String(),
			Currency:    p.Notional.Currency().String(),
			OperationID: p.OperationID,
			TxID:        p.TxID,
			State:       p.State.String(),
			Attempts:    p.Attempts,
			LastError:   p.LastError,
			UpdatedAt:   p.UpdatedAt.Format(time.RFC3339),
		})
	}
	return settlementsResponse{Items: items}
}
