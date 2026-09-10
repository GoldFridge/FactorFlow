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

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
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

// Listings answers whether a receivable was offered to the venue, and in which batch.
//
// It is the whole of what this service needs from the auction module: the question here is
// not how a batch clears but whether a bidder was asked to price this paper, because that
// is what turns the issuer's private receivable into something a stranger may read the
// terms of.
type Listings interface {
	ListingOf(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (auction.Listing, error)
}

// Service reads assessments and market snapshots back.
type Service struct {
	db          TxRunner
	invoices    invoice.Repository
	assessments risk.Repository
	snapshots   marketdata.Repository
	timeline    audit.Reader
	listings    Listings
	market      marketdata.Query
}

// Config wires the service.
type Config struct {
	DB          TxRunner
	Invoices    invoice.Repository
	Assessments risk.Repository
	Snapshots   marketdata.Repository
	// Timeline reads the audit trail back. It is optional: a deployment without one still
	// answers every other question, it just cannot show the history.
	Timeline audit.Reader
	// Listings is optional too, and leaving it out is the closed position: without it
	// nothing is ever disclosed beyond the issuer.
	Listings Listings
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
		timeline:    cfg.Timeline,
		listings:    cfg.Listings,
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
	// Explanation is the words stored about this price. It names its own source, because a
	// sentence from a model and one derived from the coefficients carry different authority.
	Explanation *risk.Explanation
}

// Disclosure is one listed receivable as a participant of the venue may read it.
//
// It carries the terms that were put on the board and the reasoning behind the price, and
// deliberately not the document, its metadata or the issuer's own history: what is
// disclosed is what a bidder was asked to price, and nothing beyond it.
type Disclosure struct {
	Invoice *invoice.Invoice
	Listing auction.Listing
	// Report is absent when the assessment behind the listing is no longer on record. The
	// terms still stand, so a missing explanation must not hide them.
	Report *Report
	// Own reports whether the caller is the issuer of this receivable, which is the one
	// case where a fuller record exists elsewhere.
	Own bool
}

// ListingFor returns what a participant may read about a receivable that was offered.
//
// This is the answer to a bidder's question "what am I pricing", and it exists because the
// issuer's own record cannot be that answer: an invoice belongs to its issuer, and the
// terms of a lot belong to the venue it was offered in.
func (s *Service) ListingFor(ctx context.Context, actor Actor, invoiceID uuid.UUID) (*Disclosure, error) {
	if actor.OrganizationID == uuid.Nil && !actor.Operator {
		return nil, apperr.Forbiddenf("authentication is required to view a listing")
	}

	inv, err := s.invoices.Get(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}

	listing, own, err := s.readable(ctx, actor, inv)
	if err != nil {
		return nil, err
	}

	out := &Disclosure{Invoice: inv, Listing: listing, Own: own}

	// The price is best-effort for the same reason the snapshot is inside a report: the
	// terms of the lot are the disclosure, and an assessment that was never made or no
	// longer exists should leave them readable rather than turn them into a refusal.
	report, err := s.reportFor(ctx, inv.ID)
	if err != nil && !apperr.IsNotFound(err) {
		return nil, err
	}
	out.Report = report
	return out, nil
}

// AssessmentFor returns the latest assessment of an invoice the caller may see.
func (s *Service) AssessmentFor(ctx context.Context, actor Actor, invoiceID uuid.UUID) (*Report, error) {
	inv, err := s.invoices.Get(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.readable(ctx, actor, inv); err != nil {
		return nil, err
	}

	return s.reportFor(ctx, inv.ID)
}

// readable decides who may read a receivable, and reports what made it readable.
//
// The issuer and an operator may always look. Everyone else may look only at paper that
// was offered to the venue, and only once the batch left draft — bidding against terms
// nobody may read is not a market. Anyone else is told the invoice does not exist, because
// confirming that it does is itself the disclosure.
func (s *Service) readable(ctx context.Context, actor Actor, inv *invoice.Invoice) (auction.Listing, bool, error) {
	own := inv.IssuerID == actor.OrganizationID

	listing, err := s.listingOf(ctx, inv.ID)
	if err != nil && !apperr.IsNotFound(err) {
		return auction.Listing{}, own, err
	}

	if own || actor.Operator || listing.Disclosed() {
		return listing, own, nil
	}
	return auction.Listing{}, own, apperr.NotFoundf("invoice %s", inv.ID)
}

// listingOf reports where a receivable was offered, or that it was not.
func (s *Service) listingOf(ctx context.Context, invoiceID uuid.UUID) (auction.Listing, error) {
	if s.listings == nil {
		return auction.Listing{}, apperr.NotFoundf("listing of invoice %s", invoiceID)
	}
	return s.listings.ListingOf(ctx, s.db.Querier(), invoiceID)
}

// reportFor assembles the price and the market it was taken against.
func (s *Service) reportFor(ctx context.Context, invoiceID uuid.UUID) (*Report, error) {
	assessment, err := s.assessments.Latest(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}

	report := &Report{Assessment: assessment, Contributions: assessment.RankedContributions()}

	// The narration is best-effort in the same way as the snapshot: words about a price are
	// not the price, and a missing one must not hide the numbers.
	if explanation, err := s.assessments.GetExplanation(ctx, s.db.Querier(), assessment.ID); err == nil {
		report.Explanation = explanation
	} else if !apperr.IsNotFound(err) {
		return nil, err
	}

	// The snapshot is best-effort: an assessment stays explainable even if the snapshot row
	// was pruned, and a missing benchmark should not hide the score it produced.
	if snapshot, err := s.snapshots.Get(ctx, s.db.Querier(), assessment.MarketSnapshotHash); err == nil {
		report.Snapshot = snapshot
	} else if !apperr.IsNotFound(err) {
		return nil, err
	}

	return report, nil
}

// TimelineFor returns what was recorded about an invoice, newest first.
//
// The scope is the same as the assessment's: an invoice's history is as private as the
// invoice, so a stranger is told it does not exist rather than that they may not look.
func (s *Service) TimelineFor(ctx context.Context, actor Actor, invoiceID uuid.UUID, limit int) ([]audit.Event, error) {
	if s.timeline == nil {
		return nil, apperr.Unavailablef("this deployment does not keep an audit timeline")
	}
	if limit < 0 {
		return nil, apperr.Invalid("limit", "must not be negative")
	}

	inv, err := s.invoices.Get(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}
	if !actor.Operator && inv.IssuerID != actor.OrganizationID {
		return nil, apperr.NotFoundf("invoice %s", invoiceID)
	}

	return s.timeline.Timeline(ctx, s.db.Querier(), invoice.EntityType, inv.ID.String(), limit)
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
