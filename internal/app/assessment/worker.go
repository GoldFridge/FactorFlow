// Package assessment orchestrates one confidential invoice assessment across the modules
// that each own a piece of it.
//
// It lives in the application layer rather than inside any one domain module because it
// necessarily touches three of them: the invoice it moves, the market it prices against,
// and the risk model that scores it. Putting it inside risk would have made risk depend on
// invoice, and the module boundaries the specification draws would stop meaning anything.
package assessment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// AssessmentWorker performs one confidential assessment end to end: it runs the workflow,
// takes a fresh market snapshot, scores and prices the receivable, and records the result.
//
// This is where the specification's separation lives in code. The workflow returns evidence
// about a document nobody else may read; the market provides a benchmark; and the pricing
// decision is made here by versioned deterministic code from those two inputs.
type AssessmentWorker struct {
	db          TxRunner
	narrator    *Narrator
	invoices    invoice.Repository
	assessments risk.Repository
	snapshots   marketdata.Repository
	market      *marketdata.Service
	workflow    risk.Workflow
	model       risk.Model
	query       marketdata.Query
	audit       audit.Recorder
	now         func() time.Time
	ids         func() uuid.UUID
}

// ActionAssessed is the timeline entry for a completed assessment.
const ActionAssessed = "invoice.assessed"

// TxRunner is the transaction boundary the worker needs.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// WorkerConfig wires the worker's collaborators.
type WorkerConfig struct {
	DB          TxRunner
	Invoices    invoice.Repository
	Assessments risk.Repository
	Snapshots   marketdata.Repository
	Market      *marketdata.Service
	Workflow    risk.Workflow
	Model       risk.Model
	// Query is the market question this deployment prices against.
	Query marketdata.Query
	// Narrator puts the score into words. It is optional: without one, every assessment is
	// explained by the derived narration.
	Narrator *Narrator
	Audit    audit.Recorder
	Now      func() time.Time
	IDs      func() uuid.UUID
}

// NewAssessmentWorker returns the worker.
func NewAssessmentWorker(cfg WorkerConfig) *AssessmentWorker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IDs == nil {
		cfg.IDs = uuid.New
	}
	if cfg.Model.Version == "" {
		cfg.Model = risk.ModelV1()
	}
	if cfg.Audit == nil {
		cfg.Audit = audit.Discard{}
	}

	if cfg.Narrator == nil {
		cfg.Narrator = NewNarrator(nil, cfg.Now)
	}

	return &AssessmentWorker{
		db:          cfg.DB,
		narrator:    cfg.Narrator,
		invoices:    cfg.Invoices,
		assessments: cfg.Assessments,
		snapshots:   cfg.Snapshots,
		market:      cfg.Market,
		workflow:    cfg.Workflow,
		model:       cfg.Model,
		query:       cfg.Query,
		audit:       cfg.Audit,
		now:         cfg.Now,
		ids:         cfg.IDs,
	}
}

// assessCommand mirrors the payload the invoice module publishes.
type assessCommand struct {
	InvoiceID  uuid.UUID `json:"invoice_id"`
	ObjectKey  string    `json:"object_key"`
	CipherHash string    `json:"cipher_hash"`
	KeyRef     string    `json:"key_ref"`
	MIME       string    `json:"mime"`
}

// Handle is the outbox handler for invoice.assess.
//
// It is idempotent, as every outbox handler must be: an invoice that already left
// EXTRACTING has been assessed, and a redelivery returns without scoring it twice.
func (w *AssessmentWorker) Handle(ctx context.Context, event outbox.Event) error {
	var command assessCommand
	if err := json.Unmarshal(event.Payload, &command); err != nil {
		return fmt.Errorf("decoding assess command: %w", err)
	}
	if command.InvoiceID == uuid.Nil {
		return apperr.Invalid("invoice_id", "must be a non-nil UUID")
	}

	inv, err := w.invoices.Get(ctx, w.db.Querier(), command.InvoiceID)
	if err != nil {
		return err
	}
	if inv.Status != invoice.StatusExtracting {
		// Already assessed, already failed, or withdrawn: a redelivery is not a second
		// assessment.
		return nil
	}

	request, result, err := w.runWorkflow(ctx, command)
	if err != nil {
		if errors.Is(err, risk.ErrCollectedElsewhere) {
			// The confidential workflow takes its own work. The invoice stays where it is
			// and the answer arrives through Complete; failing it here would mark an
			// assessment failed for the crime of not being synchronous.
			return nil
		}
		return w.recordFailure(ctx, command.InvoiceID, err)
	}

	snapshot, err := w.snapshot(ctx)
	if err != nil {
		return w.recordFailure(ctx, command.InvoiceID, err)
	}

	assessment, err := w.score(inv, snapshot, request, result)
	if err != nil {
		return w.recordFailure(ctx, command.InvoiceID, err)
	}

	if err := w.commit(ctx, inv, snapshot, assessment); err != nil {
		return err
	}

	// The narration happens after the price is stored and never blocks it. A model that is
	// slow, unreachable or wrong about a number costs this invoice its prose and nothing
	// else — which is the whole reason the deterministic path does not call one.
	w.narrate(ctx, assessment)
	return nil
}

