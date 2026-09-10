package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// Order is a transfer to perform, as the chain adapter sees it.
//
// It carries the operation id rather than the settlement's database id: the executor's
// only requirement is that the same order, submitted twice, is recognisably the same work.
type Order struct {
	OperationID string
	AssetID     string
	Network     string
	TokenID     string
	FromWallet  string
	ToWallet    string
	Notional    money.Amount
}

// Receipt is what submission returns.
type Receipt struct {
	TxID string
	// Duplicate reports that the executor recognised this operation as one it had already
	// performed and did not perform it again. The saga treats that as success, because the
	// transfer exists — which is the outcome it wanted.
	Duplicate bool
	// ExplorerURL is a human-readable link, empty when the network has no explorer.
	ExplorerURL string
	SubmittedAt time.Time
}

// Record is an independent read of what the chain holds, used to confirm a transfer
// rather than to trust the submission that claimed it.
type Record struct {
	TxID string
	// Found reports whether the transaction exists at all. A submitted transaction that
	// cannot be found is not a failure yet; it may simply not have propagated.
	Found bool
	// Succeeded reports the chain's own verdict on the transaction.
	Succeeded bool
	// Status is the chain's status string, kept verbatim for an operator.
	Status      string
	ConfirmedAt time.Time
	// Credited reports whether this transaction actually moved an amount of a token to an
	// account. It is optional — a reader that cannot say leaves it nil — and where it is
	// available it turns confirmation from "a transaction with this id succeeded" into
	// "this transaction delivered what was planned, to whom it was planned for".
	Credited func(tokenID, account string, amount int64) bool
}

// Executor performs transfers on whatever holds the asset.
//
// It is split into two calls on purpose. Submit pushes a transaction and can leave the
// system not knowing what happened; Lookup asks the chain what did happen. The saga never
// resubmits to find out, because that is how one allocation becomes two transfers.
type Executor interface {
	Submit(ctx context.Context, order Order) (Receipt, error)
	Lookup(ctx context.Context, txID string) (Record, error)
}

// LocalExecutor is an in-process executor for development and tests.
//
// It behaves the way the real one must: the same operation id submitted twice returns the
// first transaction rather than a second transfer, and a transaction can be looked up
// afterwards. It is deterministic, so a demo produces the same identifiers every run.
type LocalExecutor struct {
	mu sync.Mutex
	// byOperation maps an operation id to the transaction that performed it, which is what
	// makes a resubmission idempotent.
	byOperation map[string]string
	records     map[string]Record

	now func() time.Time

	// failSubmit and failLookup let a test drive the failure paths the saga exists for.
	failSubmit error
	failLookup error
	// pending withholds confirmation, standing in for a transaction that has not yet
	// propagated to an independent reader.
	pending bool
}

// NewLocalExecutor returns the in-process executor.
func NewLocalExecutor(now func() time.Time) *LocalExecutor {
	if now == nil {
		now = time.Now
	}
	return &LocalExecutor{
		byOperation: map[string]string{},
		records:     map[string]Record{},
		now:         now,
	}
}

// LocalNetwork names the in-process ledger, so a record made without a chain is never
// mistaken for one made on a real network.
const LocalNetwork = "local"

// Submit performs the transfer, or reports the one it already performed.
func (e *LocalExecutor) Submit(_ context.Context, order Order) (Receipt, error) {
	if err := order.validate(); err != nil {
		return Receipt{}, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.failSubmit != nil {
		return Receipt{}, e.failSubmit
	}
	if txID, exists := e.byOperation[order.OperationID]; exists {
		return Receipt{TxID: txID, Duplicate: true, SubmittedAt: e.records[txID].ConfirmedAt}, nil
	}

	txID := localTxID(order.OperationID)
	e.byOperation[order.OperationID] = txID
	e.records[txID] = Record{
		TxID:        txID,
		Found:       !e.pending,
		Succeeded:   !e.pending,
		Status:      "SUCCESS",
		ConfirmedAt: e.now().UTC(),
	}

	return Receipt{TxID: txID, SubmittedAt: e.now().UTC()}, nil
}

// Lookup reports what the local ledger holds for a transaction.
func (e *LocalExecutor) Lookup(_ context.Context, txID string) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.failLookup != nil {
		return Record{}, e.failLookup
	}

	record, ok := e.records[txID]
	if !ok {
		// Not an error: a transaction nobody has heard of may simply not have propagated,
		// and the saga decides how long to keep asking.
		return Record{TxID: txID}, nil
	}
	return record, nil
}

// FailSubmissions makes every later submission fail with err. Passing nil restores normal
// behaviour.
func (e *LocalExecutor) FailSubmissions(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.failSubmit = err
}

// FailLookups makes every later lookup fail with err.
func (e *LocalExecutor) FailLookups(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.failLookup = err
}

// WithholdConfirmation makes later submissions land as transactions that cannot yet be
// confirmed, standing in for a chain that has accepted a transaction but not surfaced it.
func (e *LocalExecutor) WithholdConfirmation(pending bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.pending = pending
}

// Confirm makes a withheld transaction visible, as propagation eventually would.
func (e *LocalExecutor) Confirm(txID string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	record, ok := e.records[txID]
	if !ok {
		return
	}
	record.Found, record.Succeeded, record.Status = true, true, "SUCCESS"
	record.ConfirmedAt = e.now().UTC()
	e.records[txID] = record
}

// Reject marks a transaction as one the chain refused, which is what the saga has to
// distinguish from a transaction it simply cannot see yet.
func (e *LocalExecutor) Reject(txID, status string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.records[txID] = Record{
		TxID: txID, Found: true, Succeeded: false,
		Status: status, ConfirmedAt: e.now().UTC(),
	}
}

// SubmissionCount reports how many distinct transfers were performed, which is how a test
// proves that a redelivery did not move the asset twice.
func (e *LocalExecutor) SubmissionCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return len(e.byOperation)
}

func (o Order) validate() error {
	switch {
	case strings.TrimSpace(o.OperationID) == "":
		return apperr.Invalid("operation_id", "must not be empty")
	case strings.TrimSpace(o.FromWallet) == "":
		return apperr.Invalid("from_wallet", "must not be empty")
	case strings.TrimSpace(o.ToWallet) == "":
		return apperr.Invalid("to_wallet", "must not be empty")
	case !o.Notional.IsValid() || !o.Notional.IsPositive():
		return apperr.Invalid("notional", "must be a positive amount")
	}
	return nil
}

// localTxID derives a transaction id from the operation, so the in-process ledger produces
// the same identifiers on every run of the same demo.
func localTxID(operationID string) string {
	sum := sha256.Sum256([]byte("local-tx|" + operationID))
	return localTxPrefix + hex.EncodeToString(sum[:16])
}

// localTxPrefix marks a transaction this process performed itself, so a later reader knows
// which executor to ask about it.
const localTxPrefix = "local-"
