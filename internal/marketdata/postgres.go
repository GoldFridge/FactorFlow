package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Repository stores market snapshots.
//
// A snapshot is stored, not just used, because a published price has to be explainable
// later: the audit view shows which markets, at which blocks, produced the benchmark a
// receivable was priced against.
type Repository interface {
	Save(ctx context.Context, q postgres.Querier, snapshot *Snapshot) error
	Get(ctx context.Context, q postgres.Querier, payloadHash string) (*Snapshot, error)
	Latest(ctx context.Context, q postgres.Querier, network, asset string) (*Snapshot, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const snapshotColumns = `
	payload_hash, provider, network, asset, query_hash, benchmark_apr, liquidity_premium,
	volatility, total_liquidity_minor, currency, subgraph_ids, block_numbers, markets,
	observed_at, ttl_seconds`

// marketRow is the JSON shape of one stored market row.
type marketRow struct {
	ID                 string `json:"id"`
	SubgraphID         string `json:"subgraph_id"`
	Asset              string `json:"asset"`
	NetSupplyAPY       string `json:"net_supply_apy"`
	AvailableLiquidity int64  `json:"available_liquidity_minor"`
	Currency           string `json:"currency"`
	BlockNumber        int64  `json:"block_number"`
}

// Save stores a snapshot.
//
// Re-observing the same markets at the same instant yields the same payload hash, so the
// insert is idempotent: a retried worker records one snapshot, not two.
func (r *PostgresRepository) Save(ctx context.Context, q postgres.Querier, snapshot *Snapshot) error {
	rows := make([]marketRow, 0, len(snapshot.Markets))
	for _, m := range snapshot.Markets {
		rows = append(rows, marketRow{
			ID:                 m.ID,
			SubgraphID:         m.SubgraphID,
			Asset:              m.Asset,
			NetSupplyAPY:       m.NetSupplyAPY.StringFixed(rateScale),
			AvailableLiquidity: m.AvailableLiquidity.Minor(),
			Currency:           m.AvailableLiquidity.Currency().String(),
			BlockNumber:        m.BlockNumber,
		})
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return err
	}

	const query = `
		INSERT INTO market_snapshots (` + snapshotColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (payload_hash) DO NOTHING`

	_, err = q.Exec(ctx, query,
		snapshot.PayloadHash, snapshot.Provider, snapshot.Network, snapshot.Asset, snapshot.QueryHash,
		snapshot.Benchmark.Decimal(), snapshot.LiquidityPremium.Decimal(), snapshot.Volatility.Decimal(),
		snapshot.TotalLiquidity.Minor(), snapshot.TotalLiquidity.Currency().String(),
		snapshot.SubgraphIDs, snapshot.BlockNumbers, encoded,
		snapshot.ObservedAt, int32(snapshot.TTL.Seconds()))
	return postgres.Translate(err)
}

// Get returns one snapshot by its payload hash, which is how a stored assessment points at
// the market data it was priced from.
func (r *PostgresRepository) Get(ctx context.Context, q postgres.Querier, payloadHash string) (*Snapshot, error) {
	const query = `SELECT ` + snapshotColumns + ` FROM market_snapshots WHERE payload_hash = $1`

	snapshot, err := scanSnapshot(q.QueryRow(ctx, query, payloadHash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("market snapshot %s", payloadHash)
		}
		return nil, postgres.Translate(err)
	}
	return snapshot, nil
}

// Latest returns the most recent snapshot for a market, which the pricing path reuses
// while it is still fresh instead of querying the gateway for every invoice.
func (r *PostgresRepository) Latest(ctx context.Context, q postgres.Querier, network, asset string) (*Snapshot, error) {
	const query = `
		SELECT ` + snapshotColumns + `
		  FROM market_snapshots
		 WHERE network = $1 AND asset = $2
		 ORDER BY observed_at DESC
		 LIMIT 1`

	snapshot, err := scanSnapshot(q.QueryRow(ctx, query, network, asset))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("market snapshot for %s/%s", network, asset)
		}
		return nil, postgres.Translate(err)
	}
	return snapshot, nil
}

// row is the shared shape of pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

func scanSnapshot(r row) (*Snapshot, error) {
	var (
		snapshot   Snapshot
		benchmark  decimal.Decimal
		liquidity  decimal.Decimal
		volatility decimal.Decimal
		totalMinor int64
		currency   string
		markets    []byte
		observedAt time.Time
		ttlSeconds int32
	)

	if err := r.Scan(&snapshot.PayloadHash, &snapshot.Provider, &snapshot.Network, &snapshot.Asset,
		&snapshot.QueryHash, &benchmark, &liquidity, &volatility, &totalMinor, &currency,
		&snapshot.SubgraphIDs, &snapshot.BlockNumbers, &markets, &observedAt, &ttlSeconds); err != nil {
		return nil, err
	}

	parsedCurrency, err := money.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	total, err := money.New(totalMinor, parsedCurrency)
	if err != nil {
		return nil, err
	}

	var rows []marketRow
	if err := json.Unmarshal(markets, &rows); err != nil {
		return nil, err
	}
	for _, stored := range rows {
		apy, err := money.ParseRate(stored.NetSupplyAPY)
		if err != nil {
			return nil, err
		}
		marketCurrency, err := money.ParseCurrency(stored.Currency)
		if err != nil {
			return nil, err
		}
		available, err := money.New(stored.AvailableLiquidity, marketCurrency)
		if err != nil {
			return nil, err
		}
		snapshot.Markets = append(snapshot.Markets, Market{
			ID:                 stored.ID,
			SubgraphID:         stored.SubgraphID,
			Asset:              stored.Asset,
			NetSupplyAPY:       apy,
			AvailableLiquidity: available,
			BlockNumber:        stored.BlockNumber,
		})
	}

	snapshot.Benchmark = money.NewRate(benchmark)
	snapshot.LiquidityPremium = money.NewRate(liquidity)
	snapshot.Volatility = money.NewRate(volatility)
	snapshot.TotalLiquidity = total
	snapshot.ObservedAt = observedAt.UTC()
	snapshot.TTL = time.Duration(ttlSeconds) * time.Second
	return &snapshot, nil
}