// narrate replaces the derived explanation with a model's, when there is one worth keeping.
func (w *AssessmentWorker) narrate(ctx context.Context, assessment *risk.Assessment) {
	if !w.narrator.Available() {
		return
	}

	explanation := w.narrator.Narrate(ctx, assessment)
	if explanation == nil || explanation.Source != risk.SourceModel {
		// The derived one is already stored, and rewriting it would only move its timestamp.
		return
	}

	if err := w.db.InTx(ctx, func(q postgres.Querier) error {
		return w.assessments.SaveExplanation(ctx, q, explanation)
	}); err != nil {
		slog.WarnContext(ctx, "the narration could not be stored",
			slog.String("assessment_id", assessment.ID.String()),
			slog.String("error", err.Error()))
	}
}

// runWorkflow calls the confidential workflow and refuses a result it cannot trust.
func (w *AssessmentWorker) runWorkflow(ctx context.Context, command assessCommand) (risk.WorkflowRequest, risk.WorkflowResult, error) {
	request := risk.WorkflowRequest{
		InvoiceID:  command.InvoiceID,
		ObjectKey:  command.ObjectKey,
		CipherHash: command.CipherHash,
		KeyRef:     command.KeyRef,
		MIME:       command.MIME,
		// Derived rather than generated: a workflow that collects its own work has to
		// arrive at the same nonce without being told it, and a verifier has to be able to
		// recompute the commitment later.
		Nonce:         risk.DeriveNonce(command.InvoiceID, command.CipherHash),
		SchemaVersion: risk.FeatureSchemaV1,
	}

	result, err := w.workflow.Assess(ctx, request)
	if err != nil {
		if errors.Is(err, risk.ErrCollectedElsewhere) {
			return request, risk.WorkflowResult{}, err
		}
		return request, risk.WorkflowResult{}, apperr.Unavailablef("confidential workflow: %v", err)
	}
	if err := result.Validate(request); err != nil {
		return request, risk.WorkflowResult{}, err
	}
	return request, result, nil
}

// snapshot takes a fresh market observation and refuses a stale one.
func (w *AssessmentWorker) snapshot(ctx context.Context) (*marketdata.Snapshot, error) {
	snapshot, err := w.market.Snapshot(ctx, w.query)
	if err != nil {
		return nil, err
	}
	if err := snapshot.EnsureFresh(w.now()); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// score computes the assessment from the workflow's features and the market snapshot.
//
// The market's own volatility replaces whatever the workflow reported for that feature: it
// is an observation of the market, not of the document, and the snapshot is the authority
// on it.
func (w *AssessmentWorker) score(inv *invoice.Invoice, snapshot *marketdata.Snapshot, request risk.WorkflowRequest, result risk.WorkflowResult) (*risk.Assessment, error) {
	features := result.Features
	features.MarketVolatility = snapshot.Volatility

	daysToDue := inv.DaysToDue(w.now())
	if daysToDue <= 0 {
		return nil, apperr.Invalid("due_at", "invoice %s is already due and cannot be financed", inv.ID)
	}

	return w.model.Assess(risk.AssessInput{
		ID:                     w.ids(),
		InvoiceID:              inv.ID,
		Face:                   inv.Face,
		DaysToDue:              daysToDue,
		Features:               features,
		Confidence:             result.Confidence,
		Mitigations:            result.Mitigations,
		ArithmeticValid:        result.ArithmeticValid,
		Benchmark:              snapshot.Benchmark,
		LiquidityPremium:       snapshot.LiquidityPremium,
		MarketSnapshotHash:     snapshot.PayloadHash,
		ConfidentialCommitment: result.Commitment,
		ConfidentialNonce:      request.Nonce,
	}, w.now())
}

// commit stores the snapshot, the assessment and the invoice's new state together.
//
// The order matters: the assessment references the snapshot, and the invoice references the
// assessment, so all three land in one transaction or none of them do.
func (w *AssessmentWorker) commit(ctx context.Context, inv *invoice.Invoice, snapshot *marketdata.Snapshot, assessment *risk.Assessment) error {
	return w.db.InTx(ctx, func(q postgres.Querier) error {
		if err := w.snapshots.Save(ctx, q, snapshot); err != nil {
			return err
		}
		if err := w.assessments.Save(ctx, q, assessment); err != nil {
			return err
		}
		// The derived explanation lands with the assessment, so a reader is never looking at
		// a price with no words beside it. A model's narration replaces it afterwards, if
		// one answers and what it wrote survives checking.
		if err := w.assessments.SaveExplanation(ctx, q, risk.Derive(assessment, w.now())); err != nil {
			return err
		}

		expectedVersion := inv.Version
		if err := inv.CompleteAssessment(assessment.ID, w.now()); err != nil {
			return err
		}
		if err := w.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}

		// The commitment and the snapshot hash are the two things a reader needs to check
		// the price later; the features and the score itself stay in the assessment.
		return w.audit.Record(ctx, q,
			audit.Of(ctx, audit.SystemActor, ActionAssessed, invoice.EntityType, inv.ID.String(), w.now()).
				With("assessment_id", assessment.ID.String()).
				With("model_version", assessment.ModelVersion).
				With("grade", assessment.Grade.String()).
				With("market_snapshot_hash", assessment.MarketSnapshotHash).
				With("confidential_commitment", assessment.ConfidentialCommitment).
				With("requires_manual_review", assessment.RequiresManualReview).
				With("status", inv.Status.String()))
	})
}

