package payments_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/payments"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

var (
	testNow   = time.Date(2026, time.September, 8, 11, 0, 0, 0, time.UTC)
	testPrice = money.MustParse("0.25", money.USD)
	testPayer = "0x9999999999999999999999999999999999999999"
)

const (
	testEndpoint  = "/paid/v1/risk-quote"
	otherEndpoint = "/paid/v1/auction-recommendation"
)

type fakeQuerier struct{}

func (fakeQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row        { panic("not used") }

// memRepo is an in-memory store with the same uniqueness rules the schema enforces.
type memRepo struct {
	mu       sync.Mutex
	requests map[uuid.UUID]payments.Request
}

func newMemRepo() *memRepo { return &memRepo{requests: map[uuid.UUID]payments.Request{}} }

func (m *memRepo) InTx(_ context.Context, fn func(postgres.Querier) error) error {
	return fn(fakeQuerier{})
}

func (m *memRepo) Querier() postgres.Querier { return fakeQuerier{} }

func (m *memRepo) Create(_ context.Context, _ postgres.Querier, r *payments.Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, stored := range m.requests {
		if stored.Nonce == r.Nonce {
			return apperr.Conflictf("nonce %s already exists", r.Nonce)
		}
		if r.IdempotencyKey != "" && stored.Endpoint == r.Endpoint && stored.IdempotencyKey == r.IdempotencyKey {
			return apperr.Conflictf("idempotency key %s already exists", r.IdempotencyKey)
		}
	}

	m.requests[r.ID] = *r
	return nil
}

func (m *memRepo) Get(_ context.Context, _ postgres.Querier, id uuid.UUID) (*payments.Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.requests[id]
	if !ok {
		return nil, apperr.NotFoundf("paid request %s", id)
	}
	copied := stored
	return &copied, nil
}

func (m *memRepo) GetByNonce(_ context.Context, _ postgres.Querier, nonce string) (*payments.Request, error) {
	return m.find(func(r payments.Request) bool { return r.Nonce == nonce },
		apperr.NotFoundf("paid request for nonce %s", nonce))
}

func (m *memRepo) GetByIdempotencyKey(_ context.Context, _ postgres.Querier, endpoint, key string) (*payments.Request, error) {
	return m.find(func(r payments.Request) bool { return r.Endpoint == endpoint && r.IdempotencyKey == key },
		apperr.NotFoundf("paid request for key %s", key))
}

func (m *memRepo) find(keep func(payments.Request) bool, missing error) (*payments.Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, stored := range m.requests {
		if keep(stored) {
			copied := stored
			return &copied, nil
		}
	}
	return nil, missing
}

func (m *memRepo) Update(_ context.Context, _ postgres.Querier, r *payments.Request, expectedVersion int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.requests[r.ID]
	if !ok {
		return apperr.NotFoundf("paid request %s", r.ID)
	}
	if stored.Version != expectedVersion {
		return apperr.Conflictf("paid request %s was modified concurrently", r.ID)
	}

	m.requests[r.ID] = *r
	return nil
}

func (m *memRepo) DeleteExpired(_ context.Context, _ postgres.Querier, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var removed int64
	for id, stored := range m.requests {
		if stored.State == payments.StatePaymentRequired && stored.ExpiresAt.Before(before) {
			delete(m.requests, id)
			removed++
		}
	}
	return removed, nil
}

// fixture is the paid endpoint, its facilitator, and a client wallet.
type fixture struct {
	service     *payments.Service
	repo        *memRepo
	facilitator *payments.LocalFacilitator
	router      http.Handler
	clock       time.Time

	// calls counts how often the work actually ran, which is how a test proves a replay
	// returned the stored answer instead of recomputing it.
	calls int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{repo: newMemRepo(), clock: testNow}
	f.facilitator = payments.NewLocalFacilitator("0x1111111111111111111111111111111111111111",
		func() time.Time { return f.clock })

	f.service = payments.NewService(payments.Config{
		DB:          f.repo,
		Repo:        f.repo,
		Facilitator: f.facilitator,
		Network:     payments.LocalNetwork,
		Recipient:   f.facilitator.Recipient(),
		Asset:       "USDC",
		Now:         func() time.Time { return f.clock },
		IDs:         uuid.New,
	})

	handler := payments.NewHandler(f.service)
	f.router = httpserver.NewRouter(httpserver.Dependencies{
		RootRoutes: func(r chi.Router) {
			r.Post(testEndpoint, handler.Paid(testEndpoint, testPrice, f.work))
			r.Post(otherEndpoint, handler.Paid(otherEndpoint, testPrice, f.work))
		},
	})
	return f
}

// work echoes the question back with an answer, and counts how often it ran.
func (f *fixture) work(_ context.Context, body []byte) ([]byte, error) {
	f.calls++

	var question map[string]any
	if err := json.Unmarshal(body, &question); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"asked": question, "grade": "B"})
}

