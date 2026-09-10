package settlement_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

/*
network stands in for a chain: it answers with transaction ids in the shape Hedera uses,
which is what lets a later reader tell its transfers from the ones this process performed
itself. A LocalExecutor cannot play this part — it names its transactions after itself.
*/
type network struct {
	submitted []settlement.Order
}

func (n *network) Submit(_ context.Context, order settlement.Order) (settlement.Receipt, error) {
	n.submitted = append(n.submitted, order)
	return settlement.Receipt{
		TxID:        fmt.Sprintf("0.0.10419676@178907459%d.473132052", len(n.submitted)),
		SubmittedAt: at()(),
	}, nil
}

func (n *network) Lookup(_ context.Context, txID string) (settlement.Record, error) {
	return settlement.Record{TxID: txID, Found: true, Succeeded: true, Status: "SUCCESS"}, nil
}

func at() func() time.Time {
	moment := time.Date(2026, time.September, 11, 9, 0, 0, 0, time.UTC)
	return func() time.Time { return moment }
}

func orderTo(destination string) settlement.Order {
	return settlement.Order{
		OperationID: "0x" + destination,
		AssetID:     "1c7094f4-2dbf-587e-8860-49c2f341e928",
		Network:     "testnet",
		TokenID:     "0.0.10500001",
		FromWallet:  "0.0.10419676",
		ToWallet:    destination,
		Notional:    money.MustParse("15000.00", money.USD),
	}
}

/*
TestATransferGoesWhereItCanArrive.

Configuring a chain says the platform can settle on one, not that every participant can
receive on one. A transfer to somebody with no account was submitted to the network anyway
and came back TOKEN_NOT_ASSOCIATED_TO_ACCOUNT: a real transaction, a real fee, and a
settlement no amount of retrying could finish, because the destination stored on it had
never been an account.
*/
func TestATransferGoesWhereItCanArrive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		destination string
		onChain     bool
	}{
		{"an account the platform opened", "0.0.10456604", true},
		{"a wallet address, which is not an account", "0x0000000000000000000000000000000000000b01", false},
		{"nothing at all where an account should be", "  ", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			chain, local := &network{}, settlement.NewLocalExecutor(at())
			executor := settlement.NewRoutedExecutor(chain, local)

			receipt, err := executor.Submit(context.Background(), orderTo(tc.destination))
			if tc.destination == "  " {
				require.Error(t, err, "an empty destination is refused before it is routed")
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, receipt.TxID)

			if tc.onChain {
				assert.Len(t, chain.submitted, 1, "the network should have performed it")
				assert.Equal(t, 0, local.SubmissionCount())
				return
			}
			assert.Empty(t, chain.submitted, "the network cannot deliver here")
			assert.Equal(t, 1, local.SubmissionCount())
		})
	}
}

/*
TestConfirmationAsksWhoeverPerformedIt. The saga confirms a transfer by reading it back, and
reading it from the wrong executor would report a settled transfer as one that never
happened — which the saga would treat as a transaction still propagating, forever.
*/
func TestConfirmationAsksWhoeverPerformedIt(t *testing.T) {
	t.Parallel()

	chain, local := &network{}, settlement.NewLocalExecutor(at())
	executor := settlement.NewRoutedExecutor(chain, local)

	offChain, err := executor.Submit(context.Background(), orderTo("0xb01"))
	require.NoError(t, err)
	onChain, err := executor.Submit(context.Background(), orderTo("0.0.10456604"))
	require.NoError(t, err)

	for _, txID := range []string{offChain.TxID, onChain.TxID} {
		record, err := executor.Lookup(context.Background(), txID)
		require.NoError(t, err)
		assert.True(t, record.Succeeded, "the transfer %s was performed and cannot be found", txID)
	}
}

// TestWhatCountsAsAnAccount states the rule on its own, because it is what decides whether
// real money moves.
func TestWhatCountsAsAnAccount(t *testing.T) {
	t.Parallel()

	for destination, deliverable := range map[string]bool{
		"0.0.10456604":   true,
		" 0.0.4402 ":     true,
		"0x73cfabefd7f2": false,
		"":               false,
		"0.0":            false,
		"northwind":      false,
	} {
		assert.Equal(t, deliverable, settlement.DeliverableOnChain(destination), "destination %q", destination)
	}
}
