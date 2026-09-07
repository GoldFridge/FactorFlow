package marketplace

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

// TopicSettle asks the settlement worker to advance one transfer.
const TopicSettle = "settlement.execute"

// Audit actions the settlement path records.
const (
	ActionSettlementPlanned = "auction.settlement_planned"
	ActionSettled           = "auction.settled"
	ActionInvoiceSettled    = "invoice.settled"
)

// OrganizationWallets answers the one question settlement has about an organization: which
// wallet it acts through. It is an adapter rather than an import so this package does not
// depend on the whole organization module for a single string.
type OrganizationWallets interface {
	WalletOf(ctx context.Context, q postgres.Querier, organizationID uuid.UUID) (string, error)
}

// settleCommand names the transfer to advance. It carries the settlement id and nothing
// else: the worker reads the plan from storage, so a command that waited in the queue
// cannot carry a stale amount onto the chain.
type settleCommand struct {
	SettlementID uuid.UUID `json:"settlement_id"`
}

// SettleAuction plans the transfers a cleared batch requires and queues them.
//
// Planning and queueing commit together, so a batch can never be left in SETTLING with
// nothing on its way to move it. The transfers themselves happen outside this transaction,
// because a chain cannot join it — that is what the saga in the settlement module is for.
//
// Calling this twice is safe: the second call finds the plans that exist and re-queues
// them rather than planning a second set of transfers.
func (s *Service) SettleAuction(ctx context.Context, actor Actor, auctionID uuid.UUID, traceID string) ([]*settlement.Settlement, error) {
	if s.settlements == nil {
		return nil, apperr.Unavailablef("this deployment cannot settle: no settlement store is configured")
	}

	var planned []*settlement.Settlement
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		a, err := s.auctions.GetAuction(ctx, q, auctionID)
		if err != nil {
			return err
		}
		if !actor.Operator && a.IssuerID != actor.OrganizationID {
			return apperr.Forbiddenf("auction %s belongs to another issuer", auctionID)
		}

		existing, err := s.settlements.ListByAuction(ctx, q, a.ID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			// Already planned. Re-queue whatever has not finished, so a repeated settle
			// command resumes the saga instead of starting a second one.
			planned = existing
			return s.queueSettlements(ctx, q, existing, traceID)
		}

		if a.Status != auction.StatusCleared {
			return apperr.Conflictf("auction %s is %s; only a cleared batch can be settled", a.ID, a.Status)
		}

		solution, err := s.auctions.GetSolution(ctx, q, a.ID)
		if err != nil {
			return err
		}
		if len(solution.Allocations) == 0 {
			return apperr.Conflictf("auction %s allocated nothing; there is nothing to settle", a.ID)
		}

		issuerWallet, err := s.wallets.WalletOf(ctx, q, a.IssuerID)
		if err != nil {
			return err
		}

		for _, allocation := range solution.Allocations {
			plan, err := s.planTransfer(ctx, q, a, allocation, issuerWallet)
			if err != nil {
				return err
			}
			if err := s.settlements.Create(ctx, q, plan); err != nil {
				return err
			}
			planned = append(planned, plan)
		}

		expectedVersion := a.Version
		if err := a.StartSettling(s.now()); err != nil {
			return err
		}
		if err := s.auctions.UpdateAuction(ctx, q, a, expectedVersion); err != nil {
			return err
		}

		if err := s.audit.Record(ctx, q,
			audit.Of(ctx, actorRef(actor), ActionSettlementPlanned, auction.EntityType, a.ID.String(), s.now()).
				With("transfers", len(planned)).
				With("status", a.Status.String())); err != nil {
			return err
		}

		return s.queueSettlements(ctx, q, planned, traceID)
	})
	if err != nil {
		return nil, err
	}
	return planned, nil
}

