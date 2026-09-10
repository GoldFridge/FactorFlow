package agents_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/agents"
	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/testsupport/memrepo"
)

var testNow = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

type fixture struct {
	service  *agents.Service
	store    *memrepo.Store
	snapshot *marketdata.Snapshot
	clock    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store := memrepo.New()
	f := &fixture{store: store, clock: testNow}

	market := marketdata.NewService(
		marketdata.NewStaticProvider(marketdata.DemoMarkets()...),
		marketdata.NewNormalizer(),
		func() time.Time { return testNow },
	)
	snapshot, err := market.Snapshot(context.Background(), marketdata.DemoQuery())
	require.NoError(t, err)
	require.NoError(t, store.Snapshots().Save(context.Background(), store.Querier(), snapshot))
	f.snapshot = snapshot

	f.service = agents.NewService(agents.Config{
		DB:        store,
		Snapshots: store.Snapshots(),
		Auctions:  store.Auctions(),
		Model:     risk.ModelV1(),
		Market:    marketdata.DemoQuery(),
		Now:       func() time.Time { return f.clock },
	})
	return f
}

func (f *fixture) quote(t *testing.T, body string) (map[string]any, error) {
	t.Helper()

	raw, err := f.service.RiskQuote(t.Context(), []byte(body))
	if err != nil {
		return nil, err
	}

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out, nil
}

const validQuote = `{
	"face": "10000.00",
	"currency": "USD",
	"days_to_due": 60,
	"features": {
		"dso_norm": "0.40",
		"late_payment_rate": "0.15",
		"dispute_flag": "0",
		"debtor_concentration": "0.38",
		"debtor_risk": "0.20"
	}
}`

// TestAQuoteIsThePublishedModelsAnswer checks that what an agent buys is the same
// computation the platform prices its own invoices with.
func TestAQuoteIsThePublishedModelsAnswer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	quote, err := f.quote(t, validQuote)
	require.NoError(t, err)

	assert.Equal(t, risk.ModelV1().Version, quote["model_version"])
	assert.Equal(t, "USD", quote["currency"])
	assert.Equal(t, f.snapshot.Benchmark.String(), quote["benchmark_apr"],
		"the benchmark is the stored market's, not one the caller supplied")
	assert.Equal(t, f.snapshot.PayloadHash, quote["market_snapshot_hash"],
		"the agent can check the quote against the same public snapshot")

	// The same numbers the platform's own assessment produces for this receivable.
	features := risk.FeatureVector{
		DSONorm:             money.MustParseRate("0.40"),
		LatePaymentRate:     money.MustParseRate("0.15"),
		DisputeFlag:         money.ZeroRate(),
		DebtorConcentration: money.MustParseRate("0.38"),
		MarketVolatility:    f.snapshot.Volatility,
		DebtorRisk:          money.MustParseRate("0.20"),
	}
	pd, err := risk.ModelV1().ProbabilityOfDefault(features)
	require.NoError(t, err)
	assert.Equal(t, pd.String(), quote["pd"])
	assert.Equal(t, risk.GradeFromPD(pd).String(), quote["grade"])
}

// TestTheSameQuestionGetsTheSameAnswer is what makes an idempotent replay honest: an agent
// billed once for an answer must be able to rely on that answer.
func TestTheSameQuestionGetsTheSameAnswer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	first, err := f.service.RiskQuote(t.Context(), []byte(validQuote))
	require.NoError(t, err)
	second, err := f.service.RiskQuote(t.Context(), []byte(validQuote))
	require.NoError(t, err)

	assert.JSONEq(t, string(first), string(second))
}

// TestAStaleMarketRefusesToQuote is where pricing differs from reporting: a price computed
// from expired market data is wrong in a way the buyer cannot see.
func TestAStaleMarketRefusesToQuote(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.clock = f.snapshot.ExpiresAt().Add(time.Second)

	_, err := f.quote(t, validQuote)
	require.ErrorIs(t, err, apperr.ErrUnavailable)
	assert.Contains(t, err.Error(), "DATA_STALE")
}

/*
 * TestQuoteWithoutAnyMarketData: a platform that cannot observe the market has nothing
 * honest to sell, and says so as a dependency being unavailable rather than as something
 * not being found.
 *
 * The distinction earns its keep on the paid path. A machine customer that meets this after
 * paying keeps its quote and may ask again when the market comes back; if this were a
 * refusal, it would have paid for an answer it could never collect.
 */
func TestQuoteWithoutAnyMarketData(t *testing.T) {
	t.Parallel()

	empty := memrepo.New()
	service := agents.NewService(agents.Config{
		DB: empty, Snapshots: empty.Snapshots(), Auctions: empty.Auctions(),
		Model: risk.ModelV1(), Market: marketdata.DemoQuery(),
		Now: func() time.Time { return testNow },
	})

	_, err := service.RiskQuote(t.Context(), []byte(validQuote))
	require.ErrorIs(t, err, apperr.ErrUnavailable)
}