func (f *fixture) post(t *testing.T, path, body, paymentHeader, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if paymentHeader != "" {
		req.Header.Set(payments.PaymentHeader, paymentHeader)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// requirementOf reads the 402 body.
func requirementOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body["accepts"].([]any)[0].(map[string]any)
}

// pay walks the client's half of the exchange: read the quote, pay it, build the header.
func (f *fixture) pay(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	accepted := requirementOf(t, rec)
	amount, err := money.Parse(accepted["amount"].(string), money.USD)
	require.NoError(t, err)

	payment := f.facilitator.Pay(testPayer, payments.Requirement{
		Nonce: accepted["nonce"].(string),
		Price: amount,
	})
	return payments.EncodePayment(payment, payments.LocalNetwork)
}

// TestAPaidRequestIsRefusedThenAnswered is the whole exchange, the way an agent performs
// it: ask, get a price, pay, ask again.
func TestAPaidRequestIsRefusedThenAnswered(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	body := `{"face":"10000.00","days_to_due":60}`

	rec := f.post(t, testEndpoint, body, "", "")
	require.Equal(t, http.StatusPaymentRequired, rec.Code, rec.Body.String())
	assert.Zero(t, f.calls, "an unpaid request costs the platform nothing")

	accepted := requirementOf(t, rec)
	assert.Equal(t, "0.25", accepted["amount"], "the price is a decimal string, not a rounded number")
	assert.Equal(t, "USD", accepted["currency"])
	assert.Equal(t, payments.LocalNetwork, accepted["network"])
	assert.NotEmpty(t, accepted["nonce"])
	assert.NotEmpty(t, accepted["pay_to"])

	rec = f.post(t, testEndpoint, body, f.pay(t, rec), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, f.calls)

	var answer map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &answer))
	assert.Equal(t, "B", answer["grade"])

	receipt := receiptOf(t, rec)
	assert.Equal(t, payments.StateSettled.String(), receipt["state"])
	assert.NotEmpty(t, receipt["tx_id"])
	assert.NotEmpty(t, receipt["response_hash"])
	assert.False(t, receipt["replayed"].(bool))
}

func receiptOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	header := rec.Header().Get(payments.PaymentResponseHeader)
	require.NotEmpty(t, header, "a client must be able to reconcile what it paid for")

	decoded, err := base64Decode(header)
	require.NoError(t, err)

	var receipt map[string]any
	require.NoError(t, json.Unmarshal(decoded, &receipt))
	return receipt
}

// TestAPaymentBuysTheAnswerItWasQuotedFor is the binding that stops a client paying for a
// cheap question and sending an expensive one.
func TestAPaymentBuysTheAnswerItWasQuotedFor(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	rec := f.post(t, testEndpoint, `{"face":"1.00"}`, "", "")
	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	header := f.pay(t, rec)

	// The same payment, presented with a different question.
	rec = f.post(t, testEndpoint, `{"face":"10000000.00"}`, header, "")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, f.calls, "the work never ran")

	// And presented to a different endpoint.
	rec = f.post(t, otherEndpoint, `{"face":"1.00"}`, header, "")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, f.calls)
}

// TestAQuoteIsPaidOnce stops one payment from buying two answers.
func TestAQuoteIsPaidOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	body := `{"face":"10000.00"}`

	rec := f.post(t, testEndpoint, body, "", "")
	header := f.pay(t, rec)

	require.Equal(t, http.StatusOK, f.post(t, testEndpoint, body, header, "").Code)
	require.Equal(t, 1, f.calls)

	// The same payment again for the same question returns the stored answer rather than
	// charging for a second run.
	rec = f.post(t, testEndpoint, body, header, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, f.calls, "the work ran once")
	assert.True(t, receiptOf(t, rec)["replayed"].(bool))
}

// TestAnExpiredQuoteIsRefused keeps a client from paying yesterday's price.
func TestAnExpiredQuoteIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	body := `{"face":"10000.00"}`

	rec := f.post(t, testEndpoint, body, "", "")
	header := f.pay(t, rec)

	f.clock = testNow.Add(payments.ChallengeTTL + time.Second)
	rec = f.post(t, testEndpoint, body, header, "")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, f.calls)
}

// TestAPaymentTheLedgerDoesNotKnowIsRefused is the check that keeps the endpoint from
// being free to anyone who can write JSON.
func TestAPaymentTheLedgerDoesNotKnowIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	body := `{"face":"10000.00"}`

	rec := f.post(t, testEndpoint, body, "", "")
	nonce := requirementOf(t, rec)["nonce"].(string)

	forged := payments.EncodePayment(payments.Payment{
		Nonce: nonce, Payer: testPayer, TxID: "pay-i-made-this-up", Amount: testPrice,
	}, payments.LocalNetwork)

	rec = f.post(t, testEndpoint, body, forged, "")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, f.calls)
}

