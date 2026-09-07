package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// TxRunner is the transaction boundary the service needs.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// Work produces the answer a client is paying for.
//
// It is a function rather than an interface so this module never learns what is being
// sold. Risk quotes and auction recommendations are supplied by the application layer;
// here they are only bytes that cost money.
type Work func(ctx context.Context, body []byte) ([]byte, error)

// Service runs the x402 exchange.
type Service struct {
	db          TxRunner
	repo        Repository
	facilitator Facilitator

	scheme    string
	network   string
	recipient string
	asset     string

	now func() time.Time
	ids func() uuid.UUID
}

// Config wires the service.
type Config struct {
	DB          TxRunner
	Repo        Repository
	Facilitator Facilitator

	// Network, Recipient and Asset are what a 402 tells the client to pay, and where.
	Network   string
	Recipient string
	Asset     string
	Scheme    string

	Now func() time.Time
	IDs func() uuid.UUID
}

// NewService returns the service.
func NewService(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IDs == nil {
		cfg.IDs = uuid.New
	}
	if cfg.Scheme == "" {
		cfg.Scheme = SchemeExact
	}
	if cfg.Network == "" {
		cfg.Network = LocalNetwork
	}
	return &Service{
		db:          cfg.DB,
		repo:        cfg.Repo,
		facilitator: cfg.Facilitator,
		scheme:      cfg.Scheme,
		network:     cfg.Network,
		recipient:   strings.ToLower(strings.TrimSpace(cfg.Recipient)),
		asset:       cfg.Asset,
		now:         cfg.Now,
		ids:         cfg.IDs,
	}
}

// Result is the outcome of one call to a paid endpoint: either a price to pay, or the
// answer that was bought.
type Result struct {
	// Requirement is set when the client must pay before being answered.
	Requirement *Requirement
	// Response is the answer, set once a payment has been accepted.
	Response []byte
	// Request is the stored exchange, for the caller that wants to report its state.
	Request *Request
	// Replayed reports that the answer came from storage rather than being computed again.
	Replayed bool
}

// Quote prices a request and records the challenge.
//
// A client that already has an answer under the same idempotency key gets that answer
// instead: paying twice for a reply that was lost in transit is not a reasonable thing to
// ask of anyone.
func (s *Service) Quote(ctx context.Context, endpoint string, body []byte, price money.Amount, idempotencyKey string) (Result, error) {
	requestHash := CanonicalHash(body)

	if idempotencyKey != "" {
		existing, err := s.repo.GetByIdempotencyKey(ctx, s.db.Querier(), endpoint, idempotencyKey)
		switch {
		case err == nil:
			return s.replay(existing, requestHash)
		case !apperr.IsNotFound(err):
			return Result{}, err
		}
	}

	nonce, err := newNonce()
	if err != nil {
		return Result{}, err
	}

	request, err := New(NewParams{
		ID:             s.ids(),
		Endpoint:       endpoint,
		RequestHash:    requestHash,
		Nonce:          nonce,
		IdempotencyKey: idempotencyKey,
		Price:          price,
	}, s.now())
	if err != nil {
		return Result{}, err
	}

	if err := s.db.InTx(ctx, func(q postgres.Querier) error {
		return s.repo.Create(ctx, q, request)
	}); err != nil {
		return Result{}, err
	}

	requirement := request.Requirement(s.scheme, s.network, s.recipient, s.asset)
	return Result{Requirement: &requirement, Request: request}, nil
}

// replay returns a stored answer, or refuses a key that is being reused for a different
// question.
func (s *Service) replay(existing *Request, requestHash string) (Result, error) {
	if existing.RequestHash != requestHash {
		// The same name for a different question. Returning the old answer would be wrong
		// and computing a new one would break the promise the key makes, so it is refused.
		return Result{}, apperr.Conflictf(
			"idempotency key %s was already used for a different request body", existing.IdempotencyKey)
	}

	switch {
	case existing.State == StateProcessed || existing.State == StateSettled:
		return Result{Response: existing.Response, Request: existing, Replayed: true}, nil
	case existing.State == StateRejected:
		return Result{}, apperr.Forbiddenf("the payment for this request was rejected: %s", existing.Reason)
	default:
		// Quoted but not yet paid: hand back the same challenge rather than a second one,
		// so a client that retries does not end up holding two payable quotes.
		requirement := existing.Requirement(s.scheme, s.network, s.recipient, s.asset)
		return Result{Requirement: &requirement, Request: existing}, nil
	}
}

