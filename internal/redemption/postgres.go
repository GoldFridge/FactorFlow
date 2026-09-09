package redemption

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Repository stores repayments.
//
// There is no update: a repayment is a record of something that happened outside this
// system, and a payment that turns out to have been wrong is corrected by recording the
// correction, not by editing history.
type Repository interface {
	Create(ctx context.Context, q postgres.Querier, r *Repayment) error
	GetByInvoice(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (*Repayment, error)
	ListForParty(ctx context.Context, q postgres.Querier, partyID uuid.UUID, limit int) ([]*Repayment, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const repaymentColumns = `
	id, invoice_id, face_minor, amount_minor, currency, reference, received_at,
	recorded_by, created_at`

// Create inserts a repayment and its shares.
//
// Both writes are in the caller's transaction, because a repayment without its division is
// a payment nobody can be paid from, and a division without its repayment is money owed by
// nothing.
func (r *PostgresRepository) Create(ctx context.Context, q postgres.Querier, rep *Repayment) error {
	const query = `
		INSERT INTO repayments (` + repaymentColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	_, err := q.Exec(ctx, query,
		rep.ID, rep.InvoiceID, rep.Face.Minor(), rep.Amount.Minor(),
		rep.Amount.Currency().String(), rep.Reference, rep.ReceivedAt,
		rep.RecordedBy, rep.CreatedAt)
	if err != nil {
		return postgres.Translate(err)
	}

	const shareQuery = `
		INSERT INTO repayment_shares (repayment_id, party_id, notional_minor, amount_minor, currency)
		VALUES ($1, $2, $3, $4, $5)`

	for _, share := range rep.Shares {
		_, err := q.Exec(ctx, shareQuery, rep.ID, share.PartyID,
			share.Notional.Minor(), share.Amount.Minor(), share.Amount.Currency().String())
		if err != nil {
			return postgres.Translate(err)
		}
	}
	return nil
}

// GetByInvoice returns the repayment of one receivable.
func (r *PostgresRepository) GetByInvoice(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (*Repayment, error) {
	const query = `SELECT ` + repaymentColumns + ` FROM repayments WHERE invoice_id = $1`

	rep, err := scanRepayment(q.QueryRow(ctx, query, invoiceID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("repayment of invoice %s", invoiceID)
		}
		return nil, postgres.Translate(err)
	}

	if err := r.loadShares(ctx, q, rep); err != nil {
		return nil, err
	}
	return rep, nil
}

// ListForParty returns the repayments a party has a share in, newest first.
//
// This is an investor's own record of what came back, which is why it is keyed by the
// share rather than by the receivable: the holder is entitled to the payments it was part
// of and to nothing else.
func (r *PostgresRepository) ListForParty(ctx context.Context, q postgres.Querier, partyID uuid.UUID, limit int) ([]*Repayment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	const query = `
		SELECT r.id, r.invoice_id, r.face_minor, r.amount_minor, r.currency, r.reference,
		       r.received_at, r.recorded_by, r.created_at
		  FROM repayments r
		  JOIN repayment_shares s ON s.repayment_id = r.id
		 WHERE s.party_id = $1
		 ORDER BY r.received_at DESC, r.id
		 LIMIT $2`

	rows, err := q.Query(ctx, query, partyID, limit)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []*Repayment
	for rows.Next() {
		rep, err := scanRepayment(rows)
		if err != nil {
			return nil, postgres.Translate(err)
		}
		out = append(out, rep)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Translate(err)
	}

	for _, rep := range out {
		if err := r.loadShares(ctx, q, rep); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// loadShares fills in the division of a repayment.
func (r *PostgresRepository) loadShares(ctx context.Context, q postgres.Querier, rep *Repayment) error {
	const query = `
		SELECT party_id, notional_minor, amount_minor, currency
		  FROM repayment_shares
		 WHERE repayment_id = $1
		 ORDER BY amount_minor DESC, party_id`

	rows, err := q.Query(ctx, query, rep.ID)
	if err != nil {
		return postgres.Translate(err)
	}
	defer rows.Close()

	rep.Shares = nil
	for rows.Next() {
		var (
			partyID  uuid.UUID
			notional int64
			amount   int64
			code     string
		)
		if err := rows.Scan(&partyID, &notional, &amount, &code); err != nil {
			return postgres.Translate(err)
		}

		currency, err := money.ParseCurrency(code)
		if err != nil {
			return err
		}
		held, err := money.New(notional, currency)
		if err != nil {
			return err
		}
		part, err := money.New(amount, currency)
		if err != nil {
			return err
		}
		rep.Shares = append(rep.Shares, Share{PartyID: partyID, Notional: held, Amount: part})
	}
	return postgres.Translate(rows.Err())
}

func scanRepayment(row pgx.Row) (*Repayment, error) {
	var (
		rep    Repayment
		face   int64
		amount int64
		code   string
	)

	if err := row.Scan(&rep.ID, &rep.InvoiceID, &face, &amount, &code, &rep.Reference,
		&rep.ReceivedAt, &rep.RecordedBy, &rep.CreatedAt); err != nil {
		return nil, err
	}

	currency, err := money.ParseCurrency(code)
	if err != nil {
		return nil, err
	}
	if rep.Face, err = money.New(face, currency); err != nil {
		return nil, err
	}
	if rep.Amount, err = money.New(amount, currency); err != nil {
		return nil, err
	}

	rep.ReceivedAt = rep.ReceivedAt.UTC()
	rep.CreatedAt = rep.CreatedAt.UTC()
	return &rep, nil
}
