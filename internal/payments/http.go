package payments

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/idempotency"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// PaymentHeader carries the client's proof of payment.
const PaymentHeader = "X-Payment"

// PaymentResponseHeader carries what the platform did with it, so a client can reconcile
// its own spending against the answers it received.
const PaymentResponseHeader = "X-Payment-Response"

// MaxPaidBodyBytes bounds a paid request body. A paid endpoint is a machine interface and
// its questions are small; anything larger is a mistake or an attempt to make the platform
// do unpaid work.
const MaxPaidBodyBytes = 64 << 10

// Handler turns a Work into a paid HTTP endpoint.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// requirementResponse is the body of a 402.
//
// It follows the x402 shape: a list of what the server accepts, so a client can pick a
// scheme it can satisfy. There is one entry here because this deployment quotes one price
// in one asset.
type requirementResponse struct {
	Error   string              `json:"error"`
	Accepts []acceptedPayment   `json:"accepts"`
	Request requestedResourceID `json:"resource"`
}

type acceptedPayment struct {
	Scheme string `json:"scheme"`
	// Network and Asset say what to pay in; PayTo says to whom.
	Network string `json:"network"`
	Asset   string `json:"asset"`
	PayTo   string `json:"pay_to"`
	// Amount is a decimal string with its currency beside it, never a JSON number: the
	// price is exact and must not be rounded on the way to a client's wallet.
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	// Nonce is what the client's payment must name, and it is good once.
	Nonce string `json:"nonce"`
	// ExpiresAt is when the quote stops being payable.
	ExpiresAt string `json:"expires_at"`
	// MaxTimeoutSeconds is how long the client has, stated the way x402 clients expect it.
	MaxTimeoutSeconds int `json:"max_timeout_seconds"`
}

type requestedResourceID struct {
	Endpoint string `json:"endpoint"`
	// RequestHash binds the quote to one body: paying for a cheap question and sending an
	// expensive one is refused because these two digests would differ.
	RequestHash string `json:"request_hash"`
}

// paymentPayload is the JSON a client puts in the X-Payment header, base64-encoded.
type paymentPayload struct {
	Scheme    string `json:"scheme"`
	Network   string `json:"network"`
	Nonce     string `json:"nonce"`
	Payer     string `json:"payer"`
	TxID      string `json:"tx_id"`
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
	Signature string `json:"signature,omitempty"`
}

// paymentReceipt is the JSON returned in X-Payment-Response.
type paymentReceipt struct {
	Nonce        string `json:"nonce"`
	TxID         string `json:"tx_id"`
	State        string `json:"state"`
	RequestHash  string `json:"request_hash"`
	ResponseHash string `json:"response_hash"`
	// Replayed reports that this answer was stored rather than computed again, so a client
	// knows it was not charged twice.
	Replayed bool `json:"replayed"`
}

// Paid wraps work in the x402 exchange.
//
// Without a payment header the request is priced and refused with 402. With one, the
// payment is checked against the quote and the work runs. The work itself never sees a
// payment: by the time it is called, the request has been paid for.
func (h *Handler) Paid(endpoint string, price money.Amount, work Work) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPaidBodyBytes))
		if err != nil {
			httpserver.WriteProblem(w, r, apperr.Invalid("body", "could not be read"))
			return
		}
		if len(body) == 0 {
			// An empty body would be priced and hashed like any other, and a client would
			// pay for a question it never asked.
			httpserver.WriteProblem(w, r, apperr.Invalid("body", "must not be empty"))
			return
		}
		if !json.Valid(body) {
			httpserver.WriteProblem(w, r, apperr.Invalid("body", "must be JSON"))
			return
		}

		header := strings.TrimSpace(r.Header.Get(PaymentHeader))
		if header == "" {
			h.quote(w, r, endpoint, body, price)
			return
		}

		payment, err := decodePayment(header)
		if err != nil {
			httpserver.WriteProblem(w, r, err)
			return
		}

		result, err := h.service.Redeem(r.Context(), endpoint, body, payment, work)
		if err != nil {
			httpserver.WriteProblem(w, r, err)
			return
		}

		writeReceipt(w, result)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Response)
	}
}

