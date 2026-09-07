package issuance_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/issuance"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/testsupport/memrepo"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

var testNow = time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)

// staticWallets answers the one question the worker asks about an organization.
type staticWallets struct {
	wallet string
	err    error
}

func (s staticWallets) WalletOf(context.Context, postgres.Querier, uuid.UUID) (string, error) {
	return s.wallet, s.err
}

// failingIssuer stands in for a network that is not answering.
type failingIssuer struct{ err error }

func (f failingIssuer) Issue(context.Context, tokenization.IssueRequest) (tokenization.IssueResult, error) {
	return tokenization.IssueResult{}, f.err
}

// lyingIssuer returns a result that cannot be trusted.
type lyingIssuer struct{ result tokenization.IssueResult }

func (l lyingIssuer) Issue(context.Context, tokenization.IssueRequest) (tokenization.IssueResult, error) {
	return l.result, nil
}

// countingIssuer records how often it was asked to mint.
type countingIssuer struct {
	inner tokenization.Issuer
	calls int
}

func (c *countingIssuer) Issue(ctx context.Context, request tokenization.IssueRequest) (tokenization.IssueResult, error) {
	c.calls++
	return c.inner.Issue(ctx, request)
}

type fixture struct {
	worker  *issuance.Worker
	store   *memrepo.Store
	issuer  *countingIssuer
	invoice *invoice.Invoice
	clock   time.Time
}

func newFixture(t *testing.T, assetIssuer tokenization.Issuer, wallets issuance.OrganizationWallets) *fixture {
	t.Helper()

	store := memrepo.New()
	inv := approvedInvoice(t)
	store.PutInvoice(inv)
	store.PutAssessment(assessmentFor(t, inv))

	if assetIssuer == nil {
		assetIssuer = tokenization.NewLocalIssuer()
	}
	if wallets == nil {
		wallets = staticWallets{wallet: "0x1111111111111111111111111111111111111111"}
	}

	f := &fixture{
		store:   store,
		issuer:  &countingIssuer{inner: assetIssuer},
		invoice: inv,
		clock:   testNow,
	}
	f.worker = issuance.NewWorker(issuance.Config{
		DB:          store,
		Invoices:    store.Invoices(),
		Assessments: store.Assessments(),
		Assets:      store.Assets(),
		Wallets:     wallets,
		Issuer:      f.issuer,
		Now:         func() time.Time { return f.clock },
		IDs:         uuid.New,
	})
	return f
}

// approvedInvoice returns an invoice that has been approved and is now being tokenized.
func approvedInvoice(t *testing.T) *invoice.Invoice {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		IssuerID:  uuid.New(),
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0042",
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)

	require.NoError(t, inv.MarkUploaded(testNow))
	require.NoError(t, inv.StartAssessment(testNow))
	require.NoError(t, inv.CompleteAssessment(uuid.New(), testNow))
	require.NoError(t, inv.Approve(testNow))
	require.NoError(t, inv.StartTokenization(testNow))
	return inv
}

func assessmentFor(t *testing.T, inv *invoice.Invoice) *risk.Assessment {
	t.Helper()

	assessment, err := risk.ModelV1().Assess(risk.AssessInput{
		ID:        inv.AssessmentID,
		InvoiceID: inv.ID,
		Face:      inv.Face,
		DaysToDue: 60,
		Features: risk.FeatureVector{
			DSONorm:             money.MustParseRate("0.40"),
			LatePaymentRate:     money.MustParseRate("0.15"),
			DisputeFlag:         money.ZeroRate(),
			DebtorConcentration: money.MustParseRate("0.38"),
			MarketVolatility:    money.MustParseRate("0.25"),
			DebtorRisk:          money.MustParseRate("0.20"),
		},
		Confidence:             money.MustParseRate("0.93"),
		ArithmeticValid:        true,
		Benchmark:              money.MustParseRate("0.0640"),
		LiquidityPremium:       money.MustParseRate("0.0150"),
		MarketSnapshotHash:     "0x3d2f1c0b9a8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4",
		ConfidentialCommitment: "0x1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
	}, testNow)
	require.NoError(t, err)
	return assessment
}

func (f *fixture) event(t *testing.T) outbox.Event {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"invoice_id": f.invoice.ID,
		"issuer_id":  f.invoice.IssuerID,
	})
	require.NoError(t, err)
	return outbox.Event{ID: 1, Topic: invoice.TopicTokenize, Payload: payload}
}

