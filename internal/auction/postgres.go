package auction

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// Repository stores auctions, their lots and bids, and the result of a clearing.
type Repository interface {
	CreateAuction(ctx context.Context, q postgres.Querier, a *Auction) error
	GetAuction(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Auction, error)
	UpdateAuction(ctx context.Context, q postgres.Querier, a *Auction, expectedVersion int64) error
	ListAuctions(ctx context.Context, q postgres.Querier, status Status, limit int) ([]*Auction, error)

	CreateBid(ctx context.Context, q postgres.Querier, b *Bid) error
	GetBid(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Bid, error)
	UpdateBid(ctx context.Context, q postgres.Querier, b *Bid, expectedVersion int64) error
	ListBids(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) ([]*Bid, error)

	SaveSolution(ctx context.Context, q postgres.Querier, s *Solution, now time.Time) error
	GetSolution(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) (*Solution, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const auctionColumns = `
	id, issuer_id, status, solver_version, certificate_hash, reason, opens_at, closes_at,
	version, created_at, updated_at`

const lotColumns = `
	id, auction_id, invoice_id, asset_id, issuer_id, debtor_ref, supply_minor,
	reserve_price_minor, currency, grade, tenor_days`

const bidColumns = `
	id, auction_id, investor_id, budget_minor, currency, min_yield, max_grade,
	max_tenor_days, minimum_lot_minor, max_issuer_share, max_debtor_share, max_grade_share,
	status, version, created_at`

// CreateAuction inserts an auction and its lots.
//
// Both are written here rather than in separate calls because an auction without its lots
// is not a batch, it is a row nobody can clear.
func (r *PostgresRepository) CreateAuction(ctx context.Context, q postgres.Querier, a *Auction) error {
	const auctionQuery = `
		INSERT INTO auctions (` + auctionColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	if _, err := q.Exec(ctx, auctionQuery,
		a.ID, a.IssuerID, a.Status.String(), a.SolverVersion, a.CertificateHash, a.Reason,
		a.OpensAt, a.ClosesAt, a.Version, a.CreatedAt, a.UpdatedAt); err != nil {
		return postgres.Translate(err)
	}

	const lotQuery = `
		INSERT INTO auction_lots (` + lotColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	for _, lot := range a.Lots {
		if _, err := q.Exec(ctx, lotQuery,
			lot.ID, a.ID, lot.InvoiceID, lot.AssetID, lot.IssuerID, lot.DebtorRef,
			lot.Supply.Minor(), lot.ReservePrice.Minor(), lot.Supply.Currency().String(),
			lot.Grade.String(), lot.TenorDays); err != nil {
			return postgres.Translate(err)
		}
	}
	return nil
}

// GetAuction returns an auction with its lots.
func (r *PostgresRepository) GetAuction(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Auction, error) {
	const query = `SELECT ` + auctionColumns + ` FROM auctions WHERE id = $1`

	a, err := scanAuction(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("auction %s", id)
		}
		return nil, postgres.Translate(err)
	}

	lots, err := r.listLots(ctx, q, id)
	if err != nil {
		return nil, err
	}
	a.Lots = lots
	return a, nil
}

// UpdateAuction writes a moved auction, refusing the write when another writer moved first.
func (r *PostgresRepository) UpdateAuction(ctx context.Context, q postgres.Querier, a *Auction, expectedVersion int64) error {
	const query = `
		UPDATE auctions
		   SET status = $2, solver_version = $3, certificate_hash = $4, reason = $5,
		       version = $6, updated_at = $7
		 WHERE id = $1 AND version = $8`

	tag, err := q.Exec(ctx, query,
		a.ID, a.Status.String(), a.SolverVersion, a.CertificateHash, a.Reason,
		a.Version, a.UpdatedAt, expectedVersion)
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflictf("auction %s was modified by another writer", a.ID)
	}
	return nil
}

