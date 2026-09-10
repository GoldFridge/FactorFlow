// Package payments implements the x402 exchange: a machine client asks for something, is
// told the price, pays, and asks again with the proof.
//
// The awkward part of charging per request is that the payment and the work are two
// different things that must end up agreeing. This package binds them: the price quoted
// names the exact request it was quoted for, the proof names the same request, and the
// answer is stored against both. A payment for one question cannot buy the answer to
// another, and the same payment cannot buy two answers.
package payments

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// ChallengeTTL is how long a quoted price stays payable.
//
// It is short because it is a price: quoting for an hour would let a client pay at
// yesterday's rate. It is not so short that a client cannot sign and submit in time.
const ChallengeTTL = 5 * time.Minute

// MaxErrorLen bounds a stored rejection reason.
const MaxErrorLen = 512

// State is where one paid request has got to.
type State string

// The states of a paid request.
const (
	// StatePaymentRequired is a quoted price nobody has paid yet.
	StatePaymentRequired State = "PAYMENT_REQUIRED"
	// StateSigned means a client presented a payment for the quote.
	StateSigned State = "SIGNED"
	// StateVerified means the facilitator agreed the payment is real, is for this request,
	// and has not been used before.
	StateVerified State = "VERIFIED"
	// StateProcessed means the work was done and the answer recorded.
	StateProcessed State = "PROCESSED"
	// StateSettled means the payment is final.
	StateSettled State = "SETTLED"
	// StateRejected means the payment was refused. The client keeps its money and gets no
	// answer.
	StateRejected State = "REJECTED"
)

// transitions is the whole exchange.
var transitions = map[State][]State{
	StatePaymentRequired: {StateSigned, StateRejected},
	StateSigned:          {StateVerified, StateRejected},
	StateVerified:        {StateProcessed, StateRejected},
	StateProcessed:       {StateSettled, StateRejected},
	StateSettled:         nil,
	StateRejected:        nil,
}

// ParseState validates a state read from storage.
func ParseState(s string) (State, error) {
	state := State(s)
	if !state.IsValid() {
		return "", apperr.Invalid("state", "unknown payment state %q", s)
	}
	return state, nil
}

// IsValid reports whether the state is part of the exchange.
func (s State) IsValid() bool {
	_, ok := transitions[s]
	return ok
}

// IsPaid reports whether the work may be done or its answer returned.
func (s State) IsPaid() bool {
	return s == StateVerified || s == StateProcessed || s == StateSettled
}

// IsFinished reports whether the exchange is over, either way.
func (s State) IsFinished() bool { return s == StateSettled || s == StateRejected }

// String returns the wire form.
func (s State) String() string { return string(s) }

// Requirement is what a 402 response tells a client: what to pay, to whom, and for which
// request.
//
// The nonce and the request hash are both in it on purpose. The nonce makes each quote
// single-use; the request hash binds the quote to one body, so a client cannot pay for a
// cheap question and send an expensive one.
type Requirement struct {
	// Scheme names the payment scheme, so a client knows how to construct the proof.
	Scheme  string
	Network string
	// Recipient is the address that must receive the payment.
	Recipient string
	// Asset names what the price is denominated in on chain.
	Asset string

	Price money.Amount

	Nonce       string
	RequestHash string

	IssuedAt  time.Time
	ExpiresAt time.Time
}

/*
PaidBy reports whether this request is already being fulfilled against exactly this payment.

It is what separates a customer asking again for an answer it paid for from somebody else
turning up with a different payment for a quote in flight. The first is owed the work; the
second is refused.
*/
func (r *Request) PaidBy(payment Payment) bool {
	return r.PaymentTx != "" &&
		r.PaymentTx == strings.TrimSpace(payment.TxID) &&
		r.Payer == strings.ToLower(strings.TrimSpace(payment.Payer))
}

// IsExpired reports whether the quoted price has gone stale.
func (r Requirement) IsExpired(now time.Time) bool { return now.After(r.ExpiresAt) }

// Payment is the proof a client presents.
type Payment struct {
	// Nonce identifies the quote being paid.
	Nonce string
	// Payer is the address the payment came from, which is also who the answer belongs to.
	Payer string
	// TxID is the on-chain payment.
	TxID   string
	Amount money.Amount
	// Signature is the payer's authorization of this exact payment, where the scheme uses
	// one. The facilitator decides whether it is required.
	Signature string
}

// Request is one paid request, from the quote to the answer.
type Request struct {
	ID uuid.UUID

	// Endpoint is the paid route, so a quote for one endpoint cannot buy another's answer.
	Endpoint string
	// RequestHash binds the exchange to the exact body that was asked about.
	RequestHash string
	// Nonce is the single-use identifier of the quote.
	Nonce string

	// IdempotencyKey is the client's own identifier for the request, when it supplied one.
	// A repeat under the same key returns the stored answer; the same key with a different
	// body is refused, because that is a different question wearing the same name.
	IdempotencyKey string

	Price money.Amount

	Payer     string
	PaymentTx string
	// ResponseHash is the digest of the answer that was returned, so a client and the
	// platform can later agree on what was paid for.
	ResponseHash string
	// Response is the stored answer, returned verbatim on a repeat.
	Response []byte

	State State
	// Reason explains a rejection, for the client that was refused.
	Reason string

	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
	Version   int64
}

// NewParams are the facts of a quoted price.
type NewParams struct {
	ID             uuid.UUID
	Endpoint       string
	RequestHash    string
	Nonce          string
	IdempotencyKey string
	Price          money.Amount
}

