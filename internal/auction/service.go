package auction

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// TxRunner is the transaction boundary the service needs.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// Actor is the authenticated caller, as far as this module is concerned.
//
// Eligible is resolved when the session is established, not here: whether an organization
// passed the demo eligibility check is the identity layer's question, and this module only
// needs the answer.
type Actor struct {
	OrganizationID uuid.UUID
	Eligible       bool
	Operator       bool
}

// Service is the application layer of the auction module.
//
// It owns who may do what and which writes happen together; the state machine, the
// feasibility rules and the clearing algorithm stay in the domain and the solver.
//
// Clearing is not here. It has to move the invoices behind the lots as well as the auction,
// and this module may not touch invoices, so the application layer runs it: one transaction
// covering both, instead of two that can disagree.
type Service struct {
	db     TxRunner
	repo   Repository
	solver *Solver
	now    func() time.Time
	ids    func() uuid.UUID
}

// NewService wires the service.
func NewService(db TxRunner, repo Repository, solver *Solver, now func() time.Time, ids func() uuid.UUID) *Service {
	if now == nil {
		now = time.Now
	}
	if ids == nil {
		ids = uuid.New
	}
	if solver == nil {
		solver = NewSolver()
	}
	return &Service{db: db, repo: repo, solver: solver, now: now, ids: ids}
}

// CreateParams carries the batch an issuer offers.
//
// Lots arrive already assembled: turning invoices and their assessments into lots needs
// both of those modules, so it belongs in the application layer rather than here.
type CreateParams struct {
	Lots     []Lot
	OpensAt  time.Time
	ClosesAt time.Time
}

// Create records a draft auction for the caller's organization.
func (s *Service) Create(ctx context.Context, actor Actor, p CreateParams) (*Auction, error) {
	if actor.OrganizationID == uuid.Nil {
		return nil, apperr.Forbiddenf("an organization is required to create an auction")
	}

	a, err := NewAuction(NewAuctionParams{
		ID:       s.ids(),
		IssuerID: actor.OrganizationID,
		Lots:     p.Lots,
		OpensAt:  p.OpensAt,
		ClosesAt: p.ClosesAt,
	}, s.now())
	if err != nil {
		return nil, err
	}

	// Every lot must belong to the issuer running the batch. Without this an issuer could
	// list someone else's receivable and collect the proceeds.
	for _, lot := range a.Lots {
		if lot.IssuerID != actor.OrganizationID {
			return nil, apperr.Forbiddenf("lot %s belongs to another issuer", lot.ID)
		}
	}

	if err := s.db.InTx(ctx, func(q postgres.Querier) error {
		return s.repo.CreateAuction(ctx, q, a)
	}); err != nil {
		return nil, err
	}
	return a, nil
}

// Open starts accepting bids.
func (s *Service) Open(ctx context.Context, actor Actor, auctionID uuid.UUID) (*Auction, error) {
	return s.mutate(ctx, actor, auctionID, func(a *Auction) error {
		return a.Open(s.now())
	})
}

// Cancel ends an auction without settling it.
func (s *Service) Cancel(ctx context.Context, actor Actor, auctionID uuid.UUID, reason string) (*Auction, error) {
	return s.mutate(ctx, actor, auctionID, func(a *Auction) error {
		return a.Cancel(reason, s.now())
	})
}

// BidParams is an investor's constrained offer.
type BidParams struct {
	Budget         money.Amount
	MinYield       money.Rate
	MaxGrade       risk.Grade
	MaxTenorDays   int64
	MinimumLot     money.Amount
	MaxIssuerShare money.Rate
	MaxDebtorShare money.Rate
	MaxGradeShare  map[risk.Grade]money.Rate
}

