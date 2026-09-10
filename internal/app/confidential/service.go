/*
Package confidential is the platform's half of the confidential assessment.

The other half runs inside an attested enclave on somebody else's infrastructure and cannot
be called and waited on. So this is a counter rather than a function call: work waiting to
be done is handed over when the workflow asks for it, and the answer is taken back when the
workflow returns.

What is handed over is the ciphertext, its digest, and the nonce the run is bound to. Not
the key — the platform does not have it, and that is the point rather than an oversight.
The key travels from the browser that made it to the enclave that reads with it, and there
is no request anybody can make of this service that would produce it.
*/
package confidential

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/objects"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// TxRunner is the read boundary the service needs.
type TxRunner interface {
	Querier() postgres.Querier
}

// Assessor finishes an assessment whose confidential half has come back.
//
// It is an interface rather than the worker itself so this package does not depend on how
// scoring is wired: what it needs is somewhere to hand a validated result.
type Assessor interface {
	Complete(ctx context.Context, invoiceID uuid.UUID, result risk.WorkflowResult) error
}

// Service hands out work and takes back results.
type Service struct {
	db       TxRunner
	invoices invoice.Repository
	objects  objects.Store
	assessor Assessor
	now      func() time.Time
}

// Config wires the service.
type Config struct {
	DB       TxRunner
	Invoices invoice.Repository
	Objects  objects.Store
	Assessor Assessor
	Now      func() time.Time
}

// NewService returns the service.
func NewService(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{
		db:       cfg.DB,
		invoices: cfg.Invoices,
		objects:  cfg.Objects,
		assessor: cfg.Assessor,
		now:      cfg.Now,
	}
}

// Work is one assessment waiting for a confidential run.
type Work struct {
	InvoiceID uuid.UUID
	// Nonce binds the run to this request. It is derived from the invoice and the digest,
	// so the workflow and the platform arrive at the same value without exchanging it.
	Nonce      string
	CipherHash string
	MIME       string
	// Ciphertext is the document as it is stored: bytes nobody here can read.
	Ciphertext []byte
	// SchemaVersion is the feature schema the answer must be in.
	SchemaVersion string
}

/*
Next returns the oldest assessment waiting to be done, or nothing.

Oldest first, because a queue that hands out the newest work leaves the unlucky invoice
waiting forever. There is no claim or lease: a workflow that collects the same item twice
produces the same commitment over the same document, and the second answer is refused by
the invoice's own state rather than by a lock nobody would be around to release.
*/
func (s *Service) Next(ctx context.Context) (*Work, error) {
	waiting, err := s.invoices.ListByStatus(ctx, s.db.Querier(), invoice.StatusExtracting, 20)
	if err != nil {
		return nil, err
	}

	for _, inv := range waiting {
		document, err := s.invoices.GetDocument(ctx, s.db.Querier(), inv.ID)
		if err != nil {
			if apperr.IsNotFound(err) {
				// Waiting to be extracted with nothing to extract. That is a broken invoice
				// rather than work, and it is not this service's to fix.
				continue
			}
			return nil, err
		}

		object, err := s.objects.Get(ctx, s.db.Querier(), document.ObjectKey)
		if err != nil {
			if apperr.IsNotFound(err) {
				continue
			}
			return nil, err
		}

		return &Work{
			InvoiceID:     inv.ID,
			Nonce:         risk.DeriveNonce(inv.ID, document.CipherHash),
			CipherHash:    document.CipherHash,
			MIME:          document.MIME,
			Ciphertext:    object.Ciphertext,
			SchemaVersion: risk.FeatureSchemaV1,
		}, nil
	}
	return nil, nil
}

/*
Deliver takes an answer back.

The result is validated against the request it claims to answer before any of it is scored —
this arrived over HTTP from a system the platform does not run, and a feature vector is a
number that goes straight into a price. What the platform checks is that it is for the right
invoice, in the expected schema, with features in range and a commitment that is at least
shaped like one.
*/
func (s *Service) Deliver(ctx context.Context, result risk.WorkflowResult) error {
	if result.InvoiceID == uuid.Nil {
		return apperr.Invalid("invoice_id", "must name the invoice this assessment answers")
	}
	return s.assessor.Complete(ctx, result.InvoiceID, result)
}