// TestAnUnderpaymentIsRefused checks the amount, not just the existence of a payment.
func TestAnUnderpaymentIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	body := `{"face":"10000.00"}`

	rec := f.post(t, testEndpoint, body, "", "")
	nonce := requirementOf(t, rec)["nonce"].(string)

	// A real payment, for less than the quote.
	short := f.facilitator.Pay(testPayer, payments.Requirement{
		Nonce: nonce, Price: money.MustParse("0.01", money.USD),
	})

	rec = f.post(t, testEndpoint, body, payments.EncodePayment(short, payments.LocalNetwork), "")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, f.calls)
}

// TestARetryUnderTheSameKeyReturnsTheStoredAnswer is what the specification asks for: a
// client that lost the reply asks again and is not charged twice.
func TestARetryUnderTheSameKeyReturnsTheStoredAnswer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	body := `{"face":"10000.00"}`

	rec := f.post(t, testEndpoint, body, "", "agent-key-1")
	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	first := requirementOf(t, rec)["nonce"].(string)

	// Asking again under the same key hands back the same quote rather than a second one,
	// so the client cannot end up holding two payable prices for one question.
	rec = f.post(t, testEndpoint, body, "", "agent-key-1")
	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	assert.Equal(t, first, requirementOf(t, rec)["nonce"])

	header := f.pay(t, rec)
	require.Equal(t, http.StatusOK, f.post(t, testEndpoint, body, header, "agent-key-1").Code)
	require.Equal(t, 1, f.calls)

	// The reply was lost; the agent asks again with no payment header at all.
	rec = f.post(t, testEndpoint, body, "", "agent-key-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, f.calls, "the answer came from storage")
	assert.True(t, receiptOf(t, rec)["replayed"].(bool))
}

// TestTheSameKeyForADifferentQuestionIsRefused keeps an idempotency key from returning
// someone else's answer.
func TestTheSameKeyForADifferentQuestionIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	rec := f.post(t, testEndpoint, `{"face":"10000.00"}`, "", "agent-key-1")
	require.Equal(t, http.StatusPaymentRequired, rec.Code)

	rec = f.post(t, testEndpoint, `{"face":"20000.00"}`, "", "agent-key-1")
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// TestReformattingTheSameQuestionIsTheSameQuestion keeps a re-serialized body from looking
// like a new one, which would make idempotent retries impossible for most clients.
func TestReformattingTheSameQuestionIsTheSameQuestion(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	rec := f.post(t, testEndpoint, `{"face":"10000.00","days_to_due":60}`, "", "")
	header := f.pay(t, rec)

	reformatted := "{\n  \"days_to_due\": 60,\n  \"face\": \"10000.00\"\n}"
	rec = f.post(t, testEndpoint, reformatted, header, "")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestRejectedPaymentsStayRejected(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	body := `{"face":"10000.00"}`

	rec := f.post(t, testEndpoint, body, "", "agent-key-1")
	header := f.pay(t, rec)

	f.facilitator.FailVerification(errors.New("the facilitator is not answering"))
	rec = f.post(t, testEndpoint, body, header, "")
	require.NotEqual(t, http.StatusOK, rec.Code)

	// The refusal is remembered: asking again under the same key reports it rather than
	// quoting a fresh price for a payment that was already refused.
	f.facilitator.FailVerification(nil)
	rec = f.post(t, testEndpoint, body, "", "agent-key-1")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, f.calls)
}

func TestPaidRequestValidation(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	tests := []struct {
		name       string
		body       string
		header     string
		wantStatus int
	}{
		{name: "empty body", body: "", wantStatus: http.StatusUnprocessableEntity},
		{name: "not json", body: "face=10000", wantStatus: http.StatusUnprocessableEntity},
		{name: "unreadable payment header", body: `{"face":"1.00"}`, header: "!!!not-base64!!!", wantStatus: http.StatusUnprocessableEntity},
		{name: "payment without a nonce", body: `{"face":"1.00"}`, header: payments.EncodePayment(payments.Payment{TxID: "pay-1"}, payments.LocalNetwork), wantStatus: http.StatusUnprocessableEntity},
		{name: "unknown nonce", body: `{"face":"1.00"}`, header: payments.EncodePayment(payments.Payment{Nonce: "nope", TxID: "pay-1"}, payments.LocalNetwork), wantStatus: http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.post(t, testEndpoint, tc.body, tc.header, "")
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
		})
	}
}

func TestExpiredQuotesArePurgedButPaidOnesAreKept(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	// One quote nobody pays.
	f.post(t, testEndpoint, `{"face":"1.00"}`, "", "")

	// One that is paid.
	body := `{"face":"10000.00"}`
	rec := f.post(t, testEndpoint, body, "", "")
	require.Equal(t, http.StatusOK, f.post(t, testEndpoint, body, f.pay(t, rec), "").Code)

	f.clock = testNow.Add(payments.ChallengeTTL + time.Minute)
	removed, err := f.service.PurgeExpiredQuotes(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed, "the unpaid quote is gone and the paid exchange is kept")
}

// base64Decode reads the receipt header the handler wrote.
func base64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
