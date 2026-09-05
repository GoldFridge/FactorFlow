// Package marketdata turns live standardized lending data into the pricing inputs the risk
// model consumes: a benchmark APR, a liquidity premium and a volatility proxy.
//
// Two rules from the specification shape this package. First, the benchmark is computed by
// Go code from a captured GraphQL response, so a language model cannot quietly move a
// price. Second, a snapshot older than its TTL is not a benchmark: pricing fails closed
// with DATA_STALE rather than using yesterday's market.
package marketdata

import (
	"errors"
	"strings"
	"time"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
)

// DefaultTTL is the demo freshness window for a market snapshot.
const DefaultTTL = 15 * time.Minute

// Market is one standardized lending market as returned by a subgraph.
type Market struct {
	// ID is the market identifier inside the subgraph, used for deterministic ordering.
	ID string
	// SubgraphID identifies the source subgraph, recorded in the snapshot as provenance.
	SubgraphID string
	// Asset is the underlying asset symbol, for display and filtering.
	Asset string
	// NetSupplyAPY is the yield a supplier earns, net of fees.
	NetSupplyAPY money.Rate
	// AvailableLiquidity is the liquidity that can currently be withdrawn.
	AvailableLiquidity money.Amount
	// BlockNumber is the block the values were indexed at.
	BlockNumber int64
}

// Query identifies the request that produced a set of markets. It is hashed into the
// snapshot so a price can be traced back to the exact question that was asked.
type Query struct {
	// Provider names the data source, for example "thegraph-gateway".
	Provider string
	// Network is the chain the markets live on.
	Network string
	// Asset is the asset the benchmark is measured in.
	Asset string
	// GraphQL is the query text sent to the gateway.
	GraphQL string
	// Variables are the query variables, in canonical key=value form.
	Variables map[string]string
}

// Snapshot is an immutable, hashable record of one market observation and everything
// derived from it.
type Snapshot struct {
	Provider string
	Network  string
	Asset    string

	// QueryHash binds the snapshot to the request that produced it.
	QueryHash string
	// PayloadHash binds it to the exact market rows that were returned.
	PayloadHash string

	// Markets are the eligible rows after filtering, in canonical order.
	Markets []Market
	// SubgraphIDs and BlockNumbers record provenance for the audit timeline.
	SubgraphIDs  []string
	BlockNumbers []int64

	// Benchmark is the liquidity-weighted median net supply APY.
	Benchmark money.Rate
	// LiquidityPremium is derived from the total eligible liquidity.
	LiquidityPremium money.Rate
	// Volatility is the dispersion proxy the risk model consumes as a feature.
	Volatility money.Rate
	// TotalLiquidity is the sum of eligible liquidity behind the benchmark.
	TotalLiquidity money.Amount

	ObservedAt time.Time
	TTL        time.Duration
}

// Validate checks the facts of a market row.
func (m Market) Validate() error {
	var violations []error

	if strings.TrimSpace(m.ID) == "" {
		violations = append(violations, apperr.Invalid("market.id", "must not be empty"))
	}
	if strings.TrimSpace(m.SubgraphID) == "" {
		violations = append(violations, apperr.Invalid("market.subgraph_id", "must not be empty"))
	}
	if m.NetSupplyAPY.IsNegative() {
		violations = append(violations, apperr.Invalid("market.net_supply_apy", "must not be negative, got %s", m.NetSupplyAPY))
	}
	if !m.AvailableLiquidity.IsValid() {
		violations = append(violations, apperr.Invalid("market.available_liquidity", "must carry a supported currency"))
	} else if m.AvailableLiquidity.IsNegative() {
		violations = append(violations, apperr.Invalid("market.available_liquidity", "must not be negative"))
	}
	if m.BlockNumber < 0 {
		violations = append(violations, apperr.Invalid("market.block_number", "must not be negative"))
	}

	return errors.Join(violations...)
}

// ExpiresAt is the instant the snapshot stops being usable for pricing.
func (s *Snapshot) ExpiresAt() time.Time { return s.ObservedAt.Add(s.TTL) }

// IsFresh reports whether the snapshot may still be used to price a receivable.
func (s *Snapshot) IsFresh(now time.Time) bool { return !now.After(s.ExpiresAt()) }

// Age is how long ago the snapshot was observed.
func (s *Snapshot) Age(now time.Time) time.Duration { return now.Sub(s.ObservedAt) }

// EnsureFresh fails closed on a stale snapshot. Pricing calls it before using a benchmark:
// the specification refuses to publish a price from market data past its TTL.
func (s *Snapshot) EnsureFresh(now time.Time) error {
	if s.IsFresh(now) {
		return nil
	}
	return apperr.Unavailablef("DATA_STALE: market snapshot %s is %s old, TTL is %s",
		s.PayloadHash, s.Age(now).Round(time.Second), s.TTL)
}