// Redeem verifies a payment and produces the answer it bought.
//
// The order is deliberate. The payment is checked against the quote before any work
// happens, so an unpaid request costs nothing; the answer is stored in the same
// transaction that marks the request processed, so a client can never be charged for an
// answer the platform did not keep.
func (s *Service) Redeem(ctx context.Context, endpoint string, body []byte, payment Payment, work Work) (Result, error) {
	if strings.TrimSpace(payment.Nonce) == "" {
		return Result{}, apperr.Invalid("payment.nonce", "must name the quote being paid")
	}

	request, err := s.repo.GetByNonce(ctx, s.db.Querier(), payment.Nonce)
	if err != nil {
		return Result{}, err
	}

	// A payment already spent on this exact request buys the same answer again rather than
	// a second charge.
	if request.State == StateProcessed || request.State == StateSettled {
		return s.replay(request, CanonicalHash(body))
	}

	if err := s.check(ctx, request, endpoint, body, payment); err != nil {
		if rejectErr := s.reject(ctx, request, err); rejectErr != nil {
			return Result{}, rejectErr
		}
		return Result{}, err
	}

	// Signed and verified are stored before the work runs. If the work then fails, the
	// record still shows a paid request that owes an answer, which is the state
	// reconciliation can act on; the alternative is a payment nobody can account for.
	if err := s.advance(ctx, request, func(r *Request) error {
		if err := r.Sign(payment.Payer, payment.TxID, s.now()); err != nil {
			return err
		}
		return r.Verify(s.now())
	}); err != nil {
		return Result{}, err
	}

	response, err := work(ctx, body)
	if err != nil {
		return Result{}, fmt.Errorf("producing the paid response: %w", err)
	}

	if err := s.advance(ctx, request, func(r *Request) error {
		return r.Process(response, s.now())
	}); err != nil {
		return Result{}, err
	}

	if err := s.facilitator.Settle(ctx, payment); err != nil {
		// The answer exists and the client will receive it. Settlement is the platform's
		// problem from here, so it is recorded rather than turned into the client's error.
		return Result{Response: response, Request: request}, nil
	}
	if err := s.advance(ctx, request, func(r *Request) error {
		return r.Settle(s.now())
	}); err != nil {
		return Result{}, err
	}

	return Result{Response: response, Request: request}, nil
}

// check runs every reason a payment may be refused.
func (s *Service) check(ctx context.Context, request *Request, endpoint string, body []byte, payment Payment) error {
	switch {
	case request.Endpoint != endpoint:
		// A quote for a cheap endpoint must not buy an expensive one's answer.
		return apperr.Forbiddenf("this payment was quoted for %s", request.Endpoint)
	case request.RequestHash != CanonicalHash(body):
		return apperr.Forbiddenf("the payment was quoted for a different request body")
	case request.IsExpired(s.now()):
		return apperr.Forbiddenf("the quoted price expired at %s", request.ExpiresAt.Format(time.RFC3339))
	case request.State != StatePaymentRequired:
		return apperr.Conflictf("this quote is %s and cannot be paid again", request.State)
	}

	requirement := request.Requirement(s.scheme, s.network, s.recipient, s.asset)
	return s.facilitator.Verify(ctx, requirement, payment)
}

// reject records why a payment was refused, so a client asking again is told the same
// thing rather than being quoted afresh.
func (s *Service) reject(ctx context.Context, request *Request, cause error) error {
	if request.State.IsFinished() {
		return nil
	}
	return s.advance(ctx, request, func(r *Request) error {
		return r.Reject(cause.Error(), s.now())
	})
}

// advance applies a change and stores it under the version it was read at.
func (s *Service) advance(ctx context.Context, request *Request, apply func(*Request) error) error {
	return s.db.InTx(ctx, func(q postgres.Querier) error {
		expectedVersion := request.Version
		if err := apply(request); err != nil {
			return err
		}
		return s.repo.Update(ctx, q, request, expectedVersion)
	})
}

// PurgeExpiredQuotes removes quotes nobody paid.
func (s *Service) PurgeExpiredQuotes(ctx context.Context) (int64, error) {
	var removed int64
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		removed, err = s.repo.DeleteExpired(ctx, q, s.now())
		return err
	})
	return removed, err
}

// newNonce returns a single-use identifier for a quote.
func newNonce() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a payment nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
