// Package collections is what happens when the receivable comes due.
//
// Everything before this point is a promise: a document was priced, a batch cleared, a
// token moved. Here the debtor either pays or does not, and the platform has to say which
// — to the issuer who sold the paper and to the investors who bought it, from the same
// record, in the same numbers.
//
// It spans modules because maturity does: what the debtor owes is the invoice's, who holds
// the paper is settlement's, and dividing the money is redemption's. The rule that ties
// them together — money is divided among the transfers that actually completed, and the
// unsold remainder stays with the issuer — belongs to none of them alone.
package collections

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/redemption"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

// Audit actions this package records.
const (
	ActionRepaid    = "invoice.repaid"
	ActionDefaulted = "invoice.defaulted"
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

// Service records what the debtor did and divides it.
type Service struct {
	db          TxRunner
	invoices    invoice.Repository
	settlements settlement.Repository
	repayments  redemption.Repository
	audit       audit.Recorder
	ids         func() uuid.UUID
	now         func() time.Time
}

// Config wires the service.
type Config struct {
	DB          TxRunner
	Invoices    invoice.Repository
	Settlements settlement.Repository
	Repayments  redemption.Repository
	Audit       audit.Recorder
	IDs         func() uuid.UUID
	Now         func() time.Time
}

// NewService returns the service.
func NewService(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IDs == nil {
		cfg.IDs = uuid.New
	}
	if cfg.Audit == nil {
		cfg.Audit = audit.Discard{}
	}
	return &Service{
		db:          cfg.DB,
		invoices:    cfg.Invoices,
		settlements: cfg.Settlements,
		repayments:  cfg.Repayments,
		audit:       cfg.Audit,
		ids:         cfg.IDs,
		now:         cfg.Now,
	}
}

// RecordParams is a payment that arrived.
type RecordParams struct {
	InvoiceID uuid.UUID
	// Amount is what the debtor actually paid, which is not always what was owed.
	Amount     money.Amount
	Reference  string
	ReceivedAt time.Time
}

// Result is the repayment and the receivable it closed.
type Result struct {
	Repayment *redemption.Repayment
	Invoice   *invoice.Invoice
}

/*
Record credits a debtor's payment and divides it among the holders.

Only an operator may record it, because in factoring the debtor pays the platform rather
than the issuer that sold the receivable: letting the seller declare that the money arrived
would let it decide when its own obligation ended.

Recording the payment, dividing it, closing the receivable and writing the audit line all
commit together. A division stored without the invoice moving would pay holders for a
receivable the venue still shows as outstanding, and the reverse would close a receivable
nobody was paid for.
*/
func (s *Service) Record(ctx context.Context, actor Actor, p RecordParams) (*Result, error) {
	if !actor.Operator {
		return nil, apperr.Forbiddenf("only an operator records what the debtor paid")
	}

	var result Result
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := s.invoices.Get(ctx, q, p.InvoiceID)
		if err != nil {
			return err
		}

		// A repayment already on record is the answer, not a reason to record a second
		// one: a retried request must not credit every holder twice.
		existing, err := s.repayments.GetByInvoice(ctx, q, inv.ID)
		switch {
		case err == nil:
			if !existing.Amount.Equal(p.Amount) || existing.Reference != p.Reference {
				return apperr.Conflictf(
					"invoice %s was already repaid with %s under reference %q",
					inv.ID, existing.Amount, existing.Reference)
			}
			result = Result{Repayment: existing, Invoice: inv}
			return nil
		case !apperr.IsNotFound(err):
			return err
		}

		if inv.Status != invoice.StatusSettled {
			return apperr.Conflictf(
				"invoice %s is %s; only a settled receivable can be repaid", inv.ID, inv.Status)
		}

		holders, err := s.holdersOf(ctx, q, inv)
		if err != nil {
			return err
		}

		repayment, err := redemption.New(redemption.NewParams{
			ID:         s.ids(),
			InvoiceID:  inv.ID,
			Face:       inv.Face,
			Amount:     p.Amount,
			Reference:  p.Reference,
			ReceivedAt: p.ReceivedAt,
			RecordedBy: actor.OrganizationID,
			Holders:    holders,
		}, s.now())
		if err != nil {
			return err
		}
		if err := s.repayments.Create(ctx, q, repayment); err != nil {
			return err
		}

		before := map[string]any{"status": inv.Status.String(), "version": inv.Version}
		expectedVersion := inv.Version

		// A receivable the debtor paid short did not come good, whatever arrived. Saying so
		// is the point of recording it at all: an investor reads this to know what its
		// position actually returned.
		if repayment.IsShortfall() {
			err = inv.MarkDefaulted("the debtor paid "+repayment.Amount.String()+
				" of "+repayment.Face.String(), s.now())
		} else {
			err = inv.MarkMatured(s.now())
		}
		if err != nil {
			return err
		}
		if err := s.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}

		if err := s.audit.Record(ctx, q,
			audit.Of(ctx, actor.OrganizationID.String(), ActionRepaid,
				invoice.EntityType, inv.ID.String(), s.now()).
				Between(before, map[string]any{"status": inv.Status.String(), "version": inv.Version}).
				With("amount", repayment.Amount.String()).
				With("face", repayment.Face.String()).
				With("shortfall", repayment.Shortfall().String()).
				With("reference", repayment.Reference).
				With("holders", len(repayment.Shares))); err != nil {
			return err
		}

		result = Result{Repayment: repayment, Invoice: inv}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

/*
Default closes a receivable the debtor never paid.

It refuses before the due date. A receivable is not in default because somebody is
impatient, and the date it was due is on the invoice for exactly this reason.
*/
func (s *Service) Default(ctx context.Context, actor Actor, invoiceID uuid.UUID, reason string) (*invoice.Invoice, error) {
	if !actor.Operator {
		return nil, apperr.Forbiddenf("only an operator declares a receivable in default")
	}

	var closed *invoice.Invoice
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := s.invoices.Get(ctx, q, invoiceID)
		if err != nil {
			return err
		}
		if inv.Status != invoice.StatusSettled {
			return apperr.Conflictf(
				"invoice %s is %s; only a settled receivable can default", inv.ID, inv.Status)
		}
		if !inv.IsOverdue(s.now()) {
			return apperr.Conflictf("invoice %s is not due until %s",
				inv.ID, inv.DueAt.Format(time.RFC3339))
		}

		before := map[string]any{"status": inv.Status.String(), "version": inv.Version}
		expectedVersion := inv.Version
		if err := inv.MarkDefaulted(reason, s.now()); err != nil {
			return err
		}
		if err := s.invoices.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}

		if err := s.audit.Record(ctx, q,
			audit.Of(ctx, actor.OrganizationID.String(), ActionDefaulted,
				invoice.EntityType, inv.ID.String(), s.now()).
				Between(before, map[string]any{"status": inv.Status.String(), "version": inv.Version}).
				With("reason", inv.Reason).
				With("due_at", inv.DueAt.Format(time.RFC3339))); err != nil {
			return err
		}

		closed = inv
		return nil
	})
	if err != nil {
		return nil, err
	}
	return closed, nil
}