// quote prices the request and answers 402.
func (h *Handler) quote(w http.ResponseWriter, r *http.Request, endpoint string, body []byte, price money.Amount) {
	key := strings.TrimSpace(r.Header.Get(idempotency.HeaderKey))

	result, err := h.service.Quote(r.Context(), endpoint, body, price, key)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	// A client that already paid under this key gets its answer, not another price.
	if result.Response != nil {
		writeReceipt(w, result)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Response)
		return
	}

	requirement := *result.Requirement
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(requirementResponse{
		Error: "payment required",
		Accepts: []acceptedPayment{{
			Scheme:            requirement.Scheme,
			Network:           requirement.Network,
			Asset:             requirement.Asset,
			PayTo:             requirement.Recipient,
			Amount:            requirement.Price.String(),
			Currency:          requirement.Price.Currency().String(),
			Nonce:             requirement.Nonce,
			ExpiresAt:         requirement.ExpiresAt.Format(time.RFC3339),
			MaxTimeoutSeconds: int(ChallengeTTL.Seconds()),
		}},
		Request: requestedResourceID{
			Endpoint:    endpoint,
			RequestHash: requirement.RequestHash,
		},
	})
}

// writeReceipt records what the payment bought, before the body is written.
func writeReceipt(w http.ResponseWriter, result Result) {
	if result.Request == nil {
		return
	}

	receipt, err := json.Marshal(paymentReceipt{
		Nonce:        result.Request.Nonce,
		TxID:         result.Request.PaymentTx,
		State:        result.Request.State.String(),
		RequestHash:  result.Request.RequestHash,
		ResponseHash: result.Request.ResponseHash,
		Replayed:     result.Replayed,
	})
	if err != nil {
		return
	}
	w.Header().Set(PaymentResponseHeader, base64.StdEncoding.EncodeToString(receipt))
}

// decodePayment reads the X-Payment header.
func decodePayment(header string) (Payment, error) {
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		// Some clients send the JSON unencoded. Accepting both costs nothing and saves an
		// integrator an afternoon.
		raw = []byte(header)
	}

	var payload paymentPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Payment{}, apperr.Invalid(PaymentHeader, "must be base64-encoded JSON")
	}
	if strings.TrimSpace(payload.Nonce) == "" {
		return Payment{}, apperr.Invalid("payment.nonce", "must name the quote being paid")
	}

	payment := Payment{
		Nonce:     strings.TrimSpace(payload.Nonce),
		Payer:     strings.TrimSpace(payload.Payer),
		TxID:      strings.TrimSpace(payload.TxID),
		Signature: payload.Signature,
	}

	if payload.Amount != "" {
		currency, err := money.ParseCurrency(payload.Currency)
		if err != nil {
			return Payment{}, apperr.Invalid("payment.currency", "must be a supported currency")
		}
		if payment.Amount, err = money.Parse(payload.Amount, currency); err != nil {
			return Payment{}, apperr.Invalid("payment.amount", "must be a decimal amount")
		}
	}

	return payment, nil
}

// EncodePayment builds the X-Payment header a client sends. It lives here so a demo agent
// and the tests construct the header the same way the server reads it.
func EncodePayment(payment Payment, network string) string {
	payload := paymentPayload{
		Scheme:    SchemeExact,
		Network:   network,
		Nonce:     payment.Nonce,
		Payer:     payment.Payer,
		TxID:      payment.TxID,
		Signature: payment.Signature,
	}
	if payment.Amount.IsValid() {
		payload.Amount = payment.Amount.String()
		payload.Currency = payment.Amount.Currency().String()
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(encoded)
}
