// Package documents accepts the encrypted upload behind a receivable.
//
// It spans two things that must not come apart: the ciphertext and the record that says an
// invoice has one. A stored blob nobody references is litter; an invoice marked UPLOADED with
// nothing behind it stops the assessment that follows. Both are written in one transaction.
package documents

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/objects"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// TxRunner is the transaction boundary the service needs.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// Actor is the authenticated caller.
type Actor struct {
	OrganizationID uuid.UUID
	Operator       bool
}

// Service stores encrypted documents and hands them back.
type Service struct {
	db       TxRunner
	invoices invoice.Repository
	objects  objects.Store
	audit    audit.Recorder
	now      func() time.Time
}

// Config wires the service.
type Config struct {
	DB       TxRunner
	Invoices invoice.Repository
	Objects  objects.Store
	Audit    audit.Recorder
	Now      func() time.Time
}

// NewService returns the service.
func NewService(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Audit == nil {
		cfg.Audit = audit.Discard{}
	}
	return &Service{
		db:       cfg.DB,
		invoices: cfg.Invoices,
		objects:  cfg.Objects,
		audit:    cfg.Audit,
		now:      cfg.Now,
	}
}

// UploadParams is one encrypted document.
type UploadParams struct {
	InvoiceID uuid.UUID
	// Ciphertext was encrypted in the browser. The key was not sent with it, and there is
	// nowhere in this system that could decrypt it.
	Ciphertext []byte
	MIME       string
	// KeyRef records where the key lives, so a reader knows why the platform cannot open
	// the document rather than assuming something was lost.
	KeyRef string
}

// Result is what the caller gets back.
type Result struct {
	Invoice  *invoice.Invoice
	Document *invoice.Document
}

/*
Upload stores the ciphertext and moves the invoice on.

The digest is computed here, over the bytes that were actually stored, rather than taken
from the uploader: a hash a client supplies proves only what the client claimed, and this
one goes on to bind the assessment and the audit trail.
*/
func (s *Service) Upload(ctx context.Context, actor Actor, p UploadParams) (*Result, error) {
	if len(p.Ciphertext) == 0 {
		return nil, apperr.Invalid("ciphertext", "must not be empty")
	}

	var result Result
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := s.invoices.Get(ctx, q, p.InvoiceID)
		if err != nil {
			return err
		}
		if !actor.Operator && inv.IssuerID != actor.OrganizationID {
			// Someone else's receivable: saying it exists is itself a disclosure.
			return apperr.NotFoundf("invoice %s", p.InvoiceID)
		}

		object, err := objects.New(inv.ID, p.Ciphertext, s.now())
		if err != nil {
			return err
		}
		if err := s.objects.Put(ctx, q, object); err != nil {
			return err
		}

		document, err := invoice.NewDocument(invoice.NewDocumentParams{
			InvoiceID:  inv.ID,
			ObjectKey:  object.Key,
			CipherHash: object.CipherHash,
			KeyRef:     p.KeyRef,
			MIME:       p.MIME,
			SizeBytes:  object.SizeBytes,
		}, s.now())
		if err != nil {
			return err
		}

		before := map[string]any{"status": inv.Status.String(), "version": inv.Version}
		expectedVersion := inv.Version
		if err := inv.MarkUploaded(s.now()); err != nil {
			return err
		}
		if err := s.invoices.SaveDocument(ctx, q, document); err != nil {
			return err
		}
		if err := s.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}

		if err := s.audit.Record(ctx, q,
			audit.Of(ctx, actor.OrganizationID.String(), invoice.ActionDocumentAttached,
				invoice.EntityType, inv.ID.String(), s.now()).
				Between(before, map[string]any{"status": inv.Status.String(), "version": inv.Version}).
				With("status", inv.Status.String()).
				With("cipher_hash", document.CipherHash).
				With("size_bytes", document.SizeBytes)); err != nil {
			return err
		}

		result = Result{Invoice: inv, Document: document}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

/*
Fetch returns the ciphertext to its owner.

It is returned as it was stored, which is the only form it exists in: the browser that
uploaded it holds the key, so this endpoint hands back something only that browser can open.
That is worth having — it is how an issuer proves to themselves that what the platform kept
is unreadable.
*/
func (s *Service) Fetch(ctx context.Context, actor Actor, invoiceID uuid.UUID) (*objects.Object, error) {
	inv, err := s.invoices.Get(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}
	if !actor.Operator && inv.IssuerID != actor.OrganizationID {
		return nil, apperr.NotFoundf("invoice %s", invoiceID)
	}

	document, err := s.invoices.GetDocument(ctx, s.db.Querier(), inv.ID)
	if err != nil {
		return nil, err
	}
	return s.objects.Get(ctx, s.db.Querier(), document.ObjectKey)
}