/*
Get returns a repayment to somebody entitled to read it.

That is the issuer who sold the receivable, an operator, and any party the payment was
divided among. Everyone else is told it does not exist, for the same reason the invoice
itself refuses: confirming that a receivable was repaid says something about it.
*/
func (s *Service) Get(ctx context.Context, actor Actor, invoiceID uuid.UUID) (*redemption.Repayment, error) {
	repayment, err := s.repayments.GetByInvoice(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}
	if actor.Operator || repayment.Holds(actor.OrganizationID) {
		return repayment, nil
	}

	inv, err := s.invoices.Get(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}
	if inv.IssuerID == actor.OrganizationID {
		return repayment, nil
	}
	return nil, apperr.NotFoundf("repayment of invoice %s", invoiceID)
}

// Holding is one position an investor bought and what has become of it.
//
// It carries the receivable's own terms as well as the transfer, because "what do I hold"
// is not answerable by either alone: the transfer says how much and at what price, and the
// receivable says who owes it, when, and whether the debtor has since paid.
type Holding struct {
	Settlement *settlement.Settlement
	Invoice    *invoice.Invoice
	// Repayment is the payment this position was paid from, absent until the debtor pays.
	Repayment *redemption.Repayment
}

/*
Holdings returns what an investor bought, newest first.

A transfer that has not finished is included and says so. Hiding it would leave an investor
whose settlement is stuck looking at a venue that has forgotten the money it took, which is
the worst possible moment to be told nothing.
*/
func (s *Service) Holdings(ctx context.Context, actor Actor, limit int) ([]*Holding, error) {
	if actor.OrganizationID == uuid.Nil {
		return nil, apperr.Forbiddenf("an organization is required to list holdings")
	}

	transfers, err := s.settlements.ListByInvestor(ctx, s.db.Querier(), actor.OrganizationID, limit)
	if err != nil {
		return nil, err
	}

	out := make([]*Holding, 0, len(transfers))
	for _, transfer := range transfers {
		inv, err := s.invoices.Get(ctx, s.db.Querier(), transfer.InvoiceID)
		if err != nil {
			if apperr.IsNotFound(err) {
				// The receivable is gone from under a transfer that names it. That is a
				// broken invariant rather than a holding, and dropping the row quietly
				// would be the platform hiding it from the one person it costs.
				return nil, apperr.Conflictf(
					"settlement %s names invoice %s, which no longer exists",
					transfer.ID, transfer.InvoiceID)
			}
			return nil, err
		}

		holding := &Holding{Settlement: transfer, Invoice: inv}

		repayment, err := s.repayments.GetByInvoice(ctx, s.db.Querier(), inv.ID)
		switch {
		case err == nil:
			holding.Repayment = repayment
		case !apperr.IsNotFound(err):
			return nil, err
		}

		out = append(out, holding)
	}
	return out, nil
}

