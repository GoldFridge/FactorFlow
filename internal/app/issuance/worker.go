// Package issuance turns an approved invoice into a tokenized asset.
//
// Like the assessment worker, it lives in the application layer because it spans modules:
// it moves an invoice, reads the price the risk model published, and asks the tokenization
// port to mint. The chain call happens outside the database transaction, which is exactly
// why the command reaches it through the outbox rather than from an HTTP handler.
package issuance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

// TxRunner is the transaction boundary the worker needs.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// OrganizationWallets resolves the wallet that receives an asset's initial supply.
//
// The worker needs one fact about an organization and defines a port for it rather than
// importing the organization module, which would tie two domain modules together for a
// single string.
type OrganizationWallets interface {
	WalletOf(ctx context.Context, q postgres.Querier, organizationID uuid.UUID) (string, error)
}

// Worker issues the asset for one approved invoice.
type Worker struct {
	db          TxRunner
	invoices    invoice.Repository
	assessments risk.Repository
	assets      tokenization.Repository
	wallets     OrganizationWallets
	issuer      tokenization.Issuer
	audit       audit.Recorder
	now         func() time.Time
	ids         func() uuid.UUID
}

// ActionTokenized is the timeline entry for a minted receivable.
const ActionTokenized = "invoice.tokenized"

// Config wires the worker's collaborators.
type Config struct {
	DB          TxRunner
	Invoices    invoice.Repository
	Assessments risk.Repository
	Assets      tokenization.Repository
	Wallets     OrganizationWallets
	Issuer      tokenization.Issuer
	Audit       audit.Recorder
	Now         func() time.Time
	IDs         func() uuid.UUID
}

// NewWorker returns the worker.
func NewWorker(cfg Config) *Worker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IDs == nil {
		cfg.IDs = uuid.New
	}
	if cfg.Audit == nil {
		cfg.Audit = audit.Discard{}
	}
	return &Worker{
		db:          cfg.DB,
		invoices:    cfg.Invoices,
		assessments: cfg.Assessments,
		assets:      cfg.Assets,
		wallets:     cfg.Wallets,
		issuer:      cfg.Issuer,
		audit:       cfg.Audit,
		now:         cfg.Now,
		ids:         cfg.IDs,
	}
}

// tokenizeCommand mirrors the payload the invoice module publishes.
type tokenizeCommand struct {
	InvoiceID uuid.UUID `json:"invoice_id"`
	IssuerID  uuid.UUID `json:"issuer_id"`
}

// Handle is the outbox handler for invoice.tokenize.
//
// It is idempotent in two layers: an invoice that already left TOKENIZING is skipped, and
// an invoice that already has an asset is finished rather than minted again. Minting twice
// would put two claims on one receivable, which no later reconciliation could undo.
func (w *Worker) Handle(ctx context.Context, event outbox.Event) error {
	var command tokenizeCommand
	if err := json.Unmarshal(event.Payload, &command); err != nil {
		return fmt.Errorf("decoding tokenize command: %w", err)
	}
	if command.InvoiceID == uuid.Nil {
		return apperr.Invalid("invoice_id", "must be a non-nil UUID")
	}

	inv, err := w.invoices.Get(ctx, w.db.Querier(), command.InvoiceID)
	if err != nil {
		return err
	}
	if inv.Status != invoice.StatusTokenizing {
		return nil
	}

	if existing, err := w.assets.GetByInvoice(ctx, w.db.Querier(), inv.ID); err == nil {
		// A redelivery after the asset was minted but before the invoice moved.
		return w.complete(ctx, inv, existing)
	} else if !apperr.IsNotFound(err) {
		return err
	}

	assessment, err := w.assessments.Latest(ctx, w.db.Querier(), inv.ID)
	if err != nil {
		return w.recordFailure(ctx, inv.ID, err)
	}

	wallet, err := w.wallets.WalletOf(ctx, w.db.Querier(), inv.IssuerID)
	if err != nil {
		return w.recordFailure(ctx, inv.ID, err)
	}

	result, err := w.issuer.Issue(ctx, tokenization.IssueRequest{
		InvoiceID:    inv.ID,
		IssuerID:     inv.IssuerID,
		IssuerWallet: wallet,
		Supply:       inv.Face,
		// The reference makes issuance repeatable: the same invoice and assessment always
		// ask for the same asset, so a retried command cannot mint a second one.
		Reference: assessment.ID.String(),
		Metadata: tokenization.Metadata{
			InvoiceCommitment: assessment.ConfidentialCommitment,
			Currency:          inv.Face.Currency().String(),
			FaceValue:         inv.Face.String(),
			MaturityDate:      inv.DueAt.Format(time.RFC3339),
			Grade:             assessment.Grade.String(),
			TermsHash:         assessment.MarketSnapshotHash,
		},
	})
	if err != nil {
		return w.recordFailure(ctx, inv.ID, apperr.Unavailablef("asset issuer: %v", err))
	}
	if err := result.Validate(); err != nil {
		return w.recordFailure(ctx, inv.ID, err)
	}

	status := tokenization.StatusPending
	if result.Confirmed {
		status = tokenization.StatusIssued
	}

	asset, err := tokenization.New(tokenization.NewParams{
		ID:            w.ids(),
		InvoiceID:     inv.ID,
		IssuerID:      inv.IssuerID,
		Network:       result.Network,
		TokenID:       result.TokenID,
		ContractID:    result.ContractID,
		Supply:        inv.Face,
		ChainStatus:   status,
		TransactionID: result.TransactionID,
		ExplorerURL:   result.ExplorerURL,
	}, w.now())
	if err != nil {
		return w.recordFailure(ctx, inv.ID, err)
	}

	return w.commit(ctx, inv, asset)
}

