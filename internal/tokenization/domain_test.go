package tokenization_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

var testNow = time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)

func validParams() tokenization.NewParams {
	return tokenization.NewParams{
		ID:            uuid.New(),
		InvoiceID:     uuid.New(),
		IssuerID:      uuid.New(),
		Network:       "hedera-testnet",
		TokenID:       "0.0.4823901",
		ContractID:    "0.0.4823900",
		Supply:        money.MustParse("10000.00", money.USD),
		ChainStatus:   tokenization.StatusIssued,
		TransactionID: "0.0.1234@1757160000.000000000",
		ExplorerURL:   "https://hashscan.io/testnet/token/0.0.4823901",
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	asset, err := tokenization.New(validParams(), testNow)
	require.NoError(t, err)

	assert.Equal(t, tokenization.StatusIssued, asset.ChainStatus)
	assert.Equal(t, "10000.00", asset.Supply.String())
	assert.True(t, asset.IsTransferable())
	assert.Equal(t, testNow, asset.CreatedAt)
}

func TestNewDefaultsToPending(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.ChainStatus = ""

	asset, err := tokenization.New(p, testNow)
	require.NoError(t, err)
	assert.Equal(t, tokenization.StatusPending, asset.ChainStatus,
		"an asset is pending until the network confirms it")
	assert.False(t, asset.IsTransferable(), "an unconfirmed asset cannot be settled")
}

func TestNewRejectsBrokenFacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*tokenization.NewParams)
		wantField string
	}{
		{name: "nil id", mutate: func(p *tokenization.NewParams) { p.ID = uuid.Nil }, wantField: "id"},
		{name: "nil invoice", mutate: func(p *tokenization.NewParams) { p.InvoiceID = uuid.Nil }, wantField: "invoice_id"},
		{name: "nil issuer", mutate: func(p *tokenization.NewParams) { p.IssuerID = uuid.Nil }, wantField: "issuer_id"},
		{name: "no network", mutate: func(p *tokenization.NewParams) { p.Network = " " }, wantField: "network"},
		{name: "no token", mutate: func(p *tokenization.NewParams) { p.TokenID = "" }, wantField: "token_id"},
		{name: "long token", mutate: func(p *tokenization.NewParams) {
			p.TokenID = strings.Repeat("x", tokenization.MaxIdentifierLen+1)
		}, wantField: "token_id"},
		{name: "long explorer url", mutate: func(p *tokenization.NewParams) {
			p.ExplorerURL = strings.Repeat("x", tokenization.MaxURLLen+1)
		}, wantField: "explorer_url"},
		{name: "zero supply", mutate: func(p *tokenization.NewParams) { p.Supply = money.Zero(money.USD) }, wantField: "supply"},
		{name: "supply without currency", mutate: func(p *tokenization.NewParams) { p.Supply = money.Amount{} }, wantField: "supply"},
		{name: "unknown status", mutate: func(p *tokenization.NewParams) {
			p.ChainStatus = tokenization.ChainStatus("MINTED")
		}, wantField: "chain_status"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := validParams()
			tc.mutate(&p)

			asset, err := tokenization.New(p, testNow)
			require.Nil(t, asset)
			require.ErrorIs(t, err, apperr.ErrValidation)

			fields := make([]string, 0)
			for _, f := range apperr.Fields(err) {
				fields = append(fields, f.Field)
			}
			assert.Contains(t, fields, tc.wantField)
		})
	}
}

// TestChainLifecycle walks the states a compliant asset moves through, including the freeze
// the specification requires as a compliance control.
func TestChainLifecycle(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.ChainStatus = tokenization.StatusPending
	asset, err := tokenization.New(p, testNow)
	require.NoError(t, err)

	require.NoError(t, asset.MarkIssued("0.0.1234@1757160000.000000000", "https://hashscan.io/testnet/tx/1", testNow.Add(time.Minute)))
	assert.Equal(t, tokenization.StatusIssued, asset.ChainStatus)
	assert.True(t, asset.IsTransferable())

	require.NoError(t, asset.Freeze(testNow.Add(2*time.Minute)))
	assert.False(t, asset.IsTransferable(), "a frozen asset cannot be settled")

	require.NoError(t, asset.Unfreeze(testNow.Add(3*time.Minute)))
	assert.True(t, asset.IsTransferable())

	require.NoError(t, asset.Redeem(testNow.Add(4*time.Minute)))
	assert.Equal(t, tokenization.StatusRedeemed, asset.ChainStatus)

	require.ErrorIs(t, asset.Freeze(testNow.Add(5*time.Minute)), apperr.ErrConflict,
		"a repaid receivable has nothing left to freeze")
	require.ErrorIs(t, asset.Redeem(testNow.Add(5*time.Minute)), apperr.ErrConflict)
}

