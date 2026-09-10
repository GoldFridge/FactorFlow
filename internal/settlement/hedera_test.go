package settlement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

// fakeChain is a network that answers exactly what a test tells it to.
type fakeChain struct {
	submitted int
	receipt   settlement.ChainReceipt
	submitErr error

	record    settlement.ChainRecord
	lookupErr error
}

func (f *fakeChain) Network() string { return "testnet" }

func (f *fakeChain) Transfer(_ context.Context, _, _ string, _ int64) (settlement.ChainReceipt, error) {
	f.submitted++
	return f.receipt, f.submitErr
}

func (f *fakeChain) Lookup(_ context.Context, _ string) (settlement.ChainRecord, error) {
	return f.record, f.lookupErr
}

func chainOrder() settlement.Order {
	return settlement.Order{
		OperationID: "settlement:9f0c",
		AssetID:     "0f9c2b7e-2f0a-4a1d-9c1e-7d3b5a6c8e10",
		Network:     "testnet",
		TokenID:     "0.0.777",
		FromWallet:  "0.0.1234",
		ToWallet:    "0.0.5678",
		Notional:    money.MustParse("10000.00", money.USD),
	}
}

// TestAChainTransferIsSubmittedOnce covers the ordinary path and the identifier the whole
// saga hangs on.
func TestAChainTransferIsSubmittedOnce(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{receipt: settlement.ChainReceipt{
		TransactionID: "0.0.1234@1699999999.123456789",
		Status:        "SUCCESS",
		ExplorerURL:   "https://hashscan.io/testnet/transaction/0.0.1234@1699999999.123456789",
	}}

	receipt, err := settlement.NewChainExecutor(chain).Submit(context.Background(), chainOrder())
	require.NoError(t, err)

	assert.Equal(t, 1, chain.submitted)
	assert.Equal(t, "0.0.1234@1699999999.123456789", receipt.TxID)
	assert.Contains(t, receipt.ExplorerURL, "hashscan.io")
}

/*
 * TestASubmissionThatFailedAfterReachingTheNetworkKeepsItsIdentifier is the failure this
 * design exists for. A timeout after the transaction reached a node leaves the platform not
 * knowing whether the transfer happened; the only thing that makes that recoverable is the
 * transaction id, and losing it means the only way to find out would be to transfer again.
 */
func TestASubmissionThatFailedAfterReachingTheNetworkKeepsItsIdentifier(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{
		receipt:   settlement.ChainReceipt{TransactionID: "0.0.1234@1699999999.123456789"},
		submitErr: errors.New("the receipt query timed out"),
	}

	receipt, err := settlement.NewChainExecutor(chain).Submit(context.Background(), chainOrder())

	require.Error(t, err)
	assert.Equal(t, "0.0.1234@1699999999.123456789", receipt.TxID,
		"the id comes back with the failure, because nothing later can work without it")
	assert.Contains(t, err.Error(), "did not confirm")
}

// TestAChainTransferNeedsAToken: a transfer with nowhere to move value from is a
// configuration mistake, and it is refused before anything reaches a network.
func TestAChainTransferNeedsAToken(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{}
	order := chainOrder()
	order.TokenID = "  "

	_, err := settlement.NewChainExecutor(chain).Submit(context.Background(), order)

	require.Error(t, err)
	assert.Zero(t, chain.submitted, "nothing was sent")
}

/*
 * TestLookupCarriesWhatMoved. The saga's second confirmation is only worth having if it can
 * say more than "a transaction with this id succeeded" — so the record carries the means to
 * ask whether this transaction delivered this notional to this buyer.
 */
func TestLookupCarriesWhatMoved(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{record: settlement.ChainRecord{
		TransactionID: "0.0.1234@1699999999.123456789",
		Found:         true,
		Succeeded:     true,
		Status:        "SUCCESS",
		ConfirmedAt:   time.Unix(1699999999, 0).UTC(),
		Credited: func(tokenID, account string, amount int64) bool {
			return tokenID == "0.0.777" && account == "0.0.5678" && amount == 1000000
		},
	}}

	record, err := settlement.NewChainExecutor(chain).
		Lookup(context.Background(), "0.0.1234@1699999999.123456789")
	require.NoError(t, err)

	require.NotNil(t, record.Credited)
	assert.True(t, record.Credited("0.0.777", "0.0.5678", 1000000))
	assert.False(t, record.Credited("0.0.777", "0.0.9999", 1000000),
		"a transfer to somebody else is not this transfer")
}