// commit stores the asset and moves the invoice, together.
func (w *Worker) commit(ctx context.Context, inv *invoice.Invoice, asset *tokenization.Asset) error {
	return w.db.InTx(ctx, func(q postgres.Querier) error {
		if err := w.assets.Create(ctx, q, asset); err != nil {
			return err
		}
		return w.finish(ctx, q, inv, asset)
	})
}

// complete finishes an invoice whose asset already exists.
func (w *Worker) complete(ctx context.Context, inv *invoice.Invoice, asset *tokenization.Asset) error {
	return w.db.InTx(ctx, func(q postgres.Querier) error {
		return w.finish(ctx, q, inv, asset)
	})
}

// finish moves the invoice and records the mint. Both redelivery paths share it, so a
// retried command produces the same timeline entry as the first attempt rather than a
// second, differently shaped one.
func (w *Worker) finish(ctx context.Context, q postgres.Querier, inv *invoice.Invoice, asset *tokenization.Asset) error {
	expectedVersion := inv.Version
	if err := inv.CompleteTokenization(asset.ID, w.now()); err != nil {
		return err
	}
	if err := w.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
		return err
	}

	return w.audit.Record(ctx, q,
		audit.Of(ctx, audit.SystemActor, ActionTokenized, invoice.EntityType, inv.ID.String(), w.now()).
			With("asset_id", asset.ID.String()).
			With("network", asset.Network).
			With("token_id", asset.TokenID).
			With("chain_status", asset.ChainStatus.String()).
			With("transaction_id", asset.TransactionID).
			With("status", inv.Status.String()))
}

// recordFailure marks the invoice failed at the issuance stage and returns the cause, so
// the outbox retries and only a repeated failure parks the event for an operator.
func (w *Worker) recordFailure(ctx context.Context, invoiceID uuid.UUID, cause error) error {
	markErr := w.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := w.invoices.Get(ctx, q, invoiceID)
		if err != nil {
			return err
		}
		if inv.Status != invoice.StatusTokenizing {
			return nil
		}

		expectedVersion := inv.Version
		if err := inv.FailTokenization(safeReason(cause), w.now()); err != nil {
			return err
		}
		return w.invoices.Update(ctx, q, inv, expectedVersion)
	})
	if markErr != nil {
		return fmt.Errorf("issuance failed (%v) and could not be recorded: %w", cause, markErr)
	}
	return cause
}

// maxReasonLen keeps a stored reason operator-facing.
const maxReasonLen = 200

func safeReason(cause error) string {
	reason := cause.Error()
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen] + "..."
	}
	return reason
}
