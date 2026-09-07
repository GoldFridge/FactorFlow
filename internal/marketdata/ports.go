package marketdata

import (
	"context"
	"sort"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// Provider fetches standardized lending markets from an external source.
//
// The live implementation queries The Graph gateway. Every external dependency in
// FactorFlow sits behind an interface like this one, so the full pricing path can be
// tested offline while the submitted demo runs against the live gateway.
type Provider interface {
	// FetchMarkets returns the markets matching q, or an error the caller must treat as a
	// pricing failure rather than an empty market.
	FetchMarkets(ctx context.Context, q Query) ([]Market, error)
}

// Service produces fresh snapshots from a provider.
type Service struct {
	provider   Provider
	normalizer *Normalizer
	now        func() time.Time
}

// NewService wires a provider to the normalizer. The clock is injected so tests and the
// deterministic demo seed can control observation times.
func NewService(provider Provider, normalizer *Normalizer, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{provider: provider, normalizer: normalizer, now: now}
}

// Snapshot fetches markets and normalizes them into a snapshot.
func (s *Service) Snapshot(ctx context.Context, q Query) (*Snapshot, error) {
	markets, err := s.provider.FetchMarkets(ctx, q)
	if err != nil {
		return nil, apperr.Unavailablef("market data provider %s: %v", q.Provider, err)
	}
	return s.normalizer.Normalize(q, markets, s.now())
}

// StaticProvider serves a fixed set of markets.
//
// It backs local development and the deterministic demo seed, and is selected whenever no
// Graph API key is configured. It is a real implementation of the port, not a test double
// hidden behind a build tag, because the offline demo path has to work the same way the
// live one does.
type StaticProvider struct {
	markets []Market
}

// NewStaticProvider returns a provider serving a copy of markets.
func NewStaticProvider(markets ...Market) *StaticProvider {
	return &StaticProvider{markets: append([]Market(nil), markets...)}
}

// FetchMarkets returns the configured markets whose asset matches the query. A query for
// an asset the provider does not carry returns no markets, which normalization then
// reports as a dependency failure.
func (p *StaticProvider) FetchMarkets(_ context.Context, q Query) ([]Market, error) {
	out := make([]Market, 0, len(p.markets))
	for _, m := range p.markets {
		if q.Asset == "" || m.Asset == q.Asset {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DemoMarkets is the synthetic market set used when no live gateway is configured. The
// values are plausible short-duration lending yields, not real market data.
func DemoMarkets() []Market {
	return []Market{
		{
			ID: "usdc-core", SubgraphID: "demo-lending-v1", Asset: "USDC",
			NetSupplyAPY:       money.MustParseRate("0.0585"),
			AvailableLiquidity: money.MustParse("18500000.00", money.USD),
			BlockNumber:        21_450_100,
		},
		{
			ID: "usdc-prime", SubgraphID: "demo-lending-v1", Asset: "USDC",
			NetSupplyAPY:       money.MustParseRate("0.0642"),
			AvailableLiquidity: money.MustParse("12250000.00", money.USD),
			BlockNumber:        21_450_100,
		},
		{
			ID: "usdc-vault", SubgraphID: "demo-yield-v1", Asset: "USDC",
			NetSupplyAPY:       money.MustParseRate("0.0710"),
			AvailableLiquidity: money.MustParse("7400000.00", money.USD),
			BlockNumber:        21_450_098,
		},
		{
			ID: "usdc-longtail", SubgraphID: "demo-yield-v1", Asset: "USDC",
			NetSupplyAPY:       money.MustParseRate("0.1980"),
			AvailableLiquidity: money.MustParse("120000.00", money.USD),
			BlockNumber:        21_450_097,
		},
		{
			ID: "usdc-drained", SubgraphID: "demo-yield-v1", Asset: "USDC",
			NetSupplyAPY:       money.MustParseRate("0.0350"),
			AvailableLiquidity: money.Zero(money.USD),
			BlockNumber:        21_450_099,
		},
	}
}

// DemoQuery is the query the static provider answers in the offline demo.
func DemoQuery() Query {
	return Query{
		Provider: "static-demo",
		Network:  "demo",
		Asset:    "USDC",
		GraphQL: `query Markets($asset: String!) {
			markets(where: { inputToken_: { symbol: $asset } }) {
				id
				rates { side type rate }
				totalDepositBalanceUSD
			}
		}`,
		Variables: map[string]string{"asset": "USDC"},
	}
}