// ListAuctions returns auctions in one state, soonest to close first, which is the order
// the marketplace shows them in.
func (r *PostgresRepository) ListAuctions(ctx context.Context, q postgres.Querier, status Status, limit int) ([]*Auction, error) {
	const query = `
		SELECT ` + auctionColumns + `
		  FROM auctions
		 WHERE ($1 = '' OR status = $1)
		 ORDER BY closes_at, id
		 LIMIT $2`

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := q.Query(ctx, query, status.String(), limit)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []*Auction
	for rows.Next() {
		a, err := scanAuction(rows)
		if err != nil {
			return nil, postgres.Translate(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Translate(err)
	}

	// Lots are fetched per auction rather than in one join: a listing is short, and a join
	// would have to de-duplicate auction rows for every lot.
	for _, a := range out {
		lots, err := r.listLots(ctx, q, a.ID)
		if err != nil {
			return nil, err
		}
		a.Lots = lots
	}
	return out, nil
}

func (r *PostgresRepository) listLots(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) ([]Lot, error) {
	const query = `
		SELECT ` + lotColumns + `
		  FROM auction_lots
		 WHERE auction_id = $1
		 ORDER BY invoice_id, id`

	rows, err := q.Query(ctx, query, auctionID)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var lots []Lot
	for rows.Next() {
		var (
			lot          Lot
			auction      uuid.UUID
			supplyMinor  int64
			reserveMinor int64
			currency     string
			grade        string
		)
		if err := rows.Scan(&lot.ID, &auction, &lot.InvoiceID, &lot.AssetID, &lot.IssuerID,
			&lot.DebtorRef, &supplyMinor, &reserveMinor, &currency, &grade, &lot.TenorDays); err != nil {
			return nil, postgres.Translate(err)
		}

		parsedCurrency, err := money.ParseCurrency(currency)
		if err != nil {
			return nil, err
		}
		if lot.Supply, err = money.New(supplyMinor, parsedCurrency); err != nil {
			return nil, err
		}
		if lot.ReservePrice, err = money.New(reserveMinor, parsedCurrency); err != nil {
			return nil, err
		}
		if lot.Grade, err = risk.ParseGrade(grade); err != nil {
			return nil, err
		}
		lots = append(lots, lot)
	}
	return lots, postgres.Translate(rows.Err())
}

// CreateBid inserts an investor's bid.
func (r *PostgresRepository) CreateBid(ctx context.Context, q postgres.Querier, b *Bid) error {
	shares, err := json.Marshal(gradeShareMap(b))
	if err != nil {
		return err
	}

	const query = `
		INSERT INTO bids (` + bidColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

	_, err = q.Exec(ctx, query,
		b.ID, b.AuctionID, b.InvestorID, b.Budget.Minor(), b.Budget.Currency().String(),
		b.MinYield.Decimal(), b.MaxGrade.String(), b.MaxTenorDays, b.MinimumLot.Minor(),
		b.MaxIssuerShare.Decimal(), b.MaxDebtorShare.Decimal(), shares,
		b.Status.String(), b.Version, b.CreatedAt)
	return postgres.Translate(err)
}

// GetBid returns one bid.
func (r *PostgresRepository) GetBid(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Bid, error) {
	const query = `SELECT ` + bidColumns + ` FROM bids WHERE id = $1`

	bid, err := scanBid(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("bid %s", id)
		}
		return nil, postgres.Translate(err)
	}
	return bid, nil
}

// UpdateBid writes a changed bid under its expected version.
func (r *PostgresRepository) UpdateBid(ctx context.Context, q postgres.Querier, b *Bid, expectedVersion int64) error {
	const query = `
		UPDATE bids
		   SET status = $2, version = $3
		 WHERE id = $1 AND version = $4`

	tag, err := q.Exec(ctx, query, b.ID, b.Status.String(), b.Version, expectedVersion)
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflictf("bid %s was modified by another writer", b.ID)
	}
	return nil
}

// ListBids returns an auction's bids in the solver's canonical order: creation time, then
// id. Reading them in that order means the database and the solver agree on tie-breaks.
func (r *PostgresRepository) ListBids(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) ([]*Bid, error) {
	const query = `
		SELECT ` + bidColumns + `
		  FROM bids
		 WHERE auction_id = $1
		 ORDER BY created_at, id`

	rows, err := q.Query(ctx, query, auctionID)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []*Bid
	for rows.Next() {
		bid, err := scanBid(rows)
		if err != nil {
			return nil, postgres.Translate(err)
		}
		out = append(out, bid)
	}
	return out, postgres.Translate(rows.Err())
}

