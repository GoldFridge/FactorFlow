package organization

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/postgres"
)

// Repository stores organizations.
//
// The interface lives beside its only implementation on purpose: a domain service depends
// on this port, not on PostgreSQL, and a test can substitute it without a database.
type Repository interface {
	Create(ctx context.Context, q postgres.Querier, org *Organization) error
	Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Organization, error)
	GetByWallet(ctx context.Context, q postgres.Querier, wallet string) (*Organization, error)
	Update(ctx context.Context, q postgres.Querier, org *Organization, expectedVersion int64) error
	List(ctx context.Context, q postgres.Querier, orgType Type, limit int) ([]*Organization, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const organizationColumns = `id, type, name, wallet, eligibility, reason, version, created_at, updated_at`

// Create inserts a new organization.
func (r *PostgresRepository) Create(ctx context.Context, q postgres.Querier, org *Organization) error {
	const query = `
		INSERT INTO organizations (id, type, name, wallet, eligibility, reason, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	_, err := q.Exec(ctx, query,
		org.ID, org.Type.String(), org.Name, org.Wallet, org.Eligibility.String(), org.Reason,
		org.Version, org.CreatedAt, org.UpdatedAt)
	return postgres.Translate(err)
}

// Get returns one organization by id.
func (r *PostgresRepository) Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Organization, error) {
	const query = `SELECT ` + organizationColumns + ` FROM organizations WHERE id = $1`

	org, err := scanOrganization(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("organization %s", id)
		}
		return nil, postgres.Translate(err)
	}
	return org, nil
}

// GetByWallet returns the organization a wallet acts for.
func (r *PostgresRepository) GetByWallet(ctx context.Context, q postgres.Querier, wallet string) (*Organization, error) {
	const query = `SELECT ` + organizationColumns + ` FROM organizations WHERE lower(wallet) = lower($1)`

	org, err := scanOrganization(q.QueryRow(ctx, query, wallet))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("organization for wallet %s", wallet)
		}
		return nil, postgres.Translate(err)
	}
	return org, nil
}

// Update writes a changed organization, refusing the write when another writer moved first.
//
// The version check is the whole point: two operators deciding eligibility at once must not
// silently overwrite each other, and the loser is told to reload rather than being told the
// write succeeded.
func (r *PostgresRepository) Update(ctx context.Context, q postgres.Querier, org *Organization, expectedVersion int64) error {
	const query = `
		UPDATE organizations
		   SET type = $2, name = $3, wallet = $4, eligibility = $5, reason = $6,
		       version = $7, updated_at = $8
		 WHERE id = $1 AND version = $9`

	tag, err := q.Exec(ctx, query,
		org.ID, org.Type.String(), org.Name, org.Wallet, org.Eligibility.String(), org.Reason,
		org.Version, org.UpdatedAt, expectedVersion)
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflictf("organization %s was modified by another writer", org.ID)
	}
	return nil
}

// List returns organizations of one type, newest first.
func (r *PostgresRepository) List(ctx context.Context, q postgres.Querier, orgType Type, limit int) ([]*Organization, error) {
	const query = `
		SELECT ` + organizationColumns + `
		  FROM organizations
		 WHERE ($1 = '' OR type = $1)
		 ORDER BY created_at DESC, id
		 LIMIT $2`

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := q.Query(ctx, query, orgType.String(), limit)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []*Organization
	for rows.Next() {
		org, err := scanOrganization(rows)
		if err != nil {
			return nil, postgres.Translate(err)
		}
		out = append(out, org)
	}
	return out, postgres.Translate(rows.Err())
}

// row is the shared shape of pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

func scanOrganization(r row) (*Organization, error) {
	var (
		org         Organization
		orgType     string
		eligibility string
		createdAt   time.Time
		updatedAt   time.Time
	)

	if err := r.Scan(&org.ID, &orgType, &org.Name, &org.Wallet, &eligibility, &org.Reason,
		&org.Version, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	parsedType, err := ParseType(orgType)
	if err != nil {
		return nil, err
	}
	parsedEligibility, err := ParseEligibility(eligibility)
	if err != nil {
		return nil, err
	}

	org.Type = parsedType
	org.Eligibility = parsedEligibility
	org.CreatedAt = createdAt.UTC()
	org.UpdatedAt = updatedAt.UTC()
	return &org, nil
}
