package marketdata_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

func TestSnapshotFreshness(t *testing.T) {
	t.Parallel()

	snapshot := normalize(t, marketdata.DemoMarkets())

	assert.Equal(t, observedAt.Add(marketdata.DefaultTTL), snapshot.ExpiresAt())
	assert.True(t, snapshot.IsFresh(observedAt))
	assert.True(t, snapshot.IsFresh(observedAt.Add(14*time.Minute)))
	assert.True(t, snapshot.IsFresh(snapshot.ExpiresAt()), "the TTL bound is inclusive")
	assert.False(t, snapshot.IsFresh(snapshot.ExpiresAt().Add(time.Second)))
	assert.Equal(t, 5*time.Minute, snapshot.Age(observedAt.Add(5*time.Minute)))
}

// TestEnsureFreshFailsClosed is the DATA_STALE rule: past its TTL a snapshot is not a
// benchmark, and pricing must refuse rather than publish a stale price.
func TestEnsureFreshFailsClosed(t *testing.T) {
	t.Parallel()

	snapshot := normalize(t, marketdata.DemoMarkets())

	require.NoError(t, snapshot.EnsureFresh(observedAt.Add(time.Minute)))

	err := snapshot.EnsureFresh(observedAt.Add(20 * time.Minute))
	require.ErrorIs(t, err, apperr.ErrUnavailable)
	assert.Contains(t, err.Error(), "DATA_STALE")
	assert.Contains(t, err.Error(), snapshot.PayloadHash, "the failure names the snapshot it refused")
}

func TestQueryHashIsStableAndSensitive(t *testing.T) {
	t.Parallel()

	base := normalize(t, marketdata.DemoMarkets())

	reindented := marketdata.DemoQuery()
	reindented.GraphQL = strings.Join(strings.Fields(reindented.GraphQL), "\n    ")
	snapshot, err := marketdata.NewNormalizer().Normalize(reindented, marketdata.DemoMarkets(), observedAt)
	require.NoError(t, err)
	assert.Equal(t, base.QueryHash, snapshot.QueryHash, "reindenting a query does not invalidate prices")

	changed := marketdata.DemoQuery()
	changed.Variables = map[string]string{"asset": "DAI"}
	snapshot, err = marketdata.NewNormalizer().Normalize(changed, marketdata.DemoMarkets(), observedAt)
	require.NoError(t, err)
	assert.NotEqual(t, base.QueryHash, snapshot.QueryHash, "a different variable is a different question")
}

func TestPayloadHashCoversObservationTime(t *testing.T) {
	t.Parallel()

	first := normalize(t, marketdata.DemoMarkets())

	later, err := marketdata.NewNormalizer().Normalize(
		marketdata.DemoQuery(), marketdata.DemoMarkets(), observedAt.Add(time.Minute))
	require.NoError(t, err)

	assert.NotEqual(t, first.PayloadHash, later.PayloadHash,
		"two observations of the same values are still two observations")
}

func TestServiceProducesSnapshotsFromAProvider(t *testing.T) {
	t.Parallel()

	service := marketdata.NewService(
		marketdata.NewStaticProvider(marketdata.DemoMarkets()...),
		marketdata.NewNormalizer(),
		func() time.Time { return observedAt },
	)

	snapshot, err := service.Snapshot(context.Background(), marketdata.DemoQuery())
	require.NoError(t, err)
	assert.Equal(t, "0.064200", snapshot.Benchmark.StringFixed(6))
	assert.Equal(t, observedAt, snapshot.ObservedAt)
}

func TestServiceReportsProviderFailureAsUnavailable(t *testing.T) {
	t.Parallel()

	service := marketdata.NewService(
		providerFunc(func(context.Context, marketdata.Query) ([]marketdata.Market, error) {
			return nil, errors.New("gateway timeout")
		}),
		marketdata.NewNormalizer(),
		func() time.Time { return observedAt },
	)

	_, err := service.Snapshot(context.Background(), marketdata.DemoQuery())
	require.ErrorIs(t, err, apperr.ErrUnavailable)
	assert.Contains(t, err.Error(), "gateway timeout")
}

func TestStaticProviderFiltersByAsset(t *testing.T) {
	t.Parallel()

	provider := marketdata.NewStaticProvider(marketdata.DemoMarkets()...)

	usdc, err := provider.FetchMarkets(context.Background(), marketdata.DemoQuery())
	require.NoError(t, err)
	assert.Len(t, usdc, len(marketdata.DemoMarkets()))

	otherAsset := marketdata.DemoQuery()
	otherAsset.Asset = "DAI"
	none, err := provider.FetchMarkets(context.Background(), otherAsset)
	require.NoError(t, err)
	assert.Empty(t, none)

	_, err = marketdata.NewService(provider, marketdata.NewNormalizer(), func() time.Time { return observedAt }).
		Snapshot(context.Background(), otherAsset)
	require.ErrorIs(t, err, apperr.ErrUnavailable, "an unknown asset is a failure, not a free benchmark")
}

// providerFunc adapts a function to the Provider port.
type providerFunc func(context.Context, marketdata.Query) ([]marketdata.Market, error)

func (f providerFunc) FetchMarkets(ctx context.Context, q marketdata.Query) ([]marketdata.Market, error) {
	return f(ctx, q)
}
