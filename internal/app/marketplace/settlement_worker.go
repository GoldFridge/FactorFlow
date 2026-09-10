package marketplace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/settlement"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

// SettlementWorker advances one transfer as far as it can go.
//
// Each step is stored before the next is attempted, so a crash resumes rather than
// restarts. The worker never asks "did I already do this?" of its own memory: it asks the
// stored state, and where that is not enough — a submission whose outcome is unknown — it
// asks the chain.
type SettlementWorker struct {
	service  *Service
	executor settlement.Executor
	assets   tokenization.Repository
}

// NewSettlementWorker returns the worker.
func NewSettlementWorker(service *Service, executor settlement.Executor, assets tokenization.Repository) *SettlementWorker {
	return &SettlementWorker{service: service, executor: executor, assets: assets}
}

// Handle advances the settlement named by the command.
//
// It returns nil for work that is already done, because the outbox may deliver the same
// command again and a redelivery is not a second transfer.
func (w *SettlementWorker) Handle(ctx context.Context, event outbox.Event) error {
	var command settleCommand
	if err := json.Unmarshal(event.Payload, &command); err != nil {
		return fmt.Errorf("decoding settle command: %w", err)
	}
	if command.SettlementID == uuid.Nil {
		return apperr.Invalid("settlement_id", "must be a non-nil UUID")
	}

	plan, err := w.service.settlements.Get(ctx, w.service.db.Querier(), command.SettlementID)
	if err != nil {
		return err
	}
	if plan.IsFinished() {
		return nil
	}

	// Each step commits on its own. Running the whole saga in one transaction would mean
	// holding it open across a chain call, and a submitted transfer would be forgotten if
	// that transaction then failed.
	for !plan.IsFinished() {
		next, err := w.advance(ctx, plan)
		if err != nil {
			return w.recordFailure(ctx, plan, err)
		}
		plan = next
	}
	return nil
}

// advance performs exactly one step and stores it.
func (w *SettlementWorker) advance(ctx context.Context, plan *settlement.Settlement) (*settlement.Settlement, error) {
	switch {
	case !plan.IsSubmitted():
		return w.submit(ctx, plan)
	case plan.State == settlement.StateSubmitted, plan.State == settlement.StateFailed:
		return w.confirmConsensus(ctx, plan)
	case plan.State == settlement.StateConsensusConfirmed:
		return w.confirmMirror(ctx, plan)
	case plan.State == settlement.StateMirrorConfirmed:
		return w.account(ctx, plan)
	default:
		return nil, apperr.Conflictf("settlement %s cannot be advanced from %s", plan.ID, plan.State)
	}
}

// submit pushes the transfer at the chain.
//
// The receipt is stored before anything else happens. A submission whose transaction id
// was lost is the one failure nothing later can recover from: no step could ask the chain
// what became of it, and the only way to find out would be to transfer again.
func (w *SettlementWorker) submit(ctx context.Context, plan *settlement.Settlement) (*settlement.Settlement, error) {
	asset, err := w.assets.Get(ctx, w.service.db.Querier(), plan.AssetID)
	if err != nil {
		return nil, err
	}

	receipt, err := w.executor.Submit(ctx, settlement.Order{
		OperationID: plan.OperationID,
		AssetID:     plan.AssetID.String(),
		Network:     asset.Network,
		TokenID:     asset.TokenID,
		FromWallet:  plan.FromWallet,
		ToWallet:    plan.ToWallet,
		Notional:    plan.Notional,
	})
	if err != nil {
		return nil, fmt.Errorf("submitting transfer: %w", err)
	}

	return w.store(ctx, plan, func(p *settlement.Settlement) error {
		return p.Submit(receipt.TxID, w.service.now())
	})
}

// confirmConsensus asks the chain what happened to the transaction, rather than assuming.
func (w *SettlementWorker) confirmConsensus(ctx context.Context, plan *settlement.Settlement) (*settlement.Settlement, error) {
	record, err := w.executor.Lookup(ctx, plan.TxID)
	if err != nil {
		return nil, fmt.Errorf("looking up transaction %s: %w", plan.TxID, err)
	}
	switch {
	case !record.Found:
		// Not yet visible. This is not a failure and must never be treated as one: the
		// transfer may be seconds from confirming, and resubmitting would move the asset
		// twice. The command is retried by the outbox instead.
		return nil, apperr.Unavailablef("transaction %s has not appeared yet", plan.TxID)
	case !record.Succeeded:
		return nil, fmt.Errorf("%w: the chain rejected transaction %s (%s)",
			apperr.ErrConflict, plan.TxID, record.Status)
	}

	return w.store(ctx, plan, func(p *settlement.Settlement) error {
		return p.ConfirmConsensus(w.service.now())
	})
}