// PlaceBid records an investor's bid on an open auction.
func (s *Service) PlaceBid(ctx context.Context, actor Actor, auctionID uuid.UUID, p BidParams) (*Bid, error) {
	if actor.OrganizationID == uuid.Nil {
		return nil, apperr.Forbiddenf("an organization is required to bid")
	}
	if !actor.Eligible {
		return nil, apperr.Forbiddenf("organization %s has not passed the eligibility check", actor.OrganizationID)
	}

	var created *Bid
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		a, err := s.repo.GetAuction(ctx, q, auctionID)
		if err != nil {
			return err
		}
		// An issuer bidding on its own batch would be buying its own receivable, which is
		// either a mistake or an attempt to set the clearing price.
		if a.IssuerID == actor.OrganizationID {
			return apperr.Forbiddenf("an issuer cannot bid on its own auction")
		}
		if !a.IsAcceptingBids(s.now()) {
			return apperr.Conflictf("auction %s is not accepting bids", a.ID)
		}

		bid, err := NewBid(NewBidParams{
			ID:             s.ids(),
			AuctionID:      a.ID,
			InvestorID:     actor.OrganizationID,
			Budget:         p.Budget,
			MinYield:       p.MinYield,
			MaxGrade:       p.MaxGrade,
			MaxTenorDays:   p.MaxTenorDays,
			MinimumLot:     p.MinimumLot,
			MaxIssuerShare: p.MaxIssuerShare,
			MaxDebtorShare: p.MaxDebtorShare,
			MaxGradeShare:  p.MaxGradeShare,
		}, s.now())
		if err != nil {
			return err
		}
		if bid.Budget.Currency() != a.Currency() {
			return apperr.Invalid("budget", "must be in %s, the currency of this auction", a.Currency())
		}

		if err := s.repo.CreateBid(ctx, q, bid); err != nil {
			return err
		}
		created = bid
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// CancelBid withdraws an investor's own bid before clearing.
func (s *Service) CancelBid(ctx context.Context, actor Actor, bidID uuid.UUID) (*Bid, error) {
	var updated *Bid

	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		bid, err := s.repo.GetBid(ctx, q, bidID)
		if err != nil {
			return err
		}
		if !actor.Operator && bid.InvestorID != actor.OrganizationID {
			// Someone else's bid: reporting "not found" avoids confirming it exists.
			return apperr.NotFoundf("bid %s", bidID)
		}

		a, err := s.repo.GetAuction(ctx, q, bid.AuctionID)
		if err != nil {
			return err
		}
		if a.Status != StatusOpen {
			return apperr.Conflictf("auction %s is %s; bids can only be withdrawn while it is open", a.ID, a.Status)
		}

		expectedVersion := bid.Version
		if err := bid.Cancel(); err != nil {
			return err
		}
		if err := s.repo.UpdateBid(ctx, q, bid, expectedVersion); err != nil {
			return err
		}
		updated = bid
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Get returns one auction with its lots.
func (s *Service) Get(ctx context.Context, actor Actor, auctionID uuid.UUID) (*Auction, error) {
	if actor.OrganizationID == uuid.Nil && !actor.Operator {
		return nil, apperr.Forbiddenf("authentication is required to view an auction")
	}
	return s.repo.GetAuction(ctx, s.db.Querier(), auctionID)
}

// List returns the marketplace, optionally filtered by state.
func (s *Service) List(ctx context.Context, actor Actor, status Status, limit int) ([]*Auction, error) {
	if actor.OrganizationID == uuid.Nil && !actor.Operator {
		return nil, apperr.Forbiddenf("authentication is required to view auctions")
	}
	if status != "" && !status.IsValid() {
		return nil, apperr.Invalid("status", "unknown auction status %q", status)
	}
	return s.repo.ListAuctions(ctx, s.db.Querier(), status, limit)
}

// Bids returns the bids the caller may see: an investor sees its own, the issuer running
// the auction sees all of them.
func (s *Service) Bids(ctx context.Context, actor Actor, auctionID uuid.UUID) ([]*Bid, error) {
	a, err := s.repo.GetAuction(ctx, s.db.Querier(), auctionID)
	if err != nil {
		return nil, err
	}

	bids, err := s.repo.ListBids(ctx, s.db.Querier(), auctionID)
	if err != nil {
		return nil, err
	}
	if actor.Operator || a.IssuerID == actor.OrganizationID {
		return bids, nil
	}

	mine := make([]*Bid, 0, len(bids))
	for _, bid := range bids {
		if bid.InvestorID == actor.OrganizationID {
			mine = append(mine, bid)
		}
	}
	return mine, nil
}

// Solution returns a cleared auction's allocation, rejections and certificate.
//
// A cleared batch is public to every authenticated participant, and deliberately so: the
// certificate is only evidence if the people it affects can read it.
func (s *Service) Solution(ctx context.Context, actor Actor, auctionID uuid.UUID) (*Solution, error) {
	if actor.OrganizationID == uuid.Nil && !actor.Operator {
		return nil, apperr.Forbiddenf("authentication is required to view a clearing")
	}
	return s.repo.GetSolution(ctx, s.db.Querier(), auctionID)
}

// mutate loads an auction, applies an issuer command and stores the result.
func (s *Service) mutate(ctx context.Context, actor Actor, auctionID uuid.UUID, apply func(*Auction) error) (*Auction, error) {
	var updated *Auction

	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		a, err := s.load(ctx, q, actor, auctionID)
		if err != nil {
			return err
		}

		expectedVersion := a.Version
		if err := apply(a); err != nil {
			return err
		}
		if err := s.repo.UpdateAuction(ctx, q, a, expectedVersion); err != nil {
			return err
		}

		updated = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// load fetches an auction the caller is allowed to change.
func (s *Service) load(ctx context.Context, q postgres.Querier, actor Actor, auctionID uuid.UUID) (*Auction, error) {
	a, err := s.repo.GetAuction(ctx, q, auctionID)
	if err != nil {
		return nil, err
	}
	if actor.Operator || a.IssuerID == actor.OrganizationID {
		return a, nil
	}
	return nil, apperr.Forbiddenf("auction %s belongs to another issuer", auctionID)
}
