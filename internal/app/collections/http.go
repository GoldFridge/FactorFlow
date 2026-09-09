package collections

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/redemption"
)

// Handler exposes what happens at maturity.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Routes registers the endpoints.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/invoices/{invoiceID}/repayment", h.record)
	r.Get("/invoices/{invoiceID}/repayment", h.get)
	r.Post("/invoices/{invoiceID}/default", h.markDefault)
	r.Get("/repayments", h.received)
	r.Get("/holdings", h.holdings)
}

// recordRequest is a payment the debtor made.
//
// The amount is a decimal string with its currency beside it, like every other amount in
// this API: a JSON number would have lost precision before validation could object.
type recordRequest struct {
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
	Reference  string `json:"reference"`
	ReceivedAt string `json:"received_at"`
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

// shareResponse is one holder's part of a payment.
type shareResponse struct {
	PartyID  string `json:"party_id"`
	Notional string `json:"notional"`
	Amount   string `json:"amount"`
}

// repaymentResponse is the wire shape of a repayment and its division.
type repaymentResponse struct {
	ID        string `json:"id"`
	InvoiceID string `json:"invoice_id"`

	Face      string `json:"face"`
	Amount    string `json:"amount"`
	Shortfall string `json:"shortfall"`
	Currency  string `json:"currency"`
	// Shortfall reports the money as a number; this reports what it means.
	IsShortfall bool `json:"is_shortfall"`

	Reference  string `json:"reference"`
	ReceivedAt string `json:"received_at"`
	RecordedBy string `json:"recorded_by"`

	Shares    []shareResponse `json:"shares"`
	CreatedAt string          `json:"created_at"`
}

type listResponse struct {
	Items []repaymentResponse `json:"items"`
}

// holdingResponse is one position an investor bought and what has become of it.
type holdingResponse struct {
	SettlementID string `json:"settlement_id"`
	InvoiceID    string `json:"invoice_id"`
	AuctionID    string `json:"auction_id"`

	Number    string `json:"number"`
	DebtorRef string `json:"debtor_ref"`
	DueAt     string `json:"due_at"`
	Status    string `json:"status"`

	// Notional is the face value held; Price is what was paid for it. The difference is
	// the return, and it is left as two numbers rather than one so nobody has to trust
	// this endpoint's arithmetic.
	Notional string `json:"notional"`
	Price    string `json:"price"`
	Currency string `json:"currency"`

	// State is where the transfer itself got to. A position still in flight says so.
	State    string `json:"state"`
	Settled  bool   `json:"settled"`
	TxID     string `json:"tx_id,omitempty"`
	SettleAt string `json:"settled_at"`

	// Received is this holder's share of the debtor's payment, once there is one.
	Received    string `json:"received,omitempty"`
	IsShortfall bool   `json:"is_shortfall,omitempty"`
	ReceivedAt  string `json:"received_at,omitempty"`
}

type holdingsResponse struct {
	Items []holdingResponse `json:"items"`
}

func (h *Handler) record(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	var body recordRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	params, err := body.toParams(id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	result, err := h.service.Record(r.Context(), actorOf(r), params)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusCreated, toResponse(result.Repayment))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	repayment, err := h.service.Get(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toResponse(repayment))
}

func (h *Handler) markDefault(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	var body reasonRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	inv, err := h.service.Default(r.Context(), actorOf(r), id, body.Reason)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, map[string]any{
		"invoice_id": inv.ID.String(),
		"status":     inv.Status.String(),
		"reason":     inv.Reason,
	})
}

func (h *Handler) received(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			httpserver.WriteProblem(w, r, apperr.Invalid("limit", "must be a positive integer"))
			return
		}
		limit = parsed
	}

	repayments, err := h.service.Received(r.Context(), actorOf(r), limit)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	items := make([]repaymentResponse, 0, len(repayments))
	for _, repayment := range repayments {
		items = append(items, toResponse(repayment))
	}
	httpserver.WriteJSON(w, r, http.StatusOK, listResponse{Items: items})
}

