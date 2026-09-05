package marketdata

import (
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
)

const (
	// rateScale is the number of decimal digits kept on derived rates, matching the risk
	// model's scale so a benchmark never loses precision on the way into pricing.
	rateScale int32 = 6
	// sqrtScale is the working precision of the integer square root used for liquidity
	// weighting. Weights only order and scale markets, so six digits is ample.
	sqrtScale int32 = 6
)

// NormalizerParams are the published constants of the benchmark computation.
type NormalizerParams struct {
	// WinsorPercentile is the tail share clipped at each end before the median, as a
	// fraction: 0.10 clips the bottom and top decile. It stops one mispriced or
	// manipulated market from dragging the benchmark.
	WinsorPercentile money.Rate

	// LiquidityConstant k in liquidity_premium = k / sqrt(total_eligible_liquidity).
	LiquidityConstant money.Rate
	// LiquidityPremiumFloor and LiquidityPremiumCeiling clamp that premium.
	LiquidityPremiumFloor   money.Rate
	LiquidityPremiumCeiling money.Rate

	// VolatilityScale is the APY spread that maps to a volatility of 1.0.
	VolatilityScale money.Rate

	// TTL is the freshness window written onto the snapshot.
	TTL time.Duration
}

// NormalizerParamsV1 returns the published constants.
//
// LiquidityConstant is calibrated so that a market with roughly 40M of eligible liquidity
// carries a 1.5% premium, and the clamp keeps the premium inside [0.25%, 5%] regardless of
// how thin or deep the market becomes.
func NormalizerParamsV1() NormalizerParams {
	return NormalizerParams{
		WinsorPercentile:        money.MustParseRate("0.10"),
		LiquidityConstant:       money.MustParseRate("95"),
		LiquidityPremiumFloor:   money.MustParseRate("0.0025"),
		LiquidityPremiumCeiling: money.MustParseRate("0.05"),
		VolatilityScale:         money.MustParseRate("0.10"),
		TTL:                     DefaultTTL,
	}
}

// Normalizer builds snapshots from raw market rows.
type Normalizer struct {
	Params NormalizerParams
}

// NewNormalizer returns a normalizer with the published constants.
func NewNormalizer() *Normalizer { return &Normalizer{Params: NormalizerParamsV1()} }

// Normalize filters, winsorizes and aggregates market rows into a snapshot.
//
// Rows with zero liquidity are excluded: an empty market quotes a yield nobody can take.
// If nothing eligible remains, the result is a dependency failure rather than a benchmark
// invented from thin data.
func (n *Normalizer) Normalize(q Query, markets []Market, observedAt time.Time) (*Snapshot, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}

	var violations []error
	for _, m := range markets {
		if err := m.Validate(); err != nil {
			violations = append(violations, err)
		}
	}
	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	eligible := make([]Market, 0, len(markets))
	for _, m := range markets {
		if m.AvailableLiquidity.IsPositive() {
			eligible = append(eligible, m)
		}
	}
	if len(eligible) == 0 {
		return nil, apperr.Unavailablef("no eligible %s markets with liquidity in %s", q.Asset, q.Network)
	}

	currency := eligible[0].AvailableLiquidity.Currency()
	for _, m := range eligible {
		if m.AvailableLiquidity.Currency() != currency {
			return nil, apperr.Invalid("market.available_liquidity",
				"markets mix currencies %s and %s", currency, m.AvailableLiquidity.Currency())
		}
	}

	// Canonical order first: everything downstream, including the payload hash, must not
	// depend on the order the gateway happened to return rows in.
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].SubgraphID != eligible[j].SubgraphID {
			return eligible[i].SubgraphID < eligible[j].SubgraphID
		}
		return eligible[i].ID < eligible[j].ID
	})

	totalLiquidity, err := totalLiquidityOf(eligible)
	if err != nil {
		return nil, err
	}

	winsorized := n.winsorize(eligible)
	benchmark := weightedMedianAPY(winsorized)

	liquidityPremium, err := n.liquidityPremium(totalLiquidity)
	if err != nil {
		return nil, err
	}

	snapshot := &Snapshot{
		Provider:         q.Provider,
		Network:          q.Network,
		Asset:            q.Asset,
		QueryHash:        hashQuery(q),
		Markets:          eligible,
		SubgraphIDs:      distinctSubgraphIDs(eligible),
		BlockNumbers:     blockNumbersOf(eligible),
		Benchmark:        benchmark.Quantize(rateScale),
		LiquidityPremium: liquidityPremium,
		Volatility:       n.volatility(winsorized),
		TotalLiquidity:   totalLiquidity,
		ObservedAt:       observedAt.UTC(),
		TTL:              n.Params.TTL,
	}
	snapshot.PayloadHash = hashPayload(snapshot)

	return snapshot, nil
}

