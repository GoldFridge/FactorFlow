package hedera_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/hedera"
)

/*
 * TestTheMirrorIsAskedInItsOwnDialect. The SDK writes a transaction id one way and the
 * mirror indexes it another, and getting it wrong does not fail loudly: the mirror answers
 * "not found", the saga reads that as "not yet", and a transfer that already happened waits
 * forever for a confirmation that cannot arrive.
 */
func TestTheMirrorIsAskedInItsOwnDialect(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0.0.1234-1699999999-123456789",
		hedera.MirrorTransactionID("0.0.1234@1699999999.123456789"))
	assert.Equal(t, "0.0.1234-1699999999-123456789",
		hedera.MirrorTransactionID("0.0.1234-1699999999-123456789"),
		"an id already in the mirror's form is left alone")
	assert.Empty(t, hedera.MirrorTransactionID("   "))
}

// TestTheMirrorReportsWhatMoved is why this is a check rather than a restatement: the record
// says which account was credited with how much of which token, on the network's word.
func TestTheMirrorReportsWhatMoved(t *testing.T) {
	t.Parallel()

	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		_, _ = w.Write([]byte(`{"transactions":[{
			"transaction_id":"0.0.1234-1699999999-123456789",
			"result":"SUCCESS",
			"consensus_timestamp":"1699999999.123456789",
			"token_transfers":[
				{"token_id":"0.0.777","account":"0.0.1234","amount":-1500000},
				{"token_id":"0.0.777","account":"0.0.5678","amount":1500000}
			]}]}`))
	}))
	defer server.Close()

	record, err := hedera.NewMirror(hedera.Testnet, server.URL).
		Transaction(context.Background(), "0.0.1234@1699999999.123456789")
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/transactions/0.0.1234-1699999999-123456789", asked)
	assert.True(t, record.Succeeded())
	assert.Equal(t, int64(1699999999), record.ConsensusAt.Unix())

	assert.True(t, record.Moved("0.0.777", "0.0.5678", 1500000),
		"the buyer was credited what the plan said")
	assert.False(t, record.Moved("0.0.777", "0.0.5678", 1500001),
		"and not a unit more")
	assert.False(t, record.Moved("0.0.999", "0.0.5678", 1500000), "nor in another token")
}

/*
 * TestAnUnknownTransactionIsNotAFailure. Mirrors lag consensus by seconds. A caller that
 * read "not indexed yet" as "did not happen" would resubmit a transfer that succeeded, which
 * is the one mistake the whole saga exists to prevent.
 */
func TestAnUnknownTransactionIsNotAFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"_status":{"messages":[{"message":"Not found"}]}}`))
	}))
	defer server.Close()

	record, err := hedera.NewMirror(hedera.Testnet, server.URL).
		Transaction(context.Background(), "0.0.1234@1699999999.123456789")

	require.NoError(t, err, "the mirror not knowing is an answer, not an error")
	assert.False(t, record.Found)
	assert.False(t, record.Succeeded())
}

// TestAFailedTransactionSaysSo: the mirror's verdict is the network's, and it is kept
// verbatim so an operator reads the same word the network used.
func TestAFailedTransactionSaysSo(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"transactions":[{
			"transaction_id":"0.0.1234-1699999999-123456789",
			"result":"TOKEN_NOT_ASSOCIATED_TO_ACCOUNT",
			"consensus_timestamp":"1699999999.123456789"}]}`))
	}))
	defer server.Close()

	record, err := hedera.NewMirror(hedera.Testnet, server.URL).
		Transaction(context.Background(), "0.0.1234@1699999999.123456789")
	require.NoError(t, err)

	assert.True(t, record.Found)
	assert.False(t, record.Succeeded())
	assert.Equal(t, "TOKEN_NOT_ASSOCIATED_TO_ACCOUNT", record.Status)
}

// TestThePublicMirrorIsChosenByNetwork, so a deployment that names a network gets the right
// one without also being told its address.
func TestThePublicMirrorIsChosenByNetwork(t *testing.T) {
	t.Parallel()

	assert.Contains(t, hedera.MirrorURL(hedera.Testnet), "testnet")
	assert.Contains(t, hedera.MirrorURL(hedera.Mainnet), "mainnet")
	assert.Contains(t, hedera.MirrorURL(""), "testnet", "the safe default is not mainnet")
}