func (h *Handler) holdings(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			httpserver.WriteProblem(w, r, apperr.Invalid("limit", "must be a positive integer"))
			return
		}
		limit = parsed
	}

	held, err := h.service.Holdings(r.Context(), actorOf(r), limit)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	actor := actorOf(r)
	items := make([]holdingResponse, 0, len(held))
	for _, holding := range held {
		items = append(items, toHoldingResponse(holding, actor.OrganizationID))
	}
	httpserver.WriteJSON(w, r, http.StatusOK, holdingsResponse{Items: items})
}

func toHoldingResponse(holding *Holding, reader uuid.UUID) holdingResponse {
	transfer, inv := holding.Settlement, holding.Invoice

	out := holdingResponse{
		SettlementID: transfer.ID.String(),
		InvoiceID:    inv.ID.String(),
		AuctionID:    transfer.AuctionID.String(),

		Number:    inv.Number,
		DebtorRef: inv.DebtorRef,
		DueAt:     inv.DueAt.Format(time.RFC3339),
		Status:    inv.Status.String(),

		Notional: transfer.Notional.String(),
		Price:    transfer.Price.String(),
		Currency: transfer.Notional.Currency().String(),

		State:    transfer.State.String(),
		Settled:  transfer.IsFinished(),
		TxID:     transfer.TxID,
		SettleAt: transfer.UpdatedAt.Format(time.RFC3339),
	}

	if holding.Repayment != nil {
		if share, ok := holding.Repayment.ShareOf(reader); ok {
			out.Received = share.Amount.String()
		}
		out.IsShortfall = holding.Repayment.IsShortfall()
		out.ReceivedAt = holding.Repayment.ReceivedAt.Format(time.RFC3339)
	}
	return out
}

// toParams turns the request into the domain's own types, failing before anything is
// touched rather than half way through.
func (b recordRequest) toParams(invoiceID uuid.UUID) (RecordParams, error) {
	currency, err := money.ParseCurrency(b.Currency)
	if err != nil {
		return RecordParams{}, apperr.Invalid("currency", "must be a supported ISO 4217 code")
	}
	amount, err := money.Parse(b.Amount, currency)
	if err != nil {
		return RecordParams{}, apperr.Invalid("amount",
			"must be a decimal amount with at most %d places", currency.Exponent())
	}

	receivedAt := time.Now().UTC()
	if b.ReceivedAt != "" {
		receivedAt, err = time.Parse(time.RFC3339, b.ReceivedAt)
		if err != nil {
			return RecordParams{}, apperr.Invalid("received_at", "must be an RFC 3339 timestamp")
		}
	}

	return RecordParams{
		InvoiceID:  invoiceID,
		Amount:     amount,
		Reference:  b.Reference,
		ReceivedAt: receivedAt,
	}, nil
}

func toResponse(r *redemption.Repayment) repaymentResponse {
	shares := make([]shareResponse, 0, len(r.Shares))
	for _, share := range r.Shares {
		shares = append(shares, shareResponse{
			PartyID:  share.PartyID.String(),
			Notional: share.Notional.String(),
			Amount:   share.Amount.String(),
		})
	}

	return repaymentResponse{
		ID:        r.ID.String(),
		InvoiceID: r.InvoiceID.String(),

		Face:        r.Face.String(),
		Amount:      r.Amount.String(),
		Shortfall:   r.Shortfall().String(),
		Currency:    r.Amount.Currency().String(),
		IsShortfall: r.IsShortfall(),

		Reference:  r.Reference,
		ReceivedAt: r.ReceivedAt.Format(time.RFC3339),
		RecordedBy: r.RecordedBy.String(),

		Shares:    shares,
		CreatedAt: r.CreatedAt.Format(time.RFC3339),
	}
}

func invoiceIDOf(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "invoiceID"))
	if err != nil {
		return uuid.Nil, apperr.Invalid("invoice_id", "must be a UUID")
	}
	return id, nil
}

func actorOf(r *http.Request) Actor {
	actor := httpserver.ActorFrom(r.Context())
	return Actor{OrganizationID: actor.OrganizationID, Operator: actor.Operator}
}