// winsorize clips the APY tails to the published percentile of *liquidity*, not of row
// count, using nearest-rank so each bound is a rate actually quoted by some market.
//
// Weighting the quantiles matters: a benchmark computed from unweighted ranks would clip a
// deep market that happens to quote the lowest rate just because three dust-sized pools
// quote higher ones. Weighted, the tails that get clipped are the ones almost nobody can
// actually trade against, which is exactly the manipulated or stale row this defends
// against.
func (n *Normalizer) winsorize(markets []Market) []Market {
	byAPY := sortedByAPY(markets)
	if len(byAPY) < 2 {
		return byAPY
	}

	weights := make([]decimal.Decimal, len(byAPY))
	total := decimal.Zero
	for i, m := range byAPY {
		weights[i] = sqrtDecimal(m.AvailableLiquidity.Decimal(), sqrtScale)
		total = total.Add(weights[i])
	}
	if total.IsZero() {
		return byAPY
	}

	p := n.Params.WinsorPercentile.Decimal()
	lowTarget := total.Mul(p)
	highTarget := total.Mul(decimal.NewFromInt(1).Sub(p))

	low, high := byAPY[0].NetSupplyAPY, byAPY[len(byAPY)-1].NetSupplyAPY
	cumulative := decimal.Zero
	lowFound := false
	for i, w := range weights {
		cumulative = cumulative.Add(w)
		if !lowFound && cumulative.GreaterThanOrEqual(lowTarget) {
			low = byAPY[i].NetSupplyAPY
			lowFound = true
		}
		if cumulative.GreaterThanOrEqual(highTarget) {
			high = byAPY[i].NetSupplyAPY
			break
		}
	}

	clipped := append([]Market(nil), byAPY...)
	for i := range clipped {
		clipped[i].NetSupplyAPY = clipped[i].NetSupplyAPY.Clamp(low, high)
	}
	return clipped
}