// SaveSolution stores a clearing: its allocations, its rejections and its certificate.
//
// Everything is written in the caller's transaction, so a batch is never half-cleared: the
// allocations, the reasons and the certificate that covers them all land together.
func (r *PostgresRepository) SaveSolution(ctx context.Context, q postgres.Querier, s *Solution, now time.Time) error {
	const allocationQuery = `
		INSERT INTO allocations (auction_id, lot_id, bid_id, investor_id, notional_minor, price_minor, currency, rank)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	for _, allocation := range s.Allocations {
		if _, err := q.Exec(ctx, allocationQuery,
			s.AuctionID, allocation.LotID, allocation.BidID, allocation.InvestorID,
			allocation.Notional.Minor(), allocation.Price.Minor(),
			allocation.Notional.Currency().String(), allocation.Rank); err != nil {
			return postgres.Translate(err)
		}
	}

	const rejectionQuery = `
		INSERT INTO allocation_rejections (auction_id, bid_id, lot_id, constraint_name)
		VALUES ($1, $2, $3, $4)`

	for _, rejection := range s.Rejections {
		var lotID any
		if rejection.LotID != uuid.Nil {
			lotID = rejection.LotID
		}
		if _, err := q.Exec(ctx, rejectionQuery,
			s.AuctionID, rejection.BidID, lotID, rejection.Constraint.String()); err != nil {
			return postgres.Translate(err)
		}
	}

	const certificateQuery = `
		INSERT INTO allocation_certificates (
			auction_id, certificate_hash, solver_version, objective, total_notional_minor,
			total_cash_minor, currency, branch_nodes, repair_rounds, limit_reached, verified, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

	_, err := q.Exec(ctx, certificateQuery,
		s.AuctionID, s.CertificateHash, s.SolverVersion, s.Objective,
		s.TotalNotional.Minor(), s.TotalCash.Minor(), s.TotalCash.Currency().String(),
		s.BranchNodes, s.RepairRounds, s.LimitReached, s.Verified, now)
	return postgres.Translate(err)
}

// GetSolution reads back a stored clearing.
func (r *PostgresRepository) GetSolution(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) (*Solution, error) {
	const certificateQuery = `
		SELECT certificate_hash, solver_version, objective, total_notional_minor,
		       total_cash_minor, currency, branch_nodes, repair_rounds, limit_reached, verified
		  FROM allocation_certificates
		 WHERE auction_id = $1`

	var (
		solution      Solution
		currency      string
		notionalMinor int64
		cashMinor     int64
	)
	err := q.QueryRow(ctx, certificateQuery, auctionID).Scan(
		&solution.CertificateHash, &solution.SolverVersion, &solution.Objective,
		&notionalMinor, &cashMinor, &currency, &solution.BranchNodes, &solution.RepairRounds,
		&solution.LimitReached, &solution.Verified)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("clearing for auction %s", auctionID)
		}
		return nil, postgres.Translate(err)
	}

	parsedCurrency, err := money.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	if solution.TotalNotional, err = money.New(notionalMinor, parsedCurrency); err != nil {
		return nil, err
	}
	if solution.TotalCash, err = money.New(cashMinor, parsedCurrency); err != nil {
		return nil, err
	}
	solution.AuctionID = auctionID

	if solution.Allocations, err = r.listAllocations(ctx, q, auctionID); err != nil {
		return nil, err
	}
	if solution.Rejections, err = r.listRejections(ctx, q, auctionID); err != nil {
		return nil, err
	}
	return &solution, nil
}

func (r *PostgresRepository) listAllocations(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) ([]Allocation, error) {
	const query = `
		SELECT a.lot_id, l.invoice_id, l.asset_id, a.bid_id, a.investor_id,
		       a.notional_minor, a.price_minor, a.currency, a.rank
		  FROM allocations a
		  JOIN auction_lots l ON l.id = a.lot_id
		 WHERE a.auction_id = $1
		 ORDER BY a.rank`

	rows, err := q.Query(ctx, query, auctionID)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []Allocation
	for rows.Next() {
		var (
			allocation    Allocation
			notionalMinor int64
			priceMinor    int64
			currency      string
		)
		if err := rows.Scan(&allocation.LotID, &allocation.InvoiceID, &allocation.AssetID,
			&allocation.BidID, &allocation.InvestorID, &notionalMinor, &priceMinor,
			&currency, &allocation.Rank); err != nil {
			return nil, postgres.Translate(err)
		}

		parsedCurrency, err := money.ParseCurrency(currency)
		if err != nil {
			return nil, err
		}
		if allocation.Notional, err = money.New(notionalMinor, parsedCurrency); err != nil {
			return nil, err
		}
		if allocation.Price, err = money.New(priceMinor, parsedCurrency); err != nil {
			return nil, err
		}
		out = append(out, allocation)
	}
	return out, postgres.Translate(rows.Err())
}