/*
Complete finishes an assessment whose confidential half ran somewhere else.

It is the other end of ErrCollectedElsewhere: the workflow collected the work, opened the
document inside an enclave and returned a feature vector, and everything after that — the
market snapshot, the score, the price, the audit line — is the same code the in-process path
runs. What arrives from outside is treated as untrusted input and validated against the
request it claims to answer before any of it is scored.
*/
func (w *AssessmentWorker) Complete(ctx context.Context, invoiceID uuid.UUID, result risk.WorkflowResult) error {
	inv, err := w.invoices.Get(ctx, w.db.Querier(), invoiceID)
	if err != nil {
		return err
	}
	if inv.Status != invoice.StatusExtracting {
		// Not waiting for an answer. A workflow that delivers twice, or delivers late for
		// an invoice somebody already failed by hand, must not reopen it.
		return apperr.Conflictf("invoice %s is %s and is not waiting for an assessment",
			inv.ID, inv.Status)
	}

	document, err := w.invoices.GetDocument(ctx, w.db.Querier(), inv.ID)
	if err != nil {
		return err
	}

	request := risk.WorkflowRequest{
		InvoiceID:     inv.ID,
		ObjectKey:     document.ObjectKey,
		CipherHash:    document.CipherHash,
		KeyRef:        document.KeyRef,
		MIME:          document.MIME,
		Nonce:         risk.DeriveNonce(inv.ID, document.CipherHash),
		SchemaVersion: risk.FeatureSchemaV1,
	}
	if err := result.Validate(request); err != nil {
		return err
	}

	snapshot, err := w.snapshot(ctx)
	if err != nil {
		return w.recordFailure(ctx, inv.ID, err)
	}

	assessment, err := w.score(inv, snapshot, request, result)
	if err != nil {
		return w.recordFailure(ctx, inv.ID, err)
	}
	if err := w.commit(ctx, inv, snapshot, assessment); err != nil {
		return err
	}

	w.narrate(ctx, assessment)
	return nil
}

// recordFailure moves the invoice to FAILED and returns the cause.
//
// The error is returned as well as recorded, so the outbox retries with backoff: a TEE that
// timed out is usually reachable again on the next attempt, and only a repeated failure
// parks the event for an operator.
func (w *AssessmentWorker) recordFailure(ctx context.Context, invoiceID uuid.UUID, cause error) error {
	markErr := w.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := w.invoices.Get(ctx, q, invoiceID)
		if err != nil {
			return err
		}
		if inv.Status != invoice.StatusExtracting {
			return nil
		}

		expectedVersion := inv.Version
		if err := inv.FailAssessment(safeReason(cause), w.now()); err != nil {
			return err
		}
		return w.invoices.Update(ctx, q, inv, expectedVersion)
	})
	if markErr != nil {
		return fmt.Errorf("assessment failed (%v) and could not be recorded: %w", cause, markErr)
	}
	return cause
}

// maxReasonLen keeps a stored reason short and operator-facing.
const maxReasonLen = 200

// safeReason trims a failure to something that belongs in an operator's view.
//
// A workflow error can only ever describe transport or schema problems, never document
// content, because the enclave has nothing else to report. Trimming bounds it anyway.
func safeReason(cause error) string {
	reason := cause.Error()
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen] + "..."
	}
	return reason
}