// planTransfer turns one allocation into a transfer from the issuer to the buyer.
func (s *Service) planTransfer(ctx context.Context, q postgres.Querier, a *auction.Auction, allocation auction.Allocation, issuerWallet string) (*settlement.Settlement, error) {
	investorWallet, err := s.wallets.WalletOf(ctx, q, allocation.InvestorID)
	if err != nil {
		return nil, err
	}

	return settlement.New(settlement.NewParams{
		ID:         s.ids(),
		AuctionID:  a.ID,
		LotID:      allocation.LotID,
		BidID:      allocation.BidID,
		InvoiceID:  allocation.InvoiceID,
		AssetID:    allocation.AssetID,
		InvestorID: allocation.InvestorID,
		FromWallet: issuerWallet,
		ToWallet:   investorWallet,
		Notional:   allocation.Notional,
		Price:      allocation.Price,
	}, s.now())
}

// queueSettlements publishes one command per unfinished transfer.
func (s *Service) queueSettlements(ctx context.Context, q postgres.Querier, plans []*settlement.Settlement, traceID string) error {
	for _, plan := range plans {
		if plan.IsFinished() {
			continue
		}
		if err := outbox.Publish(ctx, q, TopicSettle, settleCommand{SettlementID: plan.ID}, traceID, s.now()); err != nil {
			return err
		}
	}
	return nil
}

// Settlements returns a batch's transfers, for the issuer that ran it or an operator.
func (s *Service) Settlements(ctx context.Context, actor Actor, auctionID uuid.UUID) ([]*settlement.Settlement, error) {
	if s.settlements == nil {
		return nil, apperr.Unavailablef("this deployment cannot settle: no settlement store is configured")
	}

	a, err := s.auctions.GetAuction(ctx, s.db.Querier(), auctionID)
	if err != nil {
		return nil, err
	}
	if !actor.Operator && a.IssuerID != actor.OrganizationID {
		return nil, apperr.Forbiddenf("auction %s belongs to another issuer", auctionID)
	}
	return s.settlements.ListByAuction(ctx, s.db.Querier(), a.ID)
}

// finishInvoice moves a settled invoice and records it, inside the caller's transaction.
func (s *Service) finishInvoice(ctx context.Context, q postgres.Querier, plan *settlement.Settlement) error {
	inv, err := s.invoices.Get(ctx, q, plan.InvoiceID)
	if err != nil {
		return err
	}
	if inv.Status == invoice.StatusSettled {
		// A redelivery arriving after the invoice already moved. The transfer happened
		// once; saying so twice is not an error.
		return nil
	}

	expectedVersion := inv.Version
	if err := inv.MarkSettled(s.now()); err != nil {
		return fmt.Errorf("settling invoice %s: %w", inv.ID, err)
	}
	if err := s.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
		return err
	}

	return s.audit.Record(ctx, q,
		audit.Of(ctx, audit.SystemActor, ActionInvoiceSettled, invoice.EntityType, inv.ID.String(), s.now()).
			With("auction_id", plan.AuctionID.String()).
			With("tx_id", plan.TxID).
			With("status", inv.Status.String()))
}

// finishAuction closes the batch once every transfer it planned has been accounted for.
//
// The check is a read of the stored settlements rather than a counter: a counter would
// have to be kept correct across crashes, and the rows already are.
func (s *Service) finishAuction(ctx context.Context, q postgres.Querier, auctionID uuid.UUID) error {
	plans, err := s.settlements.ListByAuction(ctx, q, auctionID)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		if !plan.IsFinished() {
			return nil
		}
	}

	a, err := s.auctions.GetAuction(ctx, q, auctionID)
	if err != nil {
		return err
	}
	if a.Status == auction.StatusSettled {
		return nil
	}

	expectedVersion := a.Version
	if err := a.MarkSettled(s.now()); err != nil {
		return err
	}
	if err := s.auctions.UpdateAuction(ctx, q, a, expectedVersion); err != nil {
		return err
	}

	return s.audit.Record(ctx, q,
		audit.Of(ctx, audit.SystemActor, ActionSettled, auction.EntityType, a.ID.String(), s.now()).
			With("transfers", len(plans)).
			With("status", a.Status.String()))
}
