// Package marketplace opens an auction for invoices that are ready to be financed.
//
// It is the one place that turns three modules into one action: it reads the invoice, the
// price the risk model published and the asset that was minted, assembles the lots, opens
// the batch and moves each invoice into it. Doing that through each module's own service
// would mean three transactions and a half-listed batch whenever one of them failed, so
// this composes their repositories inside a single transaction instead.
package marketplace

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

// MaxLotsPerAuction bounds one batch. It is a demo limit, not a solver limit: the clearing
// is bounded by its own published parameters.
const MaxLotsPerAuction = 50

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

// Service opens and clears auctions over tokenized invoices.
type Service struct {
	db          TxRunner
	invoices    invoice.Repository
	assessments risk.Repository
	assets      tokenization.Repository
	auctions    auction.Repository
	solver      *auction.Solver
	now         func() time.Time
	ids         func() uuid.UUID
}

// Config wires the service.
type Config struct {
	DB          TxRunner
	Invoices    invoice.Repository
	Assessments risk.Repository
	Assets      tokenization.Repository
	Auctions    auction.Repository
	Solver      *auction.Solver
	Now         func() time.Time
	IDs         func() uuid.UUID
}

// NewService returns the service.
func NewService(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IDs == nil {
		cfg.IDs = uuid.New
	}
	if cfg.Solver == nil {
		cfg.Solver = auction.NewSolver()
	}
	return &Service{
		db:          cfg.DB,
		invoices:    cfg.Invoices,
		assessments: cfg.Assessments,
		assets:      cfg.Assets,
		auctions:    cfg.Auctions,
		solver:      cfg.Solver,
		now:         cfg.Now,
		ids:         cfg.IDs,
	}
}

// OpenParams names the invoices to offer and the bidding window.
type OpenParams struct {
	InvoiceIDs []uuid.UUID
	OpensAt    time.Time
	ClosesAt   time.Time
}

// OpenAuction lists tokenized invoices as one batch and starts accepting bids.
func (s *Service) OpenAuction(ctx context.Context, actor Actor, p OpenParams) (*auction.Auction, error) {
	if actor.OrganizationID == uuid.Nil {
		return nil, apperr.Forbiddenf("an organization is required to open an auction")
	}
	switch {
	case len(p.InvoiceIDs) == 0:
		return nil, apperr.Invalid("invoice_ids", "must name at least one invoice")
	case len(p.InvoiceIDs) > MaxLotsPerAuction:
		return nil, apperr.Invalid("invoice_ids", "must name at most %d invoices", MaxLotsPerAuction)
	}
	if seen := uniqueIDs(p.InvoiceIDs); len(seen) != len(p.InvoiceIDs) {
		return nil, apperr.Invalid("invoice_ids", "must not repeat an invoice")
	}

	var created *auction.Auction
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		invoices := make([]*invoice.Invoice, 0, len(p.InvoiceIDs))
		lots := make([]auction.Lot, 0, len(p.InvoiceIDs))

		for _, invoiceID := range p.InvoiceIDs {
			inv, lot, err := s.buildLot(ctx, q, actor, invoiceID)
			if err != nil {
				return err
			}
			invoices = append(invoices, inv)
			lots = append(lots, lot)
		}

		a, err := auction.NewAuction(auction.NewAuctionParams{
			ID:       s.ids(),
			IssuerID: actor.OrganizationID,
			Lots:     lots,
			OpensAt:  p.OpensAt,
			ClosesAt: p.ClosesAt,
		}, s.now())
		if err != nil {
			return err
		}
		// The batch is created and opened in one step: an auction nobody can bid on is not
		// a state this endpoint has any reason to leave behind.
		if err := a.Open(s.now()); err != nil {
			return err
		}
		if err := s.auctions.CreateAuction(ctx, q, a); err != nil {
			return err
		}

		for _, inv := range invoices {
			expectedVersion := inv.Version
			if err := inv.OpenAuction(s.now()); err != nil {
				return err
			}
			if err := s.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
				return err
			}
		}

		created = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ClearAuction runs the solver over a closed batch and records everything it produced.
