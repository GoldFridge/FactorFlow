package marketdata_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

var observedAt = time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

func normalize(t *testing.T, markets []marketdata.Market) *marketdata.Snapshot {
	t.Helper()

	snapshot, err := marketdata.NewNormalizer().Normalize(marketdata.DemoQuery(), markets, observedAt)
	require.NoError(t, err)
	return snapshot
}

// TestNormalizeDemoSnapshot pins the published normalization against the demo market set.
func TestNormalizeDemoSnapshot(t *testing.T) {
	t.Parallel()

	snapshot := normalize(t, marketdata.DemoMarkets())

	assert.Equal(t, "0.064200", snapshot.Benchmark.StringFixed(6), "liquidity-weighted median APY")
	assert.Equal(t, "0.015357", snapshot.LiquidityPremium.StringFixed(6), "95 / sqrt(total liquidity)")
	assert.Equal(t, "0.125000", snapshot.Volatility.StringFixed(6), "winsorized spread over the 0.10 scale")
	assert.Equal(t, "38270000.00", snapshot.TotalLiquidity.String())

	assert.Len(t, snapshot.Markets, 4, "the drained market is excluded")
	assert.Equal(t, []string{"demo-lending-v1", "demo-yield-v1"}, snapshot.SubgraphIDs)
	assert.Equal(t, marketdata.DefaultTTL, snapshot.TTL)
	assert.Equal(t, observedAt, snapshot.ObservedAt)
	assert.Regexp(t, `^0x[0-9a-f]{64}$`, snapshot.QueryHash)
	assert.Regexp(t, `^0x[0-9a-f]{64}$`, snapshot.PayloadHash)
}

func TestZeroLiquidityMarketsAreExcluded(t *testing.T) {
	t.Parallel()

	snapshot := normalize(t, marketdata.DemoMarkets())

	for _, m := range snapshot.Markets {
		assert.NotEqual(t, "usdc-drained", m.ID, "a market nobody can borrow from quotes no usable yield")
		assert.True(t, m.AvailableLiquidity.IsPositive())
	}
}

func TestNoEligibleMarketsFailsClosed(t *testing.T) {
	t.Parallel()

	n := marketdata.NewNormalizer()

	_, err := n.Normalize(marketdata.DemoQuery(), nil, observedAt)
	require.ErrorIs(t, err, apperr.ErrUnavailable)

	drained := []marketdata.Market{{
		ID: "usdc-drained", SubgraphID: "demo-yield-v1", Asset: "USDC",
		NetSupplyAPY:       money.MustParseRate("0.0350"),
		AvailableLiquidity: money.Zero(money.USD),
	}}
	_, err = n.Normalize(marketdata.DemoQuery(), drained, observedAt)
	require.ErrorIs(t, err, apperr.ErrUnavailable, "an empty market set is a dependency failure, not a zero benchmark")
}

// TestWinsorizationAbsorbsAManipulatedMarket is the control against price manipulation: a
// market quoting an absurd yield must not move the benchmark.
func TestWinsorizationAbsorbsAManipulatedMarket(t *testing.T) {
	t.Parallel()

	clean := normalize(t, marketdata.DemoMarkets())

	manipulated := append(marketdata.DemoMarkets(), marketdata.Market{
		ID: "usdc-manipulated", SubgraphID: "demo-yield-v1", Asset: "USDC",
		NetSupplyAPY:       money.MustParseRate("5.0"),
		AvailableLiquidity: money.MustParse("50000.00", money.USD),
		BlockNumber:        21_450_100,
	})
	attacked := normalize(t, manipulated)

	assert.Equal(t, clean.Benchmark.String(), attacked.Benchmark.String(),
		"a 500%% outlier is clipped, not averaged in")
	assert.NotEqual(t, clean.PayloadHash, attacked.PayloadHash, "the extra row is still recorded")
}

