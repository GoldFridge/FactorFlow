package tokenization_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/hedera"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

// fakeChain records what would have been created, so the mapping can be checked without a
// network. What Hedera does with a well-formed request is Hedera's business, and the live
// test in the platform package is where that is proven.
type fakeChain struct {
	token hedera.Token
	fail  error
}

func (c *fakeChain) CreateToken(_ context.Context, token hedera.Token) (hedera.Receipt, error) {
	c.token = token
	if c.fail != nil {
		return hedera.Receipt{}, c.fail
	}
	return hedera.Receipt{
		TokenID:       "0.0.4242",
		TransactionID: "0.0.1@1788875814.455340955",
		ExplorerURL:   "https://hashscan.io/testnet/token/0.0.4242",
		Status:        "SUCCESS",
	}, nil
}

func (c *fakeChain) Network() string  { return "testnet" }
func (c *fakeChain) Operator() string { return "0.0.1" }

func issueRequest() tokenization.IssueRequest {
	return tokenization.IssueRequest{
		InvoiceID:    uuid.MustParse("92eb7a94-7c55-5de0-baa8-7176595c8101"),
		IssuerID:     uuid.New(),
		IssuerWallet: "0x0000000000000000000000000000000000000a01",
		Supply:       money.MustParse("15000.00", money.USD),
		Reference:    "assessment-1",
		Metadata: tokenization.Metadata{
			InvoiceCommitment: "0x1a2b3c",
			Currency:          "USD",
			FaceValue:         "15000.00",
			MaturityDate:      "2026-10-24T00:00:00Z",
			Grade:             "C",
			TermsHash:         "0x9f8e7d",
		},
	}
}

/*
 * The supply is the face value in minor units, and the decimals say so.
 *
 * That mapping is the whole design of the token: a holder's balance is the notional they
 * own in cents, so nothing has to be converted to read it, and no conversion can be got
 * wrong later.
 */
func TestSupplyIsTheFaceValueInMinorUnits(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{}
	result, err := tokenization.NewHederaIssuer(chain).Issue(t.Context(), issueRequest())
	require.NoError(t, err)

	assert.Equal(t, uint64(1_500_000), chain.token.Supply)
	assert.Equal(t, uint(2), chain.token.Decimals)

	assert.Equal(t, "testnet", result.Network)
	assert.Equal(t, "0.0.4242", result.TokenID)
	assert.NotEmpty(t, result.TransactionID)
	assert.True(t, result.Confirmed, "a receipt is the network's own confirmation")
	require.NoError(t, result.Validate())
}

/*
 * The memo is public and permanent, so it carries the commitment and nothing else. A token
 * that named its debtor would publish the one fact the confidential workflow exists to keep.
 */
func TestTheMemoCarriesOnlyTheCommitment(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{}
	request := issueRequest()
	request.Metadata.InvoiceCommitment = "0xc0ffee"

	_, err := tokenization.NewHederaIssuer(chain).Issue(t.Context(), request)
	require.NoError(t, err)

	assert.Equal(t, "0xc0ffee", chain.token.Memo)
	assert.NotContains(t, chain.token.Name, "0x0000000000000000000000000000000000000a01")
	assert.Contains(t, chain.token.Name, "grade C")
	assert.Equal(t, "FF92EB7A94", chain.token.Symbol)
}

func TestIssueRefusesWhatItCannotMint(t *testing.T) {
	t.Parallel()

	issuer := tokenization.NewHederaIssuer(&fakeChain{})

	nameless := issueRequest()
	nameless.InvoiceID = uuid.Nil
	_, err := issuer.Issue(t.Context(), nameless)
	assert.ErrorIs(t, err, apperr.ErrValidation)

	empty := issueRequest()
	empty.Supply = money.Zero(money.USD)
	_, err = issuer.Issue(t.Context(), empty)
	assert.ErrorIs(t, err, apperr.ErrValidation)
}

// A network that refused is reported as it came back: the worker turns that into a failed
// invoice and a retry, and inventing a plausible token id instead would be a lie on chain.
func TestARefusedMintIsReported(t *testing.T) {
	t.Parallel()

	refusal := errors.New("INSUFFICIENT_PAYER_BALANCE")
	_, err := tokenization.NewHederaIssuer(&fakeChain{fail: refusal}).Issue(t.Context(), issueRequest())

	require.ErrorIs(t, err, refusal)
}
