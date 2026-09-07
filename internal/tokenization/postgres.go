package tokenization

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

// Repository stores tokenized assets.
type Repository interface {
	Create(ctx context.Context, q postgres.Querier, asset *Asset) error
	Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Asset, error)
	GetByInvoice(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (*Asset, error)
	Update(ctx context.Context, q postgres.Querier, asset *Asset) error
	ListByIssuer(ctx context.Context, q postgres.Querier, issuerID uuid.UUID, limit int) ([]*Asset, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const assetColumns = `
	id, invoice_id, issuer_id, network, token_id, contract_id, supply_minor, currency,
	chain_status, transaction_id, explorer_url, created_at, updated_at`

// Create inserts a newly issued asset.
func (r *PostgresRepository) Create(ctx context.Context, q postgres.Querier, asset *Asset) error {
	const query = `
		INSERT INTO tokenized_assets (` + assetColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

	_, err := q.Exec(ctx, query,
		asset.ID, asset.InvoiceID, asset.IssuerID, asset.Network, asset.TokenID, asset.ContractID,
		asset.Supply.Minor(), asset.Supply.Currency().String(), asset.ChainStatus.String(),
		asset.TransactionID, asset.ExplorerURL, asset.CreatedAt, asset.UpdatedAt)
	return postgres.Translate(err)
}

// Get returns one asset.
func (r *PostgresRepository) Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Asset, error) {
	const query = `SELECT ` + assetColumns + ` FROM tokenized_assets WHERE id = $1`

	asset, err := scanAsset(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("tokenized asset %s", id)
		}
		return nil, postgres.Translate(err)
	}
	return asset, nil
}

// GetByInvoice returns the asset issued for an invoice.
//
// The issuance handler calls this before minting: an invoice has at most one asset, and a
// redelivered command must find the existing one rather than create a second.
func (r *PostgresRepository) GetByInvoice(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (*Asset, error) {
	const query = `SELECT ` + assetColumns + ` FROM tokenized_assets WHERE invoice_id = $1`

	asset, err := scanAsset(q.QueryRow(ctx, query, invoiceID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("tokenized asset for invoice %s", invoiceID)
		}
		return nil, postgres.Translate(err)
	}
	return asset, nil
}

// Update writes a changed asset.
//
// There is no version column here, and deliberately so: the chain is the source of truth
// for this record, and the local row follows it. Concurrency is handled by the settlement
// saga's operation ids, not by an optimistic version on a mirror of chain state.
func (r *PostgresRepository) Update(ctx context.Context, q postgres.Querier, asset *Asset) error {
	const query = `
		UPDATE tokenized_assets
		   SET chain_status = $2, transaction_id = $3, explorer_url = $4, updated_at = $5
		 WHERE id = $1`

	tag, err := q.Exec(ctx, query,
		asset.ID, asset.ChainStatus.String(), asset.TransactionID, asset.ExplorerURL, asset.UpdatedAt)
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFoundf("tokenized asset %s", asset.ID)
	}
	return nil
}

// ListByIssuer returns an issuer's assets, newest first.
func (r *PostgresRepository) ListByIssuer(ctx context.Context, q postgres.Querier, issuerID uuid.UUID, limit int) ([]*Asset, error) {
	const query = `
		SELECT ` + assetColumns + `
		  FROM tokenized_assets
		 WHERE issuer_id = $1
		 ORDER BY created_at DESC, id
		 LIMIT $2`

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := q.Query(ctx, query, issuerID, limit)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []*Asset
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			return nil, postgres.Translate(err)
		}
		out = append(out, asset)
	}
	return out, postgres.Translate(rows.Err())
}

// row is the shared shape of pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

func scanAsset(r row) (*Asset, error) {
	var (
		asset       Asset
		supplyMinor int64
		currency    string
		status      string
		createdAt   time.Time
		updatedAt   time.Time
	)

	if err := r.Scan(&asset.ID, &asset.InvoiceID, &asset.IssuerID, &asset.Network, &asset.TokenID,
		&asset.ContractID, &supplyMinor, &currency, &status, &asset.TransactionID,
		&asset.ExplorerURL, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	parsedCurrency, err := money.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	if asset.Supply, err = money.New(supplyMinor, parsedCurrency); err != nil {
		return nil, err
	}
	if asset.ChainStatus, err = ParseChainStatus(status); err != nil {
		return nil, err
	}

	asset.CreatedAt = createdAt.UTC()
	asset.UpdatedAt = updatedAt.UTC()
	return &asset, nil
}