// TestBenchmarkFollowsLiquidityWeight checks that depth, not row count, decides the
// benchmark: three dust pools cannot outvote one deep market.
func TestBenchmarkFollowsLiquidityWeight(t *testing.T) {
	t.Parallel()

	markets := []marketdata.Market{
		{ID: "deep", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.05"), AvailableLiquidity: money.MustParse("90000000.00", money.USD)},
		{ID: "dust-a", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.12"), AvailableLiquidity: money.MustParse("1000.00", money.USD)},
		{ID: "dust-b", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.13"), AvailableLiquidity: money.MustParse("1000.00", money.USD)},
		{ID: "dust-c", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.14"), AvailableLiquidity: money.MustParse("1000.00", money.USD)},
	}

	snapshot := normalize(t, markets)
	assert.Equal(t, "0.050000", snapshot.Benchmark.StringFixed(6))
}

// TestNormalizeIsOrderIndependent proves the gateway's row order cannot change a price or
// a hash, which is what makes a snapshot reproducible.
func TestNormalizeIsOrderIndependent(t *testing.T) {
	t.Parallel()

	forward := marketdata.DemoMarkets()
	reversed := make([]marketdata.Market, 0, len(forward))
	for i := len(forward) - 1; i >= 0; i-- {
		reversed = append(reversed, forward[i])
	}

	first := normalize(t, forward)
	second := normalize(t, reversed)

	assert.Equal(t, first.Benchmark.String(), second.Benchmark.String())
	assert.Equal(t, first.LiquidityPremium.String(), second.LiquidityPremium.String())
	assert.Equal(t, first.Volatility.String(), second.Volatility.String())
	assert.Equal(t, first.PayloadHash, second.PayloadHash)
	assert.Equal(t, first.QueryHash, second.QueryHash)
}

func TestNormalizeIsDeterministic(t *testing.T) {
	t.Parallel()

	first := normalize(t, marketdata.DemoMarkets())
	for range 20 {
		again := normalize(t, marketdata.DemoMarkets())
		require.Equal(t, first.PayloadHash, again.PayloadHash)
		require.Equal(t, first.Benchmark.String(), again.Benchmark.String())
	}
}

// TestMarketMovesTheBenchmark is the counterfactual the demo shows: when the live market
// moves, the benchmark and the snapshot identity move with it.
func TestMarketMovesTheBenchmark(t *testing.T) {
	t.Parallel()

	before := normalize(t, marketdata.DemoMarkets())

	shifted := marketdata.DemoMarkets()
	for i := range shifted {
		shifted[i].NetSupplyAPY = shifted[i].NetSupplyAPY.Add(money.MustParseRate("0.0100"))
	}
	after := normalize(t, shifted)

	assert.Equal(t, "0.074200", after.Benchmark.StringFixed(6))
	assert.NotEqual(t, before.PayloadHash, after.PayloadHash)
	assert.Equal(t, before.QueryHash, after.QueryHash, "the same question, a different answer")
}

func TestLiquidityPremiumIsClamped(t *testing.T) {
	t.Parallel()

	thin := []marketdata.Market{
		{ID: "a", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.05"), AvailableLiquidity: money.MustParse("100.00", money.USD)},
	}
	deep := []marketdata.Market{
		{ID: "a", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.05"), AvailableLiquidity: money.MustParse("9000000000.00", money.USD)},
	}

	params := marketdata.NormalizerParamsV1()
	assert.True(t, normalize(t, thin).LiquidityPremium.Equal(params.LiquidityPremiumCeiling), "a thin market is capped")
	assert.True(t, normalize(t, deep).LiquidityPremium.Equal(params.LiquidityPremiumFloor), "a deep market still pays the floor")
}

func TestSingleMarketHasNoVolatility(t *testing.T) {
	t.Parallel()

	single := []marketdata.Market{
		{ID: "a", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.05"), AvailableLiquidity: money.MustParse("1000000.00", money.USD)},
	}

	snapshot := normalize(t, single)
	assert.True(t, snapshot.Volatility.IsZero())
	assert.Equal(t, "0.050000", snapshot.Benchmark.StringFixed(6))
}

func TestNormalizeRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	n := marketdata.NewNormalizer()
	valid := marketdata.Market{
		ID: "a", SubgraphID: "s1", Asset: "USDC",
		NetSupplyAPY: money.MustParseRate("0.05"), AvailableLiquidity: money.MustParse("1000.00", money.USD),
	}

	t.Run("market fields", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name      string
			mutate    func(*marketdata.Market)
			wantField string
		}{
			{name: "missing id", mutate: func(m *marketdata.Market) { m.ID = " " }, wantField: "market.id"},
			{name: "missing subgraph", mutate: func(m *marketdata.Market) { m.SubgraphID = "" }, wantField: "market.subgraph_id"},
			{name: "negative apy", mutate: func(m *marketdata.Market) { m.NetSupplyAPY = money.MustParseRate("-0.01") }, wantField: "market.net_supply_apy"},
			{name: "no currency", mutate: func(m *marketdata.Market) { m.AvailableLiquidity = money.Amount{} }, wantField: "market.available_liquidity"},
			{name: "negative liquidity", mutate: func(m *marketdata.Market) {
				m.AvailableLiquidity = money.MustParse("-1.00", money.USD)
			}, wantField: "market.available_liquidity"},
			{name: "negative block", mutate: func(m *marketdata.Market) { m.BlockNumber = -1 }, wantField: "market.block_number"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				m := valid
				tc.mutate(&m)

				_, err := n.Normalize(marketdata.DemoQuery(), []marketdata.Market{m}, observedAt)
				require.ErrorIs(t, err, apperr.ErrValidation)
				assert.Contains(t, fieldNames(apperr.Fields(err)), tc.wantField)
			})
		}
	})

	t.Run("query fields", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name      string
			mutate    func(*marketdata.Query)
			wantField string
		}{
			{name: "missing provider", mutate: func(q *marketdata.Query) { q.Provider = "" }, wantField: "query.provider"},
			{name: "missing network", mutate: func(q *marketdata.Query) { q.Network = " " }, wantField: "query.network"},
			{name: "missing asset", mutate: func(q *marketdata.Query) { q.Asset = "" }, wantField: "query.asset"},
			{name: "missing graphql", mutate: func(q *marketdata.Query) { q.GraphQL = "" }, wantField: "query.graphql"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				q := marketdata.DemoQuery()
				tc.mutate(&q)

				_, err := n.Normalize(q, []marketdata.Market{valid}, observedAt)
				require.ErrorIs(t, err, apperr.ErrValidation)
				assert.Contains(t, fieldNames(apperr.Fields(err)), tc.wantField)
			})
		}
	})
}

func TestMixedCurrenciesAreRejected(t *testing.T) {
	t.Parallel()

	markets := []marketdata.Market{
		{ID: "a", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.05"), AvailableLiquidity: money.MustParse("1000.00", money.USD)},
		{ID: "b", SubgraphID: "s1", Asset: "USDC", NetSupplyAPY: money.MustParseRate("0.06"), AvailableLiquidity: money.MustParse("1000.00", money.EUR)},
	}

	_, err := marketdata.NewNormalizer().Normalize(marketdata.DemoQuery(), markets, observedAt)
	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.Contains(t, fieldNames(apperr.Fields(err)), "market.available_liquidity")
}

func fieldNames(fields []*apperr.FieldError) []string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Field)
	}
	return names
}
