package payments_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/payments"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

func quoted(t *testing.T, seq int, key string) *payments.Request {
	t.Helper()

	r, err := payments.New(payments.NewParams{
		ID:             uuid.New(),
		Endpoint:       testEndpoint,
		RequestHash:    payments.Hash([]byte(fmt.Sprintf(`{"face":"%d.00"}`, seq))),
		Nonce:          fmt.Sprintf("nonce-%d", seq),
		IdempotencyKey: key,
		Price:          testPrice,
	}, testNow)
	require.NoError(t, err)
	return r
}

func TestPaidRequestRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := payments.NewPostgresRepository()

	request := quoted(t, 1, "agent-key-1")
	require.NoError(t, repo.Create(ctx, db.Querier(), request))

	got, err := repo.Get(ctx, db.Querier(), request.ID)
	require.NoError(t, err)
	assert.Equal(t, request.Endpoint, got.Endpoint)
	assert.Equal(t, request.RequestHash, got.RequestHash)
	assert.Equal(t, "0.25", got.Price.String())
	assert.Equal(t, payments.StatePaymentRequired, got.State)
	assert.True(t, got.ExpiresAt.Equal(request.ExpiresAt))

	byNonce, err := repo.GetByNonce(ctx, db.Querier(), request.Nonce)
	require.NoError(t, err)
	assert.Equal(t, request.ID, byNonce.ID)

	byKey, err := repo.GetByIdempotencyKey(ctx, db.Querier(), testEndpoint, "agent-key-1")
	require.NoError(t, err)
	assert.Equal(t, request.ID, byKey.ID)

	_, err = repo.GetByNonce(ctx, db.Querier(), "nonce-unknown")
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestTheAnswerSurvivesStorage is what a replay depends on: a client that lost its reply
// asks again and receives the same bytes.
func TestTheAnswerSurvivesStorage(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := payments.NewPostgresRepository()

	request := quoted(t, 2, "")
	require.NoError(t, repo.Create(ctx, db.Querier(), request))

	answer := []byte(`{"grade":"B","reserve_price":"9755.32"}`)
	require.NoError(t, request.Sign("0x9999999999999999999999999999999999999999", "pay-1", testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), request, request.Version-1))
	require.NoError(t, request.Verify(testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), request, request.Version-1))
	require.NoError(t, request.Process(answer, testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), request, request.Version-1))

	got, err := repo.Get(ctx, db.Querier(), request.ID)
	require.NoError(t, err)
	assert.Equal(t, answer, got.Response)
	assert.Equal(t, payments.Hash(answer), got.ResponseHash)
	assert.Equal(t, "pay-1", got.PaymentTx)
	assert.Equal(t, payments.StateProcessed, got.State)
}

// TestOnePaymentBuysOneAnswer is the constraint behind the whole exchange: the database
// refuses a second request claiming the same payment.
func TestOnePaymentBuysOneAnswer(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := payments.NewPostgresRepository()

	first := quoted(t, 3, "")
	require.NoError(t, repo.Create(ctx, db.Querier(), first))
	require.NoError(t, first.Sign("0x9999999999999999999999999999999999999999", "pay-shared", testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), first, first.Version-1))

	second := quoted(t, 4, "")
	require.NoError(t, repo.Create(ctx, db.Querier(), second))
	require.NoError(t, second.Sign("0x9999999999999999999999999999999999999999", "pay-shared", testNow))

	err := repo.Update(ctx, db.Querier(), second, second.Version-1)
	require.ErrorIs(t, err, apperr.ErrConflict, "the same payment cannot be recorded twice")
}

func TestANonceIsQuotedOnce(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := payments.NewPostgresRepository()

	require.NoError(t, repo.Create(ctx, db.Querier(), quoted(t, 5, "")))

	duplicate := quoted(t, 5, "")
	require.ErrorIs(t, repo.Create(ctx, db.Querier(), duplicate), apperr.ErrConflict)
}

func TestAnIdempotencyKeyIsUsedOncePerEndpoint(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := payments.NewPostgresRepository()

	require.NoError(t, repo.Create(ctx, db.Querier(), quoted(t, 6, "agent-key-1")))

	same := quoted(t, 7, "agent-key-1")
	require.ErrorIs(t, repo.Create(ctx, db.Querier(), same), apperr.ErrConflict)

	// The same key on another endpoint is a different exchange and is allowed.
	other := quoted(t, 8, "agent-key-1")
	other.Endpoint = otherEndpoint
	require.NoError(t, repo.Create(ctx, db.Querier(), other))
}

// TestPurgeKeepsWhatWasPaidFor removes prices nobody took up, and keeps the record of what
// a client actually bought.
func TestPurgeKeepsWhatWasPaidFor(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := payments.NewPostgresRepository()

	unpaid := quoted(t, 9, "")
	require.NoError(t, repo.Create(ctx, db.Querier(), unpaid))

	paid := quoted(t, 10, "")
	require.NoError(t, repo.Create(ctx, db.Querier(), paid))
	require.NoError(t, paid.Sign("0x9999999999999999999999999999999999999999", "pay-2", testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), paid, paid.Version-1))

	removed, err := repo.DeleteExpired(ctx, db.Querier(), testNow.Add(payments.ChallengeTTL+time.Minute))
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	_, err = repo.Get(ctx, db.Querier(), unpaid.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = repo.Get(ctx, db.Querier(), paid.ID)
	require.NoError(t, err, "an exchange someone paid for is a record, not a stale quote")
}

func TestPaidRequestOptimisticConcurrency(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := payments.NewPostgresRepository()

	request := quoted(t, 11, "")
	require.NoError(t, repo.Create(ctx, db.Querier(), request))

	first, err := repo.Get(ctx, db.Querier(), request.ID)
	require.NoError(t, err)
	second, err := repo.Get(ctx, db.Querier(), request.ID)
	require.NoError(t, err)

	require.NoError(t, first.Sign("0x1111111111111111111111111111111111111111", "pay-a", testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), first, first.Version-1))

	require.NoError(t, second.Sign("0x2222222222222222222222222222222222222222", "pay-b", testNow))
	err = repo.Update(ctx, db.Querier(), second, second.Version-1)
	require.ErrorIs(t, err, apperr.ErrConflict, "two payers cannot both claim one quote")
}