//
// One transaction covers the whole result: the auction moves to CLEARED, the allocations,
// the rejection reasons and the certificate are stored, each bid learns its outcome, and
// every invoice behind an allocated lot moves to ALLOCATED. Splitting that would leave an
// investor holding an allocation whose invoice still claims to be on sale.
//
// A failure therefore leaves everything exactly as it was, and the issuer clears again.
func (s *Service) ClearAuction(ctx context.Context, actor Actor, auctionID uuid.UUID) (*auction.Solution, error) {
	var solution *auction.Solution

	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		a, err := s.auctions.GetAuction(ctx, q, auctionID)
		if err != nil {
			return err
		}
		if !actor.Operator && a.IssuerID != actor.OrganizationID {
			return apperr.Forbiddenf("auction %s belongs to another issuer", auctionID)
		}

		bids, err := s.auctions.ListBids(ctx, q, a.ID)
		if err != nil {
			return err
		}

		clearingVersion := a.Version
		if err := a.StartClearing(s.now()); err != nil {
			return err
		}
		if err := s.auctions.UpdateAuction(ctx, q, a, clearingVersion); err != nil {
			return err
		}

		result, err := s.solver.Clear(a, bids, s.now())
		if err != nil {
			return err
		}
		if err := s.auctions.SaveSolution(ctx, q, result, s.now()); err != nil {
			return err
		}
		if err := s.recordBidOutcomes(ctx, q, bids, result); err != nil {
			return err
		}
		if err := s.recordInvoiceOutcomes(ctx, q, a, result); err != nil {
			return err
		}

		clearedVersion := a.Version
		if err := a.MarkCleared(result.SolverVersion, result.CertificateHash, s.now()); err != nil {
			return err
		}
		if err := s.auctions.UpdateAuction(ctx, q, a, clearedVersion); err != nil {
			return err
		}

		solution = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return solution, nil
}

// CancelAuction withdraws a batch and returns its invoices to their issuer.
//
// The release is the point: an auction that ended without a sale must not leave its
// receivables stranded in AUCTION_OPEN, unable to be listed again.
func (s *Service) CancelAuction(ctx context.Context, actor Actor, auctionID uuid.UUID, reason string) (*auction.Auction, error) {
	var cancelled *auction.Auction

	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		a, err := s.auctions.GetAuction(ctx, q, auctionID)
		if err != nil {
			return err
		}
		if !actor.Operator && a.IssuerID != actor.OrganizationID {
			return apperr.Forbiddenf("auction %s belongs to another issuer", auctionID)
		}

		expectedVersion := a.Version
		if err := a.Cancel(reason, s.now()); err != nil {
			return err
		}
		if err := s.auctions.UpdateAuction(ctx, q, a, expectedVersion); err != nil {
			return err
		}
		if err := s.releaseInvoices(ctx, q, a, reason); err != nil {
			return err
		}

		cancelled = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cancelled, nil
}

// releaseInvoices returns every lot's invoice to TOKENIZED so it can be listed again.
func (s *Service) releaseInvoices(ctx context.Context, q postgres.Querier, a *auction.Auction, reason string) error {
	for _, lot := range a.Lots {
		inv, err := s.invoices.Get(ctx, q, lot.InvoiceID)
		if err != nil {
			return err
		}
		if inv.Status != invoice.StatusAuctionOpen {
			continue
		}

		expectedVersion := inv.Version
		if err := inv.CancelAuction(reason, s.now()); err != nil {
			return err
		}
		if err := s.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}
	}
	return nil
}

