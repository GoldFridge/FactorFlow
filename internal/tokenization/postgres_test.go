package tokenization_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/pgtest"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// seedInvoice inserts the organization and invoice an asset points at.
func seedInvoice(t *testing.T, db *postgres.DB, seq int) (uuid.UUID, uuid.UUID) {
	t.Helper()

	org, err := organization.New(organization.NewParams{
		ID:     uuid.New(),
		Type:   organization.TypeIssuer,
		Name:   fmt.Sprintf("Issuer %d", seq),
		Wallet: fmt.Sprintf("0x%040x", seq),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, organization.NewPostgresRepository().Create(context.Background(), db.Querier(), org))

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  org.ID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    fmt.Sprintf("INV-2026-%04d", seq),
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, invoice.NewPostgresRepository().Create(context.Background(), db.Querier(), inv))

	return org.ID, inv.ID
}

func storedAsset(t *testing.T, issuerID, invoiceID uuid.UUID) *tokenization.Asset {
	t.Helper()

	asset, err := tokenization.New(tokenization.NewParams{
		ID:            uuid.New(),
		InvoiceID:     invoiceID,
		IssuerID:      issuerID,
		Network:       "hedera-testnet",
		TokenID:       "0.0.4823901",
		ContractID:    "0.0.4823900",
		Supply:        money.MustParse("10000.00", money.USD),
		ChainStatus:   tokenization.StatusPending,
		TransactionID: "0.0.1234@1757160000.000000000",
		ExplorerURL:   "https://hashscan.io/testnet/token/0.0.4823901",
	}, testNow)
	require.NoError(t, err)
	return asset
}

func TestAssetRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := tokenization.NewPostgresRepository()

	issuerID, invoiceID := seedInvoice(t, db, 1)
	asset := storedAsset(t, issuerID, invoiceID)
	require.NoError(t, repo.Create(ctx, db.Querier(), asset))

	got, err := repo.Get(ctx, db.Querier(), asset.ID)
	require.NoError(t, err)

	assert.Equal(t, asset.ID, got.ID)
	assert.Equal(t, invoiceID, got.InvoiceID)
	assert.Equal(t, "hedera-testnet", got.Network)
	assert.Equal(t, "0.0.4823901", got.TokenID)
	assert.Equal(t, "10000.00", got.Supply.String())
	assert.Equal(t, tokenization.StatusPending, got.ChainStatus)
	assert.Equal(t, asset.ExplorerURL, got.ExplorerURL, "the evidence link survives storage")

	byInvoice, err := repo.GetByInvoice(ctx, db.Querier(), invoiceID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, byInvoice.ID)
}

// TestOneAssetPerInvoice is the constraint that stops two claims on one receivable, which
// no later reconciliation could undo.
func TestOneAssetPerInvoice(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := tokenization.NewPostgresRepository()

	issuerID, invoiceID := seedInvoice(t, db, 2)
	require.NoError(t, repo.Create(ctx, db.Querier(), storedAsset(t, issuerID, invoiceID)))

	err := repo.Create(ctx, db.Querier(), storedAsset(t, issuerID, invoiceID))
	require.ErrorIs(t, err, apperr.ErrConflict)
}

func TestChainStatusUpdatePersists(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := tokenization.NewPostgresRepository()

	issuerID, invoiceID := seedInvoice(t, db, 3)
	asset := storedAsset(t, issuerID, invoiceID)
	require.NoError(t, repo.Create(ctx, db.Querier(), asset))

	require.NoError(t, asset.MarkIssued("0.0.9999@1757160500.000000000", "https://hashscan.io/testnet/tx/2", testNow.Add(time.Minute)))
	require.NoError(t, repo.Update(ctx, db.Querier(), asset))

	got, err := repo.Get(ctx, db.Querier(), asset.ID)
	require.NoError(t, err)
	assert.Equal(t, tokenization.StatusIssued, got.ChainStatus)
	assert.Equal(t, "0.0.9999@1757160500.000000000", got.TransactionID)
	assert.True(t, got.IsTransferable())

	require.NoError(t, got.Freeze(testNow.Add(2*time.Minute)))
	require.NoError(t, repo.Update(ctx, db.Querier(), got))

	frozen, err := repo.Get(ctx, db.Querier(), asset.ID)
	require.NoError(t, err)
	assert.False(t, frozen.IsTransferable(), "a frozen asset stays unsellable across a restart")
}

func TestGetMissingAsset(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := tokenization.NewPostgresRepository()

	_, err := repo.Get(ctx, db.Querier(), uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = repo.GetByInvoice(ctx, db.Querier(), uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)

	orphan := storedAsset(t, uuid.New(), uuid.New())
	require.NoError(t, orphan.MarkIssued("", "", testNow))
	require.ErrorIs(t, repo.Update(ctx, db.Querier(), orphan), apperr.ErrNotFound)
}

func TestAssetRequiresAKnownInvoice(t *testing.T) {
	db := pgtest.New(t)

	issuerID, _ := seedInvoice(t, db, 4)
	orphan := storedAsset(t, issuerID, uuid.New())

	err := tokenization.NewPostgresRepository().Create(context.Background(), db.Querier(), orphan)
	require.ErrorIs(t, err, apperr.ErrConflict)
}

func TestListByIssuer(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := tokenization.NewPostgresRepository()

	issuerID, firstInvoice := seedInvoice(t, db, 5)
	require.NoError(t, repo.Create(ctx, db.Querier(), storedAsset(t, issuerID, firstInvoice)))

	otherIssuer, otherInvoice := seedInvoice(t, db, 6)
	require.NoError(t, repo.Create(ctx, db.Querier(), storedAsset(t, otherIssuer, otherInvoice)))

	mine, err := repo.ListByIssuer(ctx, db.Querier(), issuerID, 10)
	require.NoError(t, err)
	require.Len(t, mine, 1, "an issuer sees only its own assets")
	assert.Equal(t, firstInvoice, mine[0].InvoiceID)
}