func TestWorkerIssuesTheAsset(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, nil)
	require.NoError(t, f.worker.Handle(context.Background(), f.event(t)))

	stored, ok := f.store.Invoice(f.invoice.ID)
	require.True(t, ok)
	assert.Equal(t, invoice.StatusTokenized, stored.Status)
	require.NotEqual(t, uuid.Nil, stored.AssetID)

	asset, ok := f.store.Asset(stored.AssetID)
	require.True(t, ok)
	assert.Equal(t, f.invoice.ID, asset.InvoiceID)
	assert.Equal(t, "10000.00", asset.Supply.String(), "the asset represents the whole face value")
	assert.Equal(t, tokenization.StatusIssued, asset.ChainStatus)
	assert.True(t, asset.IsTransferable())
}

// TestRedeliveryDoesNotMintTwice is the rule that matters most here: two assets for one
// receivable would be two claims on the same money, and no reconciliation could undo it.
func TestRedeliveryDoesNotMintTwice(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, nil)
	event := f.event(t)

	require.NoError(t, f.worker.Handle(context.Background(), event))
	require.NoError(t, f.worker.Handle(context.Background(), event))

	assert.Equal(t, 1, f.store.AssetCount())
	assert.Equal(t, 1, f.issuer.calls, "the second delivery did not reach the issuer")
}

// TestAssetWithoutTheInvoiceMoveIsFinished covers the crash window: the asset was minted
// and stored, but the invoice never moved. A redelivery must finish the job, not mint again.
func TestAssetWithoutTheInvoiceMoveIsFinished(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, nil)

	asset, err := tokenization.New(tokenization.NewParams{
		ID:          uuid.New(),
		InvoiceID:   f.invoice.ID,
		IssuerID:    f.invoice.IssuerID,
		Network:     tokenization.LocalNetwork,
		TokenID:     "local-token-existing",
		Supply:      f.invoice.Face,
		ChainStatus: tokenization.StatusIssued,
	}, testNow)
	require.NoError(t, err)
	f.store.PutAsset(asset)

	require.NoError(t, f.worker.Handle(context.Background(), f.event(t)))

	stored, ok := f.store.Invoice(f.invoice.ID)
	require.True(t, ok)
	assert.Equal(t, invoice.StatusTokenized, stored.Status)
	assert.Equal(t, asset.ID, stored.AssetID, "the invoice points at the asset that already existed")
	assert.Equal(t, 1, f.store.AssetCount())
	assert.Equal(t, 0, f.issuer.calls)
}

func TestAlreadyTokenizedInvoiceIsSkipped(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, nil)
	require.NoError(t, f.worker.Handle(context.Background(), f.event(t)))

	// A much later redelivery, after the invoice has moved on to an auction.
	stored, _ := f.store.Invoice(f.invoice.ID)
	require.NoError(t, stored.OpenAuction(testNow.Add(time.Hour)))
	f.store.PutInvoice(&stored)

	require.NoError(t, f.worker.Handle(context.Background(), f.event(t)))
	assert.Equal(t, 1, f.issuer.calls)

	current, _ := f.store.Invoice(f.invoice.ID)
	assert.Equal(t, invoice.StatusAuctionOpen, current.Status, "the redelivery changed nothing")
}

// TestIssuerFailureFailsTheInvoiceAndRetries checks both halves: the invoice records which
// stage failed, and the error propagates so the outbox retries with backoff.
func TestIssuerFailureFailsTheInvoiceAndRetries(t *testing.T) {
	t.Parallel()

	f := newFixture(t, failingIssuer{err: errors.New("hedera node refused the transaction")}, nil)

	err := f.worker.Handle(context.Background(), f.event(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, apperr.ErrUnavailable)

	stored, ok := f.store.Invoice(f.invoice.ID)
	require.True(t, ok)
	assert.Equal(t, invoice.StatusFailed, stored.Status)
	assert.Equal(t, invoice.StatusTokenizing, stored.FailedFrom, "the retry resumes issuance, not assessment")
	assert.Contains(t, stored.Reason, "hedera node refused")
	assert.Equal(t, 0, f.store.AssetCount())
}

// TestUntrustedIssuerResultIsRejected treats the issuer as what it is: an external system
// whose answer is input, not truth.
func TestUntrustedIssuerResultIsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result tokenization.IssueResult
	}{
		{name: "no network", result: tokenization.IssueResult{TokenID: "t-1", Confirmed: true}},
		{name: "no token", result: tokenization.IssueResult{Network: "local", Confirmed: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, lyingIssuer{result: tc.result}, nil)

			err := f.worker.Handle(context.Background(), f.event(t))
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Equal(t, 0, f.store.AssetCount())

			stored, _ := f.store.Invoice(f.invoice.ID)
			assert.Equal(t, invoice.StatusFailed, stored.Status)
		})
	}
}

