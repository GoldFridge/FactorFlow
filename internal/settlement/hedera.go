package settlement

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

/*
Chain is what a network has to offer for a transfer to be real.

Two calls, because the saga needs two different things from two different places: a
submission that says what was sent, and a reading that says what the network agreed to.
Collapsing them into one would leave the platform confirming its own transfers, which is not
a confirmation.
*/
type Chain interface {
	// Transfer moves an amount of a token to an account and returns the transaction it was
	// submitted as.
	Transfer(ctx context.Context, tokenID, to string, amount int64) (ChainReceipt, error)
	// Lookup asks an independent reader what became of a transaction.
	Lookup(ctx context.Context, transactionID string) (ChainRecord, error)
	// Network names the chain, for the record kept beside a transfer.
	Network() string
}

// ChainReceipt is what a submission returns.
type ChainReceipt struct {
	TransactionID string
	Status        string
	ExplorerURL   string
}

// ChainRecord is what the independent reader knows.
type ChainRecord struct {
	TransactionID string
	Found         bool
	Succeeded     bool
	Status        string
	ConfirmedAt   time.Time
	// Credited reports whether this transaction actually moved the amount that was planned
	// to the account it was planned for.
	Credited func(tokenID, account string, amount int64) bool
}

/*
ChainExecutor performs settlement transfers on a real network.

The interesting part is what it does not do. Submitting a transfer can fail in a way that
leaves the platform not knowing whether the transfer happened — a timeout after the
transaction reached a node is exactly that — and the only safe response is to record the
transaction id and ask later. So Submit never retries, and Lookup never guesses: an unknown
transaction stays unknown until a mirror says otherwise, because the alternative is sending
a second transfer for one allocation.
*/
type ChainExecutor struct {
	chain Chain
}

// NewChainExecutor returns the executor.
func NewChainExecutor(chain Chain) *ChainExecutor { return &ChainExecutor{chain: chain} }

// Submit sends the transfer at the network.
func (e *ChainExecutor) Submit(ctx context.Context, order Order) (Receipt, error) {
	if err := order.validate(); err != nil {
		return Receipt{}, err
	}
	if strings.TrimSpace(order.TokenID) == "" {
		return Receipt{}, apperr.Invalid("token_id",
			"a chain transfer needs the token the asset was minted as")
	}

	receipt, err := e.chain.Transfer(ctx, order.TokenID, order.ToWallet, order.Notional.Minor())
	if err != nil {
		// The transaction id is carried out with the error when there is one: a submission
		// whose id was lost is the single failure nothing later can recover from, because no
		// step could ask the network what became of it.
		if receipt.TransactionID != "" {
			return Receipt{TxID: receipt.TransactionID, SubmittedAt: time.Now().UTC()},
				fmt.Errorf("the transfer was submitted as %s and did not confirm: %w",
					receipt.TransactionID, err)
		}
		return Receipt{}, fmt.Errorf("submitting the transfer: %w", err)
	}

	return Receipt{
		TxID:        receipt.TransactionID,
		ExplorerURL: receipt.ExplorerURL,
		SubmittedAt: time.Now().UTC(),
	}, nil
}

// Lookup asks the independent reader what happened.
func (e *ChainExecutor) Lookup(ctx context.Context, txID string) (Record, error) {
	record, err := e.chain.Lookup(ctx, txID)
	if err != nil {
		return Record{}, err
	}

	return Record{
		TxID:        record.TransactionID,
		Found:       record.Found,
		Succeeded:   record.Succeeded,
		Status:      record.Status,
		ConfirmedAt: record.ConfirmedAt,
		Credited:    record.Credited,
	}, nil
}

// Network names the chain these transfers happen on.
func (e *ChainExecutor) Network() string { return e.chain.Network() }
