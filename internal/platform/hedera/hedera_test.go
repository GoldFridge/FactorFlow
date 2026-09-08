package hedera_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/hedera"
)

// A generated testnet key, used only to check that both encodings parse. It controls
// nothing: no account is derived from it here and it is not the operator of anything.
const (
	hexKey = "0xfb4937ffa2347d4e16d3f63274afd81f6d0af480b09707f4cdb1c701e6ff0110"
	bareID = "0.0.10419676"
)

func TestConnectValidatesItsCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  hedera.Config
	}{
		{name: "unknown network", cfg: hedera.Config{Network: "moonnet", AccountID: bareID, PrivateKey: hexKey}},
		{name: "malformed account", cfg: hedera.Config{Network: "testnet", AccountID: "not-an-account", PrivateKey: hexKey}},
		{name: "empty account", cfg: hedera.Config{Network: "testnet", PrivateKey: hexKey}},
		{name: "empty key", cfg: hedera.Config{Network: "testnet", AccountID: bareID}},
		{name: "malformed key", cfg: hedera.Config{Network: "testnet", AccountID: bareID, PrivateKey: "0xzz"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := hedera.Connect(tc.cfg)
			assert.ErrorIs(t, err, apperr.ErrValidation)
		})
	}
}

/*
 * A key arrives in one of two shapes depending on where it was copied from: the raw ECDSA
 * hex an EVM tool exports, or the DER string the Hedera portal shows. Rejecting either
 * would look like a wrong key rather than an unsupported encoding.
 */
func TestConnectAcceptsBothKeyEncodings(t *testing.T) {
	t.Parallel()

	for _, key := range []string{hexKey, hexKey[2:]} {
		client, err := hedera.Connect(hedera.Config{Network: "testnet", AccountID: bareID, PrivateKey: key})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })

		assert.Equal(t, bareID, client.Operator())
		assert.Equal(t, hedera.Testnet, client.Network())
	}
}

func TestNetworkDefaultsToTestnet(t *testing.T) {
	t.Parallel()

	client, err := hedera.Connect(hedera.Config{AccountID: bareID, PrivateKey: hexKey})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	assert.Equal(t, hedera.Testnet, client.Network())
}

func TestExplorerLinksPointAtTheRightNetwork(t *testing.T) {
	t.Parallel()

	client, err := hedera.Connect(hedera.Config{Network: "testnet", AccountID: bareID, PrivateKey: hexKey})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	assert.Equal(t, "https://hashscan.io/testnet/token/0.0.42", client.TokenURL("0.0.42"))
	assert.Contains(t, client.TransactionURL("0.0.1@169.0"), "/testnet/transaction/")
}

func TestCallsRefuseNonsenseBeforeReachingTheNetwork(t *testing.T) {
	t.Parallel()

	client, err := hedera.Connect(hedera.Config{Network: "testnet", AccountID: bareID, PrivateKey: hexKey})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	ctx := t.Context()
	_, err = client.CreateToken(ctx, hedera.Token{Name: "x", Symbol: "X", Supply: 0})
	assert.ErrorIs(t, err, apperr.ErrValidation)

	_, err = client.Transfer(ctx, "0.0.42", "0.0.7", 0)
	assert.ErrorIs(t, err, apperr.ErrValidation)

	_, err = client.Transfer(ctx, "not-a-token", "0.0.7", 1)
	assert.ErrorIs(t, err, apperr.ErrValidation)

	_, err = client.Transfer(ctx, "0.0.42", "", 1)
	assert.ErrorIs(t, err, apperr.ErrValidation)

	// A cancelled caller is not a reason to spend money on a network call.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.CreateToken(cancelled, hedera.Token{Name: "x", Symbol: "X", Supply: 1})
	assert.ErrorIs(t, err, context.Canceled)
}

/*
 * TestCreateTokenOnTheNetwork is the only test here that proves anything about Hedera.
 *
 * It runs against the real testnet and is skipped without credentials, the same bargain the
 * database tests make: a laptop with no configuration still runs the suite, and a machine
 * that can reach the network checks the thing that actually matters.
 */
func TestCreateTokenOnTheNetwork(t *testing.T) {
	account, key := os.Getenv("FF_HEDERA_ACCOUNT_ID"), os.Getenv("FF_HEDERA_PRIVATE_KEY")
	if testing.Short() || account == "" || key == "" {
		t.Skip("network test: needs FF_HEDERA_ACCOUNT_ID and FF_HEDERA_PRIVATE_KEY")
	}

	client, err := hedera.Connect(hedera.Config{
		Network:    os.Getenv("FF_HEDERA_NETWORK"),
		AccountID:  account,
		PrivateKey: key,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	receipt, err := client.CreateToken(ctx, hedera.Token{
		Name:     "FactorFlow test receivable",
		Symbol:   "FFTEST",
		Memo:     "0xdeadbeef",
		Decimals: 2,
		Supply:   1_500_000,
	})
	require.NoError(t, err)

	assert.Equal(t, "SUCCESS", receipt.Status)
	assert.Regexp(t, `^\d+\.\d+\.\d+$`, receipt.TokenID)
	assert.NotEmpty(t, receipt.TransactionID)
	assert.Contains(t, receipt.ExplorerURL, receipt.TokenID)

	t.Logf("minted %s on %s: %s", receipt.TokenID, client.Network(), receipt.ExplorerURL)
}
