// Package reporting answers the questions a participant asks about a decision that was
// already made: why this price, and against which market.
//
// It spans modules because an explanation does: the assessment belongs to risk, the
// benchmark to marketdata, and who may see either is the invoice's question.
package reporting

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// TxRunner is the read boundary the service needs.
type TxRunner interface {
	Querier() postgres.Querier
}

// Actor is the authenticated caller.
type Actor struct {
	OrganizationID uuid.UUID
	Operator       bool
}

// Service reads assessments and market snapshots back.
type Service struct {
	db          TxRunner
	invoices    invoice.Repository
	assessments risk.Repository
	snapshots   marketdata.Repository
	market      marketdata.Query
}

// Config wires the service.
type Config struct {
	DB          TxRunner
	Invoices    invoice.Repository
	Assessments risk.Repository
	Snapshots   marketdata.Repository
	// Market names the question this deployment prices against, so the latest snapshot can
	// be found without the caller knowing how it was taken.
	Market marketdata.Query
}

// NewService returns the service.
func NewService(cfg Config) *Service {
	return &Service{
		db:          cfg.DB,
		invoices:    cfg.Invoices,
		assessments: cfg.Assessments,
		snapshots:   cfg.Snapshots,
		market:      cfg.Market,
	}
}

// Report is everything behind one published price.
//
// It carries the market snapshot as well as the assessment, because "why this price" is
// only answerable with both: the model's own contributions explain the risk premium, and
// the snapshot explains the benchmark it was added to.
type Report struct {
	Assessment *risk.Assessment
	Snapshot   *marketdata.Snapshot
	// Contributions are ordered by how much each feature moved the score.
	Contributions []risk.Contribution
}

// AssessmentFor returns the latest assessment of an invoice the caller may see.
func (s *Service) AssessmentFor(ctx context.Context, actor Actor, invoiceID uuid.UUID) (*Report, error) {
	inv, err := s.invoices.Get(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}
	if !actor.Operator && inv.IssuerID != actor.OrganizationID {
		// An assessment describes a private document's risk. Telling a stranger it exists
		// is itself a disclosure, so the answer is that the invoice does not exist.
		return nil, apperr.NotFoundf("invoice %s", invoiceID)
	}

	assessment, err := s.assessments.Latest(ctx, s.db.Querier(), inv.ID)
	if err != nil {
		return nil, err
	}

	report := &Report{Assessment: assessment, Contributions: assessment.RankedContributions()}

	// The snapshot is best-effort: an assessment stays explainable even if the snapshot row
	// was pruned, and a missing benchmark should not hide the score it produced.
	if snapshot, err := s.snapshots.Get(ctx, s.db.Querier(), assessment.MarketSnapshotHash); err == nil {
		report.Snapshot = snapshot
	} else if !apperr.IsNotFound(err) {
		return nil, err
	}

	return report, nil
}

// LatestSnapshot returns the market observation the next price will be computed from.
func (s *Service) LatestSnapshot(ctx context.Context, actor Actor, now time.Time) (*marketdata.Snapshot, bool, error) {
	if actor.OrganizationID == uuid.Nil && !actor.Operator {
		return nil, false, apperr.Forbiddenf("authentication is required to view market data")
	}

	snapshot, err := s.snapshots.Latest(ctx, s.db.Querier(), s.market.Network, s.market.Asset)
	if err != nil {
		return nil, false, err
	}

	// Freshness is reported rather than enforced here: this endpoint describes what the
	// market looked like, and pricing is where a stale snapshot has to fail closed.
	return snapshot, snapshot.IsFresh(now), nil
}
