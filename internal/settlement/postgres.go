package settlement

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Repository stores settlements.
type Repository interface {
	Create(ctx context.Context, q postgres.Querier, s *Settlement) error
	Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Settlement, error)
	GetByOperation(ctx context.Context, q postgres.Querier, operationID string) (*Settlement, error)
	Update(ctx context.Context, q postgres.Querier, s *Settlement, expectedVersion int64) error
	ListByAuction(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) ([]*Settlement, error)
	ListUnfinished(ctx context.Context, q postgres.Querier, limit int) ([]*Settlement, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const settlementColumns = `
	id, auction_id, lot_id, bid_id, invoice_id, asset_id, investor_id, from_wallet, to_wallet,
	notional_minor, price_minor, currency, operation_id, tx_id, state, attempts, last_error,
	version, created_at, updated_at`

// Create inserts a planned settlement.
func (r *PostgresRepository) Create(ctx context.Context, q postgres.Querier, s *Settlement) error {
	const query = `
		INSERT INTO settlements (` + settlementColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`

	_, err := q.Exec(ctx, query,
		s.ID, s.AuctionID, s.LotID, s.BidID, s.InvoiceID, s.AssetID, s.InvestorID,
		s.FromWallet, s.ToWallet, s.Notional.Minor(), s.Price.Minor(),
		s.Notional.Currency().String(), s.OperationID, s.TxID, s.State.String(),
		s.Attempts, s.LastError, s.Version, s.CreatedAt, s.UpdatedAt)
	return postgres.Translate(err)
}

// Get returns one settlement.
func (r *PostgresRepository) Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Settlement, error) {
	const query = `SELECT ` + settlementColumns + ` FROM settlements WHERE id = $1`

	s, err := scanSettlement(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("settlement %s", id)
		}
		return nil, postgres.Translate(err)
	}
	return s, nil
}

// GetByOperation returns the settlement behind a deterministic operation id.
//
// This is how a saga step that is unsure whether it already ran finds out, without having
// to trust anything it remembers.
func (r *PostgresRepository) GetByOperation(ctx context.Context, q postgres.Querier, operationID string) (*Settlement, error) {
	const query = `SELECT ` + settlementColumns + ` FROM settlements WHERE operation_id = $1`

	s, err := scanSettlement(q.QueryRow(ctx, query, operationID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("settlement for operation %s", operationID)
		}
		return nil, postgres.Translate(err)
	}
	return s, nil
}

// Update stores a settlement under the version it was read at.
//
// Optimistic concurrency matters more here than elsewhere: two workers advancing the same
// saga would otherwise each believe they were the one that submitted the transfer.
func (r *PostgresRepository) Update(ctx context.Context, q postgres.Querier, s *Settlement, expectedVersion int64) error {
	const query = `
		UPDATE settlements
		   SET tx_id = $3, state = $4, attempts = $5, last_error = $6, version = $7, updated_at = $8
		 WHERE id = $1 AND version = $2`

	tag, err := q.Exec(ctx, query,
		s.ID, expectedVersion, s.TxID, s.State.String(), s.Attempts, s.LastError, s.Version, s.UpdatedAt)
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflictf("settlement %s was modified concurrently", s.ID)
	}
	return nil
}

// ListByAuction returns a batch's settlements in the order they were planned.
func (r *PostgresRepository) ListByAuction(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) ([]*Settlement, error) {
	const query = `
		SELECT ` + settlementColumns + `
		  FROM settlements
		 WHERE auction_id = $1
		 ORDER BY created_at, id`

	return querySettlements(ctx, q, query, auctionID)
}

// ListUnfinished returns settlements that still need work, oldest first.
//
// It is the reconciliation worker's queue: a saga interrupted by a crash is found here
// rather than being lost with the process that was running it.
func (r *PostgresRepository) ListUnfinished(ctx context.Context, q postgres.Querier, limit int) ([]*Settlement, error) {
	if limit <= 0 {
		limit = 100
	}

	const query = `
		SELECT ` + settlementColumns + `
		  FROM settlements
		 WHERE state <> 'ACCOUNTED'
		 ORDER BY updated_at, id
		 LIMIT $1`

	return querySettlements(ctx, q, query, limit)
}

func querySettlements(ctx context.Context, q postgres.Querier, query string, args ...any) ([]*Settlement, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []*Settlement
	for rows.Next() {
		s, err := scanSettlement(rows)
		if err != nil {
			return nil, postgres.Translate(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Translate(err)
	}
	return out, nil
}

// row is the shared shape of pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

func scanSettlement(r row) (*Settlement, error) {
	var (
		s             Settlement
		notionalMinor int64
		priceMinor    int64
		currency      string
		state         string
		createdAt     time.Time
		updatedAt     time.Time
	)

	if err := r.Scan(&s.ID, &s.AuctionID, &s.LotID, &s.BidID, &s.InvoiceID, &s.AssetID,
		&s.InvestorID, &s.FromWallet, &s.ToWallet, &notionalMinor, &priceMinor, &currency,
		&s.OperationID, &s.TxID, &state, &s.Attempts, &s.LastError, &s.Version,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}

	parsedCurrency, err := money.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	if s.Notional, err = money.New(notionalMinor, parsedCurrency); err != nil {
		return nil, err
	}
	if s.Price, err = money.New(priceMinor, parsedCurrency); err != nil {
		return nil, err
	}
	if s.State, err = ParseState(state); err != nil {
		return nil, err
	}

	s.CreatedAt = createdAt.UTC()
	s.UpdatedAt = updatedAt.UTC()
	return &s, nil
}