func TestQuoteValidation(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "not json", body: `nope`},
		{name: "no face", body: `{"days_to_due":60}`},
		{name: "negative face", body: `{"face":"-1.00","days_to_due":60}`},
		{name: "no tenor", body: `{"face":"10000.00"}`},
		{name: "unknown currency", body: `{"face":"10000.00","currency":"XYZ","days_to_due":60}`},
		{name: "feature out of range", body: `{"face":"10000.00","days_to_due":60,"features":{"dso_norm":"4.2"}}`},
		{name: "feature not a number", body: `{"face":"10000.00","days_to_due":60,"features":{"dso_norm":"high"}}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.quote(t, tc.body)
			require.ErrorIs(t, err, apperr.ErrValidation)
		})
	}
}

// seedAuction stores an open batch with three lots of different grades and tenors.
func (f *fixture) seedAuction(t *testing.T) *auction.Auction {
	t.Helper()

	issuer := uuid.New()
	lot := func(number int, grade risk.Grade, tenor int64, reserve string) auction.Lot {
		return auction.Lot{
			ID:           uuid.New(),
			InvoiceID:    uuid.New(),
			AssetID:      uuid.New(),
			IssuerID:     issuer,
			DebtorRef:    "ACME Logistics GmbH",
			Supply:       money.MustParse("10000.00", money.USD),
			ReservePrice: money.MustParse(reserve, money.USD),
			Grade:        grade,
			TenorDays:    tenor,
		}
	}

	a, err := auction.NewAuction(auction.NewAuctionParams{
		ID:       uuid.New(),
		IssuerID: issuer,
		Lots: []auction.Lot{
			lot(1, risk.GradeB, 60, "9755.32"),
			lot(2, risk.GradeD, 60, "9600.00"),
			lot(3, risk.GradeB, 180, "9400.00"),
		},
		OpensAt:  testNow,
		ClosesAt: testNow.Add(2 * time.Hour),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, a.Open(testNow))
	require.NoError(t, f.store.Auctions().CreateAuction(t.Context(), f.store.Querier(), a))
	return a
}

func (f *fixture) recommend(t *testing.T, body string) (map[string]any, error) {
	t.Helper()

	raw, err := f.service.AuctionRecommendation(t.Context(), []byte(body))
	if err != nil {
		return nil, err
	}

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out, nil
}

// TestARecommendationAppliesTheSolversConstraints is what stops the advice and the
// clearing from disagreeing: a lot recommended here must be one the solver would allocate.
func TestARecommendationAppliesTheSolversConstraints(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	a := f.seedAuction(t)

	out, err := f.recommend(t, `{
		"auction_id": "`+a.ID.String()+`",
		"budget": "50000.00",
		"min_yield": "0.05",
		"max_grade": "C",
		"max_tenor_days": 90
	}`)
	require.NoError(t, err)

	eligible := out["eligible"].([]any)
	require.Len(t, eligible, 1, "one lot is too low a grade and one too long a tenor")

	fit := eligible[0].(map[string]any)
	assert.Equal(t, a.Lots[0].ID.String(), fit["lot_id"])
	assert.Equal(t, "B", fit["grade"])
	assert.NotEmpty(t, fit["implied_yield"])

	// Each refusal names the limit that refused it, in the solver's own vocabulary.
	rejected := out["rejected"].([]any)
	require.Len(t, rejected, 2)

	reasons := map[string]string{}
	for _, item := range rejected {
		entry := item.(map[string]any)
		reasons[entry["lot_id"].(string)] = entry["constraint"].(string)
	}
	assert.Equal(t, string(auction.ConstraintGrade), reasons[a.Lots[1].ID.String()])
	assert.Equal(t, string(auction.ConstraintMaturity), reasons[a.Lots[2].ID.String()])
}

// TestARecommendationReportsWhatItWouldCost answers the question an investor actually has
// before bidding: is my budget enough for what I said I wanted.
func TestARecommendationReportsWhatItWouldCost(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	a := f.seedAuction(t)

	out, err := f.recommend(t, `{
		"auction_id": "`+a.ID.String()+`",
		"budget": "50000.00",
		"min_yield": "0.01",
		"max_grade": "E"
	}`)
	require.NoError(t, err)

	assert.Len(t, out["eligible"], 3)
	assert.Equal(t, "28755.32", out["suggested_budget"], "the three reserve prices")
	assert.Equal(t, "0.00", out["budget_shortfall"])
	assert.Equal(t, a.Status.String(), out["status"])

	// A budget that cannot cover them is told by how much.
	out, err = f.recommend(t, `{
		"auction_id": "`+a.ID.String()+`",
		"budget": "10000.00",
		"min_yield": "0.01",
		"max_grade": "E"
	}`)
	require.NoError(t, err)
	assert.Equal(t, "18755.32", out["budget_shortfall"])
}

// TestAMandateNothingFitsIsAnAnswer keeps an empty recommendation from being an error: the
// agent paid for the analysis, and "nothing here suits you" is the analysis.
func TestAMandateNothingFitsIsAnAnswer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	a := f.seedAuction(t)

	out, err := f.recommend(t, `{
		"auction_id": "`+a.ID.String()+`",
		"budget": "50000.00",
		"min_yield": "0.95",
		"max_grade": "A"
	}`)
	require.NoError(t, err)

	assert.Empty(t, out["eligible"])
	assert.Len(t, out["rejected"], 3)
	assert.Equal(t, "0.00", out["suggested_budget"])
}

func TestRecommendationValidation(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.recommend(t, `nope`)
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.recommend(t, `{"auction_id":"not-a-uuid"}`)
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.recommend(t, `{"auction_id":"`+uuid.NewString()+`","max_grade":"Z"}`)
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.recommend(t, `{"auction_id":"`+uuid.NewString()+`"}`)
	require.ErrorIs(t, err, apperr.ErrNotFound)
}