// TestUnconfirmedIssuanceStaysPending covers the live-chain case: a network that has not
// finalized yet leaves an asset nobody may settle until reconciliation confirms it.
func TestUnconfirmedIssuanceStaysPending(t *testing.T) {
	t.Parallel()

	f := newFixture(t, lyingIssuer{result: tokenization.IssueResult{
		Network:       "hedera-testnet",
		TokenID:       "0.0.4823901",
		TransactionID: "0.0.1234@1757160000.000000000",
		Confirmed:     false,
	}}, nil)

	require.NoError(t, f.worker.Handle(context.Background(), f.event(t)))

	stored, _ := f.store.Invoice(f.invoice.ID)
	asset, ok := f.store.Asset(stored.AssetID)
	require.True(t, ok)

	assert.Equal(t, tokenization.StatusPending, asset.ChainStatus)
	assert.False(t, asset.IsTransferable(), "an unconfirmed asset cannot be sold")
}

func TestMissingAssessmentFailsTheInvoice(t *testing.T) {
	t.Parallel()

	store := memrepo.New()
	inv := approvedInvoice(t)
	store.PutInvoice(inv)
	// No assessment is seeded: an unpriced receivable must not reach the chain.

	worker := issuance.NewWorker(issuance.Config{
		DB: store, Invoices: store.Invoices(), Assessments: store.Assessments(),
		Assets: store.Assets(), Wallets: staticWallets{wallet: "0xabc"},
		Issuer: tokenization.NewLocalIssuer(), Now: func() time.Time { return testNow },
	})

	payload, err := json.Marshal(map[string]any{"invoice_id": inv.ID, "issuer_id": inv.IssuerID})
	require.NoError(t, err)

	err = worker.Handle(context.Background(), outbox.Event{Payload: payload})
	require.ErrorIs(t, err, apperr.ErrNotFound)

	stored, _ := store.Invoice(inv.ID)
	assert.Equal(t, invoice.StatusFailed, stored.Status)
	assert.Equal(t, 0, store.AssetCount())
}

func TestUnknownWalletFailsTheInvoice(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, staticWallets{err: apperr.NotFoundf("organization")})

	err := f.worker.Handle(context.Background(), f.event(t))
	require.ErrorIs(t, err, apperr.ErrNotFound)
	assert.Equal(t, 0, f.issuer.calls, "nothing was minted for an organization we cannot pay")
}

// TestNothingIsStoredWhenTheCommitFails keeps the asset and the invoice together: an asset
// whose invoice never moved would be a claim nobody can trace back.
func TestNothingIsStoredWhenTheCommitFails(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, nil)
	f.store.FailCommit = errors.New("commit failed")

	require.Error(t, f.worker.Handle(context.Background(), f.event(t)))
	f.store.FailCommit = nil

	assert.Equal(t, 0, f.store.AssetCount())
	stored, _ := f.store.Invoice(f.invoice.ID)
	assert.Equal(t, invoice.StatusTokenizing, stored.Status, "the outbox will retry the whole issuance")
}

func TestMalformedCommand(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, nil)

	require.Error(t, f.worker.Handle(context.Background(), outbox.Event{Payload: []byte("{not json")}))
	require.ErrorIs(t, f.worker.Handle(context.Background(), outbox.Event{Payload: []byte(`{"invoice_id":null}`)}),
		apperr.ErrValidation)
}

func TestUnknownInvoice(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, nil)
	payload, err := json.Marshal(map[string]any{"invoice_id": uuid.New()})
	require.NoError(t, err)

	err = f.worker.Handle(context.Background(), outbox.Event{Payload: payload})
	require.ErrorIs(t, err, apperr.ErrNotFound)
}