// New quotes a price for one request.
func New(p NewParams, now time.Time) (*Request, error) {
	var violations []error

	if p.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("id", "must be a non-nil UUID"))
	}
	if strings.TrimSpace(p.Endpoint) == "" {
		violations = append(violations, apperr.Invalid("endpoint", "must not be empty"))
	}
	if !hashPattern(p.RequestHash) {
		violations = append(violations, apperr.Invalid("request_hash", "must be a 0x-prefixed SHA-256 digest"))
	}
	if strings.TrimSpace(p.Nonce) == "" {
		violations = append(violations, apperr.Invalid("nonce", "must not be empty"))
	}
	if !p.Price.IsValid() || !p.Price.IsPositive() {
		violations = append(violations, apperr.Invalid("price", "must be a positive amount"))
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	return &Request{
		ID:             p.ID,
		Endpoint:       strings.TrimSpace(p.Endpoint),
		RequestHash:    strings.ToLower(p.RequestHash),
		Nonce:          strings.TrimSpace(p.Nonce),
		IdempotencyKey: strings.TrimSpace(p.IdempotencyKey),
		Price:          p.Price,
		State:          StatePaymentRequired,
		CreatedAt:      now.UTC(),
		UpdatedAt:      now.UTC(),
		ExpiresAt:      now.Add(ChallengeTTL).UTC(),
		Version:        1,
	}, nil
}

// Requirement returns what the client must pay.
func (r *Request) Requirement(scheme, network, recipient, asset string) Requirement {
	return Requirement{
		Scheme:      scheme,
		Network:     network,
		Recipient:   recipient,
		Asset:       asset,
		Price:       r.Price,
		Nonce:       r.Nonce,
		RequestHash: r.RequestHash,
		IssuedAt:    r.CreatedAt,
		ExpiresAt:   r.ExpiresAt,
	}
}

// IsExpired reports whether the quote has gone stale.
func (r *Request) IsExpired(now time.Time) bool { return now.After(r.ExpiresAt) }

// Sign records that a client presented a payment.
func (r *Request) Sign(payer, txID string, now time.Time) error {
	switch {
	case strings.TrimSpace(payer) == "":
		return apperr.Invalid("payer", "must not be empty")
	case strings.TrimSpace(txID) == "":
		return apperr.Invalid("tx_id", "must not be empty")
	}
	if err := r.transition(StateSigned, now); err != nil {
		return err
	}

	r.Payer = strings.ToLower(strings.TrimSpace(payer))
	r.PaymentTx = strings.TrimSpace(txID)
	return nil
}

// Verify records that the facilitator accepted the payment.
func (r *Request) Verify(now time.Time) error { return r.transition(StateVerified, now) }

// Process records the answer the client paid for.
//
// The response is stored, not just its hash: a client that paid and then lost the reply
// must be able to ask again and receive what it bought, rather than being told to pay
// twice for the same answer.
func (r *Request) Process(response []byte, now time.Time) error {
	if len(response) == 0 {
		return apperr.Invalid("response", "must not be empty")
	}
	if err := r.transition(StateProcessed, now); err != nil {
		return err
	}

	r.Response = response
	r.ResponseHash = Hash(response)
	return nil
}

// Settle records that the payment is final.
func (r *Request) Settle(now time.Time) error { return r.transition(StateSettled, now) }

// Reject refuses the exchange and says why.
func (r *Request) Reject(reason string, now time.Time) error {
	cleaned := strings.TrimSpace(reason)
	switch {
	case cleaned == "":
		return apperr.Invalid("reason", "must not be empty")
	case len(cleaned) > MaxErrorLen:
		cleaned = cleaned[:MaxErrorLen]
	}
	if err := r.transition(StateRejected, now); err != nil {
		return err
	}

	r.Reason = cleaned
	return nil
}

// IsPaid reports whether the answer may be produced or returned.
func (r *Request) IsPaid() bool { return r.State.IsPaid() }

func (r *Request) transition(next State, now time.Time) error {
	for _, allowed := range transitions[r.State] {
		if allowed == next {
			r.State = next
			r.Version++
			r.UpdatedAt = now.UTC()
			return nil
		}
	}
	return apperr.Conflictf("paid request %s cannot move from %s to %s", r.ID, r.State, next)
}

// Hash digests a request body or a response.
//
// It is what binds a payment to one question and one answer, so it must be computed the
// same way on both sides: the bytes exactly as they were sent, with no reformatting.
func Hash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "0x" + hex.EncodeToString(sum[:])
}

// CanonicalHash digests a JSON body independently of key order and whitespace.
//
// A client that re-serializes the same request must get the same hash, or an idempotent
// retry would look like a different question. Anything that is not JSON is hashed as it
// arrived, because then the bytes are all there is.
func CanonicalHash(payload []byte) string {
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Hash(payload)
	}

	canonical, err := json.Marshal(canonicalize(decoded))
	if err != nil {
		return Hash(payload)
	}
	return Hash(canonical)
}

// canonicalize rewrites decoded JSON so that marshalling it is deterministic. Go already
// sorts map keys when marshalling, so this only has to rebuild the containers.
func canonicalize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[k] = canonicalize(typed[k])
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = canonicalize(item)
		}
		return out
	default:
		return value
	}
}

func hashPattern(s string) bool {
	if len(s) != 66 || !strings.HasPrefix(s, "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}
