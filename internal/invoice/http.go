package invoice

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/httpserver"
	"github.com/skimer2king/factorflow/internal/platform/money"
)

// Handler exposes the invoice module over HTTP.
//
// It is deliberately thin: it decodes a request into domain types, calls the service, and
// renders the result. Every rule, from the state machine to who may see an invoice, lives
// behind the service, so an endpoint cannot accidentally become a second place where
// business rules are decided.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Routes registers the module's endpoints on a router that already carries authentication
// and idempotency middleware.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/invoices", h.create)
	r.Get("/invoices", h.list)
	r.Get("/invoices/{invoiceID}", h.get)
	r.Get("/invoices/{invoiceID}/document", h.getDocument)
	r.Post("/invoices/{invoiceID}/document", h.attachDocument)
	r.Post("/invoices/{invoiceID}/assess", h.assess)
	r.Post("/invoices/{invoiceID}/approve", h.approve)
	r.Post("/invoices/{invoiceID}/reject", h.reject)
}

// createRequest is the body of POST /invoices.
//
// Money crosses the boundary as a decimal string with its currency beside it, never as a
// float: a JSON number would already have lost precision before validation could object.
type createRequest struct {
	DebtorRef string `json:"debtor_ref"`
	Number    string `json:"number"`
	Face      string `json:"face"`
	Currency  string `json:"currency"`
	IssuedAt  string `json:"issued_at"`
	DueAt     string `json:"due_at"`
}

// documentRequest is the body of POST /invoices/{id}/document. It carries metadata only:
// the ciphertext went straight to object storage, and the plaintext never left the browser.
type documentRequest struct {
	ObjectKey  string `json:"object_key"`
	CipherHash string `json:"cipher_hash"`
	KeyRef     string `json:"key_ref"`
	MIME       string `json:"mime"`
	SizeBytes  int64  `json:"size_bytes"`
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

// invoiceResponse is the wire shape of an invoice.
type invoiceResponse struct {
	ID           string `json:"id"`
	IssuerID     string `json:"issuer_id"`
	DebtorRef    string `json:"debtor_ref"`
	Number       string `json:"number"`
	Face         string `json:"face"`
	Currency     string `json:"currency"`
	IssuedAt     string `json:"issued_at"`
	DueAt        string `json:"due_at"`
	TenorDays    int64  `json:"tenor_days"`
	Status       string `json:"status"`
	FailedFrom   string `json:"failed_from,omitempty"`
	Reason       string `json:"reason,omitempty"`
	AssessmentID string `json:"assessment_id,omitempty"`
	AssetID      string `json:"asset_id,omitempty"`
	Version      int64  `json:"version"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type documentResponse struct {
	InvoiceID  string `json:"invoice_id"`
	ObjectKey  string `json:"object_key"`
	CipherHash string `json:"cipher_hash"`
	MIME       string `json:"mime"`
	SizeBytes  int64  `json:"size_bytes"`
	UploadedAt string `json:"uploaded_at"`
}

type listResponse struct {
	Items []invoiceResponse `json:"items"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var body createRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	params, err := body.toParams()
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	inv, err := h.service.Create(r.Context(), actorOf(r), params)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusCreated, toResponse(inv))
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

	invoices, err := h.service.List(r.Context(), actorOf(r), limit)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	items := make([]invoiceResponse, 0, len(invoices))
	for _, inv := range invoices {
		items = append(items, toResponse(inv))
	}
	httpserver.WriteJSON(w, r, http.StatusOK, listResponse{Items: items})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	inv, err := h.service.Get(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toResponse(inv))
}