// confirmMirror repeats the question against an independent reader.
//
// The two confirmations are separate on purpose: the node that accepted a transaction is
// not evidence that the network agrees it happened.
func (w *SettlementWorker) confirmMirror(ctx context.Context, plan *settlement.Settlement) (*settlement.Settlement, error) {
	record, err := w.executor.Lookup(ctx, plan.TxID)
	if err != nil {
		return nil, fmt.Errorf("confirming transaction %s: %w", plan.TxID, err)
	}
	if !record.Found || !record.Succeeded {
		return nil, apperr.Unavailablef("transaction %s is not confirmed by an independent read", plan.TxID)
	}

	// Where the reader can say what moved, it is asked. A transaction that succeeded is not
	// the same claim as a transaction that delivered this notional to this buyer, and the
	// difference is the whole reason a second, independent confirmation exists.
	if record.Credited != nil {
		asset, err := w.assets.Get(ctx, w.service.db.Querier(), plan.AssetID)
		if err != nil {
			return nil, err
		}
		if !record.Credited(asset.TokenID, plan.ToWallet, plan.Notional.Minor()) {
			return nil, fmt.Errorf(
				"%w: transaction %s succeeded but did not credit %s with %s of token %s",
				apperr.ErrConflict, plan.TxID, plan.ToWallet, plan.Notional, asset.TokenID)
		}
	}

	return w.store(ctx, plan, func(p *settlement.Settlement) error {
		return p.ConfirmMirror(w.service.now())
	})
}

// account brings the local records in line with what the chain now holds.
//
// The settlement, the invoice and — when it is the last transfer — the auction move in one
// transaction. Splitting them would leave an investor holding a confirmed transfer whose
// invoice still claims to be on sale.
func (w *SettlementWorker) account(ctx context.Context, plan *settlement.Settlement) (*settlement.Settlement, error) {
	var updated *settlement.Settlement

	err := w.service.db.InTx(ctx, func(q postgres.Querier) error {
		current, err := w.service.settlements.Get(ctx, q, plan.ID)
		if err != nil {
			return err
		}
		if current.IsFinished() {
			updated = current
			return nil
		}

		expectedVersion := current.Version
		if err := current.Account(w.service.now()); err != nil {
			return err
		}
		if err := w.service.settlements.Update(ctx, q, current, expectedVersion); err != nil {
			return err
		}
		if err := w.service.finishInvoice(ctx, q, current); err != nil {
			return err
		}
		if err := w.service.finishAuction(ctx, q, current.AuctionID); err != nil {
			return err
		}

		updated = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// store applies one step and writes it under the version it was read at.
func (w *SettlementWorker) store(ctx context.Context, plan *settlement.Settlement, apply func(*settlement.Settlement) error) (*settlement.Settlement, error) {
	var updated *settlement.Settlement

	err := w.service.db.InTx(ctx, func(q postgres.Querier) error {
		expectedVersion := plan.Version
		if err := apply(plan); err != nil {
			return err
		}
		if err := w.service.settlements.Update(ctx, q, plan, expectedVersion); err != nil {
			return err
		}

		updated = plan
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// recordFailure stores the cause and returns it, so the outbox retries and a repeated
// failure parks the command for an operator.
//
// The settlement is not rolled back: a transfer that reached the chain stays there, and
// the stored transaction id is what reconciliation will use to find out whether it landed.
func (w *SettlementWorker) recordFailure(ctx context.Context, plan *settlement.Settlement, cause error) error {
	err := w.service.db.InTx(ctx, func(q postgres.Querier) error {
		current, err := w.service.settlements.Get(ctx, q, plan.ID)
		if err != nil {
			return err
		}
		if current.IsFinished() {
			return nil
		}

		expectedVersion := current.Version
		if err := current.Fail(cause.Error(), w.service.now()); err != nil {
			return err
		}
		return w.service.settlements.Update(ctx, q, current, expectedVersion)
	})
	if err != nil {
		// Reporting the storage failure would hide the reason the transfer stopped, which
		// is the more useful of the two.
		return fmt.Errorf("%w (recording the failure also failed: %v)", cause, err)
	}
	return cause
}
