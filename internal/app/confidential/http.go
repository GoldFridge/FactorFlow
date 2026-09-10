package confidential

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

/*
Handler exposes the two endpoints a confidential workflow uses.

They are not part of the participant API and are not reachable with a session: the caller is
a program running in an enclave, authenticated by a token released to it by the Vault DON.
A bearer token is a weak thing to hold alone, and it is not alone — the work it collects is
ciphertext, and the answer it returns is checked against the request it claims to answer.
*/
type Handler struct {
	service *Service
	// token is the shared secret the workflow presents. Empty disables both endpoints,
	// which is the right default: a deployment with no confidential workflow should not
	// have a route that hands out documents.
	token string
}

// NewHandler returns the handler.
func NewHandler(service *Service, token string) *Handler {
	return &Handler{service: service, token: strings.TrimSpace(token)}
}

// Enabled reports whether this deployment has a confidential workflow to serve.
func (h *Handler) Enabled() bool { return h != nil && h.token != "" }

// Routes registers the endpoints.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/confidential/work", h.next)
	r.Post("/confidential/results", h.deliver)
}

// workResponse is one waiting assessment as the workflow receives it.
type workResponse struct {
	InvoiceID  string `json:"invoice_id"`
	Nonce      string `json:"nonce"`
	CipherHash string `json:"cipher_hash"`
	MIME       string `json:"mime"`
	// Ciphertext is base64. There is no key beside it, here or anywhere else the platform
	// can reach.
	Ciphertext    string `json:"ciphertext"`
	SchemaVersion string `json:"schema_version"`
}

// resultRequest is the minimal output a confidential run returns.
type resultRequest struct {
	InvoiceID string `json:"invoice_id"`
	Features  struct {
		DSONorm             string `json:"dso_norm"`
		LatePaymentRate     string `json:"late_payment_rate"`
		DisputeFlag         string `json:"dispute_flag"`
		DebtorConcentration string `json:"debtor_concentration"`
		MarketVolatility    string `json:"market_volatility"`
		DebtorRisk          string `json:"debtor_risk"`
	} `json:"features"`

	Confidence      string `json:"confidence"`
	ArithmeticValid bool   `json:"arithmetic_valid"`

	Mitigations struct {
		Recourse       bool `json:"recourse"`
		Collateralized bool `json:"collateralized"`
	} `json:"mitigations"`

	Commitment    string `json:"commitment"`
	SchemaVersion string `json:"schema_version"`
	ModelVersion  string `json:"model_version"`
	// Evidence points at the run's simulation or deployment log, so a judge can check the
	// claim rather than take it.
	Evidence string `json:"evidence"`
}

func (h *Handler) next(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		httpserver.WriteProblem(w, r, apperr.Forbiddenf("this endpoint is for the confidential workflow"))
		return
	}

	work, err := h.service.Next(r.Context())
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	if work == nil {
		// Nothing waiting. An empty body says so more clearly than an empty list, and the
		// workflow's own code treats it as "nothing to assess" rather than as a failure.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	httpserver.WriteJSON(w, r, http.StatusOK, workResponse{
		InvoiceID:     work.InvoiceID.String(),
		Nonce:         work.Nonce,
		CipherHash:    work.CipherHash,
		MIME:          work.MIME,
		Ciphertext:    base64.StdEncoding.EncodeToString(work.Ciphertext),
		SchemaVersion: work.SchemaVersion,
	})
}

func (h *Handler) deliver(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		httpserver.WriteProblem(w, r, apperr.Forbiddenf("this endpoint is for the confidential workflow"))
		return
	}

	var body resultRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	result, err := body.toResult()
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	if err := h.service.Deliver(r.Context(), result); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	httpserver.WriteJSON(w, r, http.StatusAccepted, map[string]string{
		"invoice_id": result.InvoiceID.String(),
		"status":     "assessed",
	})
}

// toResult turns the wire shape into the domain's own types, refusing anything that is not
// a number where a number belongs.
func (b resultRequest) toResult() (risk.WorkflowResult, error) {
	invoiceID, err := uuid.Parse(strings.TrimSpace(b.InvoiceID))
	if err != nil {
		return risk.WorkflowResult{}, apperr.Invalid("invoice_id", "must be a UUID")
	}

	features := map[string]string{
		"dso_norm":             b.Features.DSONorm,
		"late_payment_rate":    b.Features.LatePaymentRate,
		"dispute_flag":         b.Features.DisputeFlag,
		"debtor_concentration": b.Features.DebtorConcentration,
		"market_volatility":    b.Features.MarketVolatility,
		"debtor_risk":          b.Features.DebtorRisk,
	}
	parsed := map[string]money.Rate{}
	for name, raw := range features {
		rate, err := money.ParseRate(strings.TrimSpace(raw))
		if err != nil {
			return risk.WorkflowResult{}, apperr.Invalid("features."+name, "must be a decimal rate")
		}
		parsed[name] = rate
	}

	confidence, err := money.ParseRate(strings.TrimSpace(b.Confidence))
	if err != nil {
		return risk.WorkflowResult{}, apperr.Invalid("confidence", "must be a decimal rate")
	}

	return risk.WorkflowResult{
		InvoiceID: invoiceID,
		Features: risk.FeatureVector{
			DSONorm:             parsed["dso_norm"],
			LatePaymentRate:     parsed["late_payment_rate"],
			DisputeFlag:         parsed["dispute_flag"],
			DebtorConcentration: parsed["debtor_concentration"],
			MarketVolatility:    parsed["market_volatility"],
			DebtorRisk:          parsed["debtor_risk"],
		},
		Confidence:      confidence,
		ArithmeticValid: b.ArithmeticValid,
		Mitigations: risk.Mitigations{
			Recourse:       b.Mitigations.Recourse,
			Collateralized: b.Mitigations.Collateralized,
		},
		Commitment:    strings.TrimSpace(b.Commitment),
		SchemaVersion: strings.TrimSpace(b.SchemaVersion),
		ModelVersion:  strings.TrimSpace(b.ModelVersion),
		Evidence:      strings.TrimSpace(b.Evidence),
	}, nil
}

/*
authorized checks the bearer token in constant time.

Constant time because the comparison is against a secret and the caller may retry as often
as it likes: a comparison that returns early tells an attacker how much of the token it has
guessed, one byte at a time.
*/
func (h *Handler) authorized(r *http.Request) bool {
	if h.token == "" {
		return false
	}

	presented := strings.TrimSpace(strings.TrimPrefix(
		strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "))

	return subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) == 1
}