func (r *PostgresRepository) listRejections(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) ([]Rejection, error) {
	const query = `
		SELECT bid_id, lot_id, constraint_name
		  FROM allocation_rejections
		 WHERE auction_id = $1
		 ORDER BY bid_id`

	rows, err := q.Query(ctx, query, auctionID)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []Rejection
	for rows.Next() {
		var (
			rejection  Rejection
			lotID      *uuid.UUID
			constraint string
		)
		if err := rows.Scan(&rejection.BidID, &lotID, &constraint); err != nil {
			return nil, postgres.Translate(err)
		}
		if lotID != nil {
			rejection.LotID = *lotID
		}
		rejection.Constraint = Constraint(constraint)
		out = append(out, rejection)
	}
	return out, postgres.Translate(rows.Err())
}

// gradeShareMap renders a bid's per-grade limits for storage.
func gradeShareMap(b *Bid) map[string]string {
	shares := make(map[string]string, len(b.MaxGradeShare))
	for grade, share := range b.MaxGradeShare {
		shares[grade.String()] = share.StringFixed(rateScale)
	}
	return shares
}

// row is the shared shape of pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

func scanAuction(r row) (*Auction, error) {
	var (
		a         Auction
		status    string
		opensAt   time.Time
		closesAt  time.Time
		createdAt time.Time
		updatedAt time.Time
	)

	if err := r.Scan(&a.ID, &a.IssuerID, &status, &a.SolverVersion, &a.CertificateHash,
		&a.Reason, &opensAt, &closesAt, &a.Version, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	parsedStatus, err := ParseStatus(status)
	if err != nil {
		return nil, err
	}

	a.Status = parsedStatus
	a.OpensAt = opensAt.UTC()
	a.ClosesAt = closesAt.UTC()
	a.CreatedAt = createdAt.UTC()
	a.UpdatedAt = updatedAt.UTC()
	return &a, nil
}

func scanBid(r row) (*Bid, error) {
	var (
		bid            Bid
		budgetMinor    int64
		currency       string
		minYield       decimal.Decimal
		maxGrade       string
		minimumLot     int64
		maxIssuerShare decimal.Decimal
		maxDebtorShare decimal.Decimal
		gradeShares    []byte
		status         string
		createdAt      time.Time
	)

	if err := r.Scan(&bid.ID, &bid.AuctionID, &bid.InvestorID, &budgetMinor, &currency,
		&minYield, &maxGrade, &bid.MaxTenorDays, &minimumLot, &maxIssuerShare,
		&maxDebtorShare, &gradeShares, &status, &bid.Version, &createdAt); err != nil {
		return nil, err
	}

	parsedCurrency, err := money.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	if bid.Budget, err = money.New(budgetMinor, parsedCurrency); err != nil {
		return nil, err
	}
	if bid.MinimumLot, err = money.New(minimumLot, parsedCurrency); err != nil {
		return nil, err
	}
	if bid.MaxGrade, err = risk.ParseGrade(maxGrade); err != nil {
		return nil, err
	}

	var stored map[string]string
	if err := json.Unmarshal(gradeShares, &stored); err != nil {
		return nil, err
	}
	bid.MaxGradeShare = make(map[risk.Grade]money.Rate, len(stored))
	for grade, share := range stored {
		parsedGrade, err := risk.ParseGrade(grade)
		if err != nil {
			return nil, err
		}
		rate, err := money.ParseRate(share)
		if err != nil {
			return nil, apperr.Invalid("max_grade_share", "stored value %q is not a decimal", share)
		}
		bid.MaxGradeShare[parsedGrade] = rate
	}

	bid.MinYield = money.NewRate(minYield)
	bid.MaxIssuerShare = money.NewRate(maxIssuerShare)
	bid.MaxDebtorShare = money.NewRate(maxDebtorShare)
	bid.Status = BidStatus(status)
	bid.CreatedAt = createdAt.UTC()
	return &bid, nil
}