// sortedByAPY returns a copy ordered by rate, then by subgraph and market id so equal
// rates never depend on the order the gateway returned them in.
func sortedByAPY(markets []Market) []Market {
	out := append([]Market(nil), markets...)
	sort.SliceStable(out, func(i, j int) bool {
		if cmp := out[i].NetSupplyAPY.Cmp(out[j].NetSupplyAPY); cmp != 0 {
			return cmp < 0
		}
		if out[i].SubgraphID != out[j].SubgraphID {
			return out[i].SubgraphID < out[j].SubgraphID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// weightedMedianAPY is the median net supply APY weighted by sqrt(available liquidity).
//
// The square root keeps a single very deep market from dictating the benchmark on its own,
// while still letting depth count for more than a dust-sized pool.
func weightedMedianAPY(markets []Market) money.Rate {
	if len(markets) == 0 {
		return money.ZeroRate()
	}

	byAPY := sortedByAPY(markets)

	weights := make([]decimal.Decimal, len(byAPY))
	total := decimal.Zero
	for i, m := range byAPY {
		weights[i] = sqrtDecimal(m.AvailableLiquidity.Decimal(), sqrtScale)
		total = total.Add(weights[i])
	}

	half := total.Div(decimal.NewFromInt(2))
	cumulative := decimal.Zero
	for i, w := range weights {
		cumulative = cumulative.Add(w)
		if cumulative.GreaterThanOrEqual(half) {
			return byAPY[i].NetSupplyAPY
		}
	}
	return byAPY[len(byAPY)-1].NetSupplyAPY
}

// liquidityPremium is clamp(k / sqrt(total_eligible_liquidity), floor, ceiling).
func (n *Normalizer) liquidityPremium(totalLiquidity money.Amount) (money.Rate, error) {
	root := sqrtDecimal(totalLiquidity.Decimal(), sqrtScale)
	if root.IsZero() {
		return n.Params.LiquidityPremiumCeiling, nil
	}

	premium, err := n.Params.LiquidityConstant.Div(money.NewRate(root))
	if err != nil {
		return money.Rate{}, err
	}
	return premium.Quantize(rateScale).Clamp(n.Params.LiquidityPremiumFloor, n.Params.LiquidityPremiumCeiling), nil
}

// volatility maps the winsorized APY spread onto [0,1] against the published scale, so the
// risk model receives a normalized feature like every other input.
func (n *Normalizer) volatility(markets []Market) money.Rate {
	if len(markets) < 2 {
		return money.ZeroRate()
	}

	low, high := markets[0].NetSupplyAPY, markets[0].NetSupplyAPY
	for _, m := range markets[1:] {
		low = money.MinRate(low, m.NetSupplyAPY)
		high = money.MaxRate(high, m.NetSupplyAPY)
	}

	spread, err := high.Sub(low).Div(n.Params.VolatilityScale)
	if err != nil {
		return money.ZeroRate()
	}
	return spread.Quantize(rateScale).Clamp(money.ZeroRate(), money.OneRate())
}

func totalLiquidityOf(markets []Market) (money.Amount, error) {
	amounts := make([]money.Amount, 0, len(markets))
	for _, m := range markets {
		amounts = append(amounts, m.AvailableLiquidity)
	}
	return money.Sum(amounts...)
}

func distinctSubgraphIDs(markets []Market) []string {
	seen := make(map[string]struct{}, len(markets))
	out := make([]string, 0, len(markets))
	for _, m := range markets {
		if _, ok := seen[m.SubgraphID]; ok {
			continue
		}
		seen[m.SubgraphID] = struct{}{}
		out = append(out, m.SubgraphID)
	}
	sort.Strings(out)
	return out
}

func blockNumbersOf(markets []Market) []int64 {
	out := make([]int64, 0, len(markets))
	for _, m := range markets {
		out = append(out, m.BlockNumber)
	}
	return out
}

func (q Query) validate() error {
	var violations []error

	if strings.TrimSpace(q.Provider) == "" {
		violations = append(violations, apperr.Invalid("query.provider", "must not be empty"))
	}
	if strings.TrimSpace(q.Network) == "" {
		violations = append(violations, apperr.Invalid("query.network", "must not be empty"))
	}
	if strings.TrimSpace(q.Asset) == "" {
		violations = append(violations, apperr.Invalid("query.asset", "must not be empty"))
	}
	if strings.TrimSpace(q.GraphQL) == "" {
		violations = append(violations, apperr.Invalid("query.graphql", "must not be empty"))
	}

	return errors.Join(violations...)
}

// sqrtDecimal computes a square root to a fixed number of digits using integer arithmetic.
//
// It is used instead of a floating-point sqrt so that liquidity weights, and therefore the
// benchmark, are identical on every machine that recomputes a snapshot.
func sqrtDecimal(d decimal.Decimal, scale int32) decimal.Decimal {
	if d.IsNegative() || d.IsZero() {
		return decimal.Zero
	}
	// Shift by 2*scale so the integer square root carries `scale` digits of precision.
	shifted := d.Shift(2 * scale).Truncate(0)
	root := new(big.Int).Sqrt(shifted.BigInt())
	return decimal.NewFromBigInt(root, -scale)
}