func (h *Handler) getDocument(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	doc, err := h.service.GetDocument(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	// The key reference is withheld: it is the handle the confidential workflow uses to
	// unwrap the data key, and nothing outside that workflow has any use for it.
	httpserver.WriteJSON(w, r, http.StatusOK, documentResponse{
		InvoiceID:  doc.InvoiceID.String(),
		ObjectKey:  doc.ObjectKey,
		CipherHash: doc.CipherHash,
		MIME:       doc.MIME,
		SizeBytes:  doc.SizeBytes,
		UploadedAt: doc.UploadedAt.Format(time.RFC3339),
	})
}

func (h *Handler) attachDocument(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	var body documentRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	inv, err := h.service.AttachDocument(r.Context(), actorOf(r), id, NewDocumentParams{
		ObjectKey:  body.ObjectKey,
		CipherHash: body.CipherHash,
		KeyRef:     body.KeyRef,
		MIME:       body.MIME,
		SizeBytes:  body.SizeBytes,
	})
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toResponse(inv))
}

func (h *Handler) assess(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	inv, err := h.service.RequestAssessment(r.Context(), actorOf(r), id, httpserver.TraceIDFrom(r.Context()))
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusAccepted, toResponse(inv))
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	inv, err := h.service.Approve(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toResponse(inv))
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request) {
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

	inv, err := h.service.Reject(r.Context(), actorOf(r), id, body.Reason)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toResponse(inv))
}

// toParams converts a request body into domain types, reporting every malformed field at
// once rather than one per round trip.
func (b createRequest) toParams() (CreateParams, error) {
	var violations []error

	currency, err := money.ParseCurrency(b.Currency)
	if err != nil {
		violations = append(violations, apperr.Invalid("currency", "must be a supported ISO 4217 code"))
	}

	var face money.Amount
	if currency.IsValid() {
		face, err = money.Parse(b.Face, currency)
		if err != nil {
			violations = append(violations, apperr.Invalid("face",
				"must be a decimal amount with at most %d decimal places", currency.Exponent()))
		}
	}

	issuedAt, err := parseTime("issued_at", b.IssuedAt)
	if err != nil {
		violations = append(violations, err)
	}
	dueAt, err := parseTime("due_at", b.DueAt)
	if err != nil {
		violations = append(violations, err)
	}

	if err := joinViolations(violations); err != nil {
		return CreateParams{}, err
	}

	return CreateParams{
		DebtorRef: b.DebtorRef,
		Number:    b.Number,
		Face:      face,
		IssuedAt:  issuedAt,
		DueAt:     dueAt,
	}, nil
}

func toResponse(inv *Invoice) invoiceResponse {
	out := invoiceResponse{
		ID:         inv.ID.String(),
		IssuerID:   inv.IssuerID.String(),
		DebtorRef:  inv.DebtorRef,
		Number:     inv.Number,
		Face:       inv.Face.String(),
		Currency:   inv.Face.Currency().String(),
		IssuedAt:   inv.IssuedAt.Format(time.RFC3339),
		DueAt:      inv.DueAt.Format(time.RFC3339),
		TenorDays:  inv.TenorDays(),
		Status:     inv.Status.String(),
		FailedFrom: inv.FailedFrom.String(),
		Reason:     inv.Reason,
		Version:    inv.Version,
		CreatedAt:  inv.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  inv.UpdatedAt.Format(time.RFC3339),
	}
	if inv.AssessmentID != uuid.Nil {
		out.AssessmentID = inv.AssessmentID.String()
	}
	if inv.AssetID != uuid.Nil {
		out.AssetID = inv.AssetID.String()
	}
	return out
}

func invoiceIDOf(r *http.Request) (uuid.UUID, error) {
	raw := chi.URLParam(r, "invoiceID")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, apperr.Invalid("invoice_id", "must be a UUID")
	}
	return id, nil
}

// actorOf maps the transport's authenticated caller onto the module's own actor, so the
// domain module never imports the transport's notion of a session.
func actorOf(r *http.Request) Actor {
	actor := httpserver.ActorFrom(r.Context())
	return Actor{OrganizationID: actor.OrganizationID, Operator: actor.Operator}
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

// joinViolations collects field errors into one validation error, or nil when there are
// none.
func joinViolations(violations []error) error {
	return errors.Join(violations...)
}