// recordBidOutcomes marks each bid allocated or rejected.
func (s *Service) recordBidOutcomes(ctx context.Context, q postgres.Querier, bids []*auction.Bid, solution *auction.Solution) error {
	allocated := make(map[uuid.UUID]bool, len(solution.Allocations))
	for _, allocation := range solution.Allocations {
		allocated[allocation.BidID] = true
	}

	for _, bid := range bids {
		if bid.Status != auction.BidStatusActive {
			continue
		}

		expectedVersion := bid.Version
		if allocated[bid.ID] {
			bid.Status = auction.BidStatusAllocated
		} else {
			bid.Status = auction.BidStatusRejected
		}
		bid.Version++

		if err := s.auctions.UpdateBid(ctx, q, bid, expectedVersion); err != nil {
			return err
		}
	}
	return nil
}

// recordInvoiceOutcomes moves each financed invoice to ALLOCATED, and returns the ones that
// found no buyer to TOKENIZED so their issuer can list them again.
func (s *Service) recordInvoiceOutcomes(ctx context.Context, q postgres.Querier, a *auction.Auction, solution *auction.Solution) error {
	sold := make(map[uuid.UUID]bool, len(solution.Allocations))
	for _, allocation := range solution.Allocations {
		sold[allocation.InvoiceID] = true
	}

	for _, lot := range a.Lots {
		inv, err := s.invoices.Get(ctx, q, lot.InvoiceID)
		if err != nil {
			return err
		}
		if inv.Status != invoice.StatusAuctionOpen {
			continue
		}

		expectedVersion := inv.Version
		if sold[lot.InvoiceID] {
			err = inv.MarkAllocated(s.now())
		} else {
			err = inv.CancelAuction("no bid took this lot", s.now())
		}
		if err != nil {
			return err
		}
		if err := s.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}
	}
	return nil
}

// buildLot turns one tokenized invoice into a lot at the price the risk model published.
func (s *Service) buildLot(ctx context.Context, q postgres.Querier, actor Actor, invoiceID uuid.UUID) (*invoice.Invoice, auction.Lot, error) {
	inv, err := s.invoices.Get(ctx, q, invoiceID)
	if err != nil {
		return nil, auction.Lot{}, err
	}
	if !actor.Operator && inv.IssuerID != actor.OrganizationID {
		// Someone else's invoice: reporting "not found" avoids confirming it exists.
		return nil, auction.Lot{}, apperr.NotFoundf("invoice %s", invoiceID)
	}
	if inv.Status != invoice.StatusTokenized {
		return nil, auction.Lot{}, apperr.Conflictf(
			"invoice %s is %s; only a tokenized invoice can be auctioned", inv.ID, inv.Status)
	}

	assessment, err := s.assessments.Latest(ctx, q, inv.ID)
	if err != nil {
		return nil, auction.Lot{}, err
	}
	asset, err := s.assets.GetByInvoice(ctx, q, inv.ID)
	if err != nil {
		return nil, auction.Lot{}, err
	}
	if !asset.IsTransferable() {
		return nil, auction.Lot{}, apperr.Conflictf(
			"asset %s is %s and cannot be sold", asset.ID, asset.ChainStatus)
	}

	// The tenor is measured from today, not from the assessment: an invoice that sat in the
	// queue is closer to maturity, and the yield an investor sees has to say so.
	tenor := inv.DaysToDue(s.now())
	if tenor <= 0 {
		return nil, auction.Lot{}, apperr.Conflictf("invoice %s is already due", inv.ID)
	}

	lot := auction.Lot{
		ID:           s.ids(),
		InvoiceID:    inv.ID,
		AssetID:      asset.ID,
		IssuerID:     inv.IssuerID,
		DebtorRef:    inv.DebtorRef,
		Supply:       inv.Face,
		ReservePrice: assessment.ReservePrice,
		Grade:        assessment.Grade,
		TenorDays:    tenor,
	}
	if err := lot.Validate(); err != nil {
		return nil, auction.Lot{}, err
	}
	return inv, lot, nil
}

func uniqueIDs(ids []uuid.UUID) map[uuid.UUID]struct{} {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	return seen
}