// Received returns the repayments a party has a share in, newest first.
func (s *Service) Received(ctx context.Context, actor Actor, limit int) ([]*redemption.Repayment, error) {
	if actor.OrganizationID == uuid.Nil {
		return nil, apperr.Forbiddenf("an organization is required to list repayments")
	}
	return s.repayments.ListForParty(ctx, s.db.Querier(), actor.OrganizationID, limit)
}

/*
holdersOf works out who is owed part of what the debtor paid.

Only a settlement that finished makes anybody a holder: a transfer still in flight, or one
that failed, moved nothing, and paying against it would hand money to an investor who never
received the asset. Whatever notional was never sold stays with the issuer, which is what
keeps the division adding up to the whole receivable rather than to the part that sold.
*/
func (s *Service) holdersOf(ctx context.Context, q postgres.Querier, inv *invoice.Invoice) ([]redemption.Holder, error) {
	transfers, err := s.settlements.ListByInvoice(ctx, q, inv.ID)
	if err != nil {
		return nil, err
	}

	held := map[uuid.UUID]money.Amount{}
	sold := money.Zero(inv.Face.Currency())

	for _, transfer := range transfers {
		if !transfer.IsFinished() {
			continue
		}
		current, seen := held[transfer.InvestorID]
		if !seen {
			current = money.Zero(inv.Face.Currency())
		}
		// One investor can win several allocations of the same receivable; they are one
		// holder with one share, not two claims on the whole.
		if current, err = current.Add(transfer.Notional); err != nil {
			return nil, err
		}
		held[transfer.InvestorID] = current

		if sold, err = sold.Add(transfer.Notional); err != nil {
			return nil, err
		}
	}

	retained, err := inv.Face.Sub(sold)
	if err != nil {
		return nil, err
	}
	if retained.IsNegative() {
		return nil, apperr.Conflictf(
			"invoice %s has %s settled against a face of %s", inv.ID, sold, inv.Face)
	}
	if retained.IsPositive() {
		current, seen := held[inv.IssuerID]
		if !seen {
			current = money.Zero(inv.Face.Currency())
		}
		if current, err = current.Add(retained); err != nil {
			return nil, err
		}
		held[inv.IssuerID] = current
	}

	holders := make([]redemption.Holder, 0, len(held))
	for partyID, notional := range held {
		holders = append(holders, redemption.Holder{PartyID: partyID, Notional: notional})
	}
	// A map has no order and the division must not depend on one, so the holders are
	// sorted before they are handed over.
	sort.Slice(holders, func(i, j int) bool {
		return holders[i].PartyID.String() < holders[j].PartyID.String()
	})
	return holders, nil
}
