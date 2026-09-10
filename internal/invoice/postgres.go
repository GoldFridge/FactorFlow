package invoice

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

// Repository stores invoices and the metadata of their encrypted documents.
type Repository interface {
	Create(ctx context.Context, q postgres.Querier, inv *Invoice) error
	Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Invoice, error)
	Update(ctx context.Context, q postgres.Querier, inv *Invoice, expectedVersion int64) error
	ListByIssuer(ctx context.Context, q postgres.Querier, issuerID uuid.UUID, limit int) ([]*Invoice, error)
	ListByStatus(ctx context.Context, q postgres.Querier, status Status, limit int) ([]*Invoice, error)

	SaveDocument(ctx context.Context, q postgres.Querier, doc *Document) error
	GetDocument(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (*Document, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const invoiceColumns = `
	id, issuer_id, debtor_ref, number, face_minor, currency, issued_at, due_at,
	status, failed_from, reason, assessment_id, asset_id, version, created_at, updated_at`

// Create inserts a draft invoice.
func (r *PostgresRepository) Create(ctx context.Context, q postgres.Querier, inv *Invoice) error {
	const query = `
		INSERT INTO invoices (
			id, issuer_id, debtor_ref, number, face_minor, currency, issued_at, due_at,
			status, failed_from, reason, assessment_id, asset_id, version, created_at, updated_at,
			fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

	_, err := q.Exec(ctx, query,
		inv.ID, inv.IssuerID, inv.DebtorRef, inv.Number,
		inv.Face.Minor(), inv.Face.Currency().String(), inv.IssuedAt, inv.DueAt,
		inv.Status.String(), inv.FailedFrom.String(), inv.Reason,
		nullableUUID(inv.AssessmentID), nullableUUID(inv.AssetID),
		inv.Version, inv.CreatedAt, inv.UpdatedAt, inv.Fingerprint())
	return postgres.Translate(err)
}

// Get returns one invoice.
func (r *PostgresRepository) Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Invoice, error) {
	const query = `SELECT ` + invoiceColumns + ` FROM invoices WHERE id = $1`

	inv, err := scanInvoice(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("invoice %s", id)
		}
		return nil, postgres.Translate(err)
	}
	return inv, nil
}

// Update writes a moved invoice, refusing the write when another writer moved first.
//
// This is the database half of the specification's rule that a command checks the record's
// version: the domain refuses an illegal transition, and this refuses a stale one.
func (r *PostgresRepository) Update(ctx context.Context, q postgres.Querier, inv *Invoice, expectedVersion int64) error {
	const query = `
		UPDATE invoices
		   SET status = $2, failed_from = $3, reason = $4, assessment_id = $5, asset_id = $6,
		       version = $7, updated_at = $8
		 WHERE id = $1 AND version = $9`

	tag, err := q.Exec(ctx, query,
		inv.ID, inv.Status.String(), inv.FailedFrom.String(), inv.Reason,
		nullableUUID(inv.AssessmentID), nullableUUID(inv.AssetID),
		inv.Version, inv.UpdatedAt, expectedVersion)
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflictf("invoice %s was modified by another writer", inv.ID)
	}
	return nil
}

// ListByIssuer returns an issuer's invoices, newest first.
func (r *PostgresRepository) ListByIssuer(ctx context.Context, q postgres.Querier, issuerID uuid.UUID, limit int) ([]*Invoice, error) {
	const query = `
		SELECT ` + invoiceColumns + `
		  FROM invoices
		 WHERE issuer_id = $1
		 ORDER BY created_at DESC, id
		 LIMIT $2`

	return queryInvoices(ctx, q, query, issuerID, boundedLimit(limit))
}

// ListByStatus returns invoices in one state, oldest first, which is the order a worker
// wants when draining a queue of work.
func (r *PostgresRepository) ListByStatus(ctx context.Context, q postgres.Querier, status Status, limit int) ([]*Invoice, error) {
	const query = `
		SELECT ` + invoiceColumns + `
		  FROM invoices
		 WHERE status = $1
		 ORDER BY created_at, id
		 LIMIT $2`

	return queryInvoices(ctx, q, query, status.String(), boundedLimit(limit))
}

// SaveDocument records the metadata of an encrypted upload.
//
// An invoice has one document: re-uploading replaces the metadata, and the new ciphertext
// hash is what every later step binds to.
func (r *PostgresRepository) SaveDocument(ctx context.Context, q postgres.Querier, doc *Document) error {
	const query = `
		INSERT INTO invoice_documents (invoice_id, object_key, cipher_hash, key_ref, mime, size_bytes, uploaded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (invoice_id) DO UPDATE
		   SET object_key = EXCLUDED.object_key,
		       cipher_hash = EXCLUDED.cipher_hash,
		       key_ref = EXCLUDED.key_ref,
		       mime = EXCLUDED.mime,
		       size_bytes = EXCLUDED.size_bytes,
		       uploaded_at = EXCLUDED.uploaded_at`

	_, err := q.Exec(ctx, query,
		doc.InvoiceID, doc.ObjectKey, doc.CipherHash, doc.KeyRef, doc.MIME, doc.SizeBytes, doc.UploadedAt)
	return postgres.Translate(err)
}

// GetDocument returns the document metadata of an invoice.
func (r *PostgresRepository) GetDocument(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (*Document, error) {
	const query = `
		SELECT invoice_id, object_key, cipher_hash, key_ref, mime, size_bytes, uploaded_at
		  FROM invoice_documents
		 WHERE invoice_id = $1`

	var (
		doc        Document
		uploadedAt time.Time
	)
	err := q.QueryRow(ctx, query, invoiceID).Scan(
		&doc.InvoiceID, &doc.ObjectKey, &doc.CipherHash, &doc.KeyRef, &doc.MIME, &doc.SizeBytes, &uploadedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("document for invoice %s", invoiceID)
		}
		return nil, postgres.Translate(err)
	}
	doc.UploadedAt = uploadedAt.UTC()
	return &doc, nil
}

func queryInvoices(ctx context.Context, q postgres.Querier, query string, args ...any) ([]*Invoice, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var out []*Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, postgres.Translate(err)
		}
		out = append(out, inv)
	}
	return out, postgres.Translate(rows.Err())
}

// row is the shared shape of pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

func scanInvoice(r row) (*Invoice, error) {
	var (
		inv          Invoice
		faceMinor    int64
		currency     string
		status       string
		failedFrom   string
		assessmentID *uuid.UUID
		assetID      *uuid.UUID
		issuedAt     time.Time
		dueAt        time.Time
		createdAt    time.Time
		updatedAt    time.Time
	)

	if err := r.Scan(&inv.ID, &inv.IssuerID, &inv.DebtorRef, &inv.Number, &faceMinor, &currency,
		&issuedAt, &dueAt, &status, &failedFrom, &inv.Reason, &assessmentID, &assetID,
		&inv.Version, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	parsedCurrency, err := money.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	face, err := money.New(faceMinor, parsedCurrency)
	if err != nil {
		return nil, err
	}
	parsedStatus, err := ParseStatus(status)
	if err != nil {
		return nil, err
	}
	if failedFrom != "" {
		parsedFailedFrom, err := ParseStatus(failedFrom)
		if err != nil {
			return nil, err
		}
		inv.FailedFrom = parsedFailedFrom
	}

	inv.Face = face
	inv.Status = parsedStatus
	inv.IssuedAt = issuedAt.UTC()
	inv.DueAt = dueAt.UTC()
	inv.CreatedAt = createdAt.UTC()
	inv.UpdatedAt = updatedAt.UTC()
	if assessmentID != nil {
		inv.AssessmentID = *assessmentID
	}
	if assetID != nil {
		inv.AssetID = *assetID
	}
	return &inv, nil
}

// nullableUUID maps the nil UUID to SQL NULL, so "no assessment yet" is absent in the
// database rather than a magic all-zero id.
func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func boundedLimit(limit int) int {
	if limit <= 0 || limit > 200 {
		return 50
	}
	return limit
}