func TestPendingAssetCannotBeFrozenOrRedeemed(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.ChainStatus = tokenization.StatusPending
	asset, err := tokenization.New(p, testNow)
	require.NoError(t, err)

	require.ErrorIs(t, asset.Freeze(testNow), apperr.ErrConflict)
	require.ErrorIs(t, asset.Redeem(testNow), apperr.ErrConflict)
}

func TestParseChainStatus(t *testing.T) {
	t.Parallel()

	got, err := tokenization.ParseChainStatus("FROZEN")
	require.NoError(t, err)
	assert.Equal(t, tokenization.StatusFrozen, got)

	_, err = tokenization.ParseChainStatus("frozen")
	require.ErrorIs(t, err, apperr.ErrValidation)
}

// TestLocalIssuerIsDeterministic is what makes the issuance handler safe to redeliver: the
// same invoice always asks for the same token.
func TestLocalIssuerIsDeterministic(t *testing.T) {
	t.Parallel()

	issuer := tokenization.NewLocalIssuer()
	request := tokenization.IssueRequest{
		InvoiceID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		IssuerID:  uuid.New(),
		Supply:    money.MustParse("10000.00", money.USD),
		Reference: "assessment-1",
	}

	first, err := issuer.Issue(context.Background(), request)
	require.NoError(t, err)

	second, err := issuer.Issue(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, first.TokenID, second.TokenID)

	// A different assessment is a different issuance.
	request.Reference = "assessment-2"
	third, err := issuer.Issue(context.Background(), request)
	require.NoError(t, err)
	assert.NotEqual(t, first.TokenID, third.TokenID)
}

// TestLocalIssuerAdmitsWhatItIs keeps the offline path honest: there is no chain, so there
// is no transaction and no explorer link to show.
func TestLocalIssuerAdmitsWhatItIs(t *testing.T) {
	t.Parallel()

	result, err := tokenization.NewLocalIssuer().Issue(context.Background(), tokenization.IssueRequest{
		InvoiceID: uuid.New(),
		Supply:    money.MustParse("10000.00", money.USD),
	})
	require.NoError(t, err)

	assert.Equal(t, tokenization.LocalNetwork, result.Network)
	assert.NotEqual(t, "hedera-testnet", result.Network, "a local asset must not look like a testnet one")
	assert.Empty(t, result.TransactionID)
	assert.Empty(t, result.ExplorerURL)
	assert.True(t, result.Confirmed)
	require.NoError(t, result.Validate())
}

func TestLocalIssuerRejectsBadRequests(t *testing.T) {
	t.Parallel()

	issuer := tokenization.NewLocalIssuer()

	_, err := issuer.Issue(context.Background(), tokenization.IssueRequest{
		Supply: money.MustParse("100.00", money.USD),
	})
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = issuer.Issue(context.Background(), tokenization.IssueRequest{
		InvoiceID: uuid.New(),
		Supply:    money.Zero(money.USD),
	})
	require.ErrorIs(t, err, apperr.ErrValidation)
}

// TestIssueResultValidation treats the issuer as an external system whose answer is input.
func TestIssueResultValidation(t *testing.T) {
	t.Parallel()

	require.NoError(t, tokenization.IssueResult{Network: "local", TokenID: "t-1"}.Validate())
	require.ErrorIs(t, tokenization.IssueResult{TokenID: "t-1"}.Validate(), apperr.ErrValidation)
	require.ErrorIs(t, tokenization.IssueResult{Network: "local"}.Validate(), apperr.ErrValidation)
}
