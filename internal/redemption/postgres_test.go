package redemption_test

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
	"github.com/GoldFridge/factorflow/internal/redemption"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// seedParty inserts an organization, since a repayment and every share it divides into
// point at one.
func seedParty(t *testing.T, db *postgres.DB, seq int) uuid.UUID {
	t.Helper()

	kind := organization.TypeInvestor
	if seq == 1 {
		kind = organization.TypeIssuer
	}
	org, err := organization.New(organization.NewParams{
		ID:     uuid.New(),
		Type:   kind,
		Name:   fmt.Sprintf("Organization %d", seq),
		Wallet: fmt.Sprintf("0x%040x", seq),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, org.Approve(testNow))
	require.NoError(t, organization.NewPostgresRepository().
		Create(context.Background(), db.Querier(), org))
	return org.ID
}

// seedInvoice inserts the receivable the repayment settles.
func seedInvoice(t *testing.T, db *postgres.DB, issuerID uuid.UUID, number string) uuid.UUID {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  issuerID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    number,
		Face:      usd("10000.00"),
		IssuedAt:  testNow.Add(-60 * 24 * time.Hour),
		DueAt:     testNow,
	}, testNow.Add(-60*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, invoice.NewPostgresRepository().
		Create(context.Background(), db.Querier(), inv))
	return inv.ID
}

// TestRepaymentRoundTrip is the storage contract: what was divided is what comes back,
// down to the minor unit and with the currency it was divided in.
func TestRepaymentRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := redemption.NewPostgresRepository()

	issuerID := seedParty(t, db, 1)
	investorID := seedParty(t, db, 2)
	invoiceID := seedInvoice(t, db, issuerID, "INV-2026-0101")

	rep, err := redemption.New(redemption.NewParams{
		ID:         uuid.New(),
		InvoiceID:  invoiceID,
		Face:       usd("10000.00"),
		Amount:     usd("10000.00"),
		Reference:  "SWIFT-2026-11-07-0042",
		ReceivedAt: testNow.Add(-time.Hour),
		RecordedBy: issuerID,
		Holders: []redemption.Holder{
			{PartyID: investorID, Notional: usd("6000.00")},
			{PartyID: issuerID, Notional: usd("4000.00")},
		},
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, repo.Create(ctx, db.Querier(), rep))

	got, err := repo.GetByInvoice(ctx, db.Querier(), invoiceID)
	require.NoError(t, err)

	assert.Equal(t, rep.ID, got.ID)
	assert.Equal(t, "10000.00", got.Amount.String(), "money survives as integer minor units")
	assert.Equal(t, "10000.00", got.Face.String())
	assert.Equal(t, money.USD, got.Amount.Currency())
	assert.Equal(t, rep.Reference, got.Reference)
	assert.False(t, got.IsShortfall())

	require.Len(t, got.Shares, 2)
	assert.Equal(t, usd("10000.00"), paid(t, got.Shares), "the division came back whole")

	share, ok := got.ShareOf(investorID)
	require.True(t, ok)
	assert.Equal(t, "6000.00", share.Amount.String())
	assert.Equal(t, "6000.00", share.Notional.String())
}

/*
 * TestAReceivableIsRepaidOnce is the constraint that makes double-crediting impossible
 * rather than unlikely. Two payments recorded against one receivable would pay every
 * holder twice, and no amount of care in the calling code is as good as the database
 * refusing.
 */
func TestAReceivableIsRepaidOnce(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := redemption.NewPostgresRepository()

	issuerID := seedParty(t, db, 1)
	invoiceID := seedInvoice(t, db, issuerID, "INV-2026-0102")

	first := storedRepayment(t, invoiceID, issuerID, "10000.00")
	require.NoError(t, repo.Create(ctx, db.Querier(), first))

	second := storedRepayment(t, invoiceID, issuerID, "10000.00")
	require.ErrorIs(t, repo.Create(ctx, db.Querier(), second), apperr.ErrConflict)
}

// TestMissingRepayment reports the absence as absence, which is what a caller deciding
// whether to record one needs to be able to tell.
func TestMissingRepayment(t *testing.T) {
	db := pgtest.New(t)

	_, err := redemption.NewPostgresRepository().
		GetByInvoice(context.Background(), db.Querier(), uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestListForPartyReturnsOnlyWhatWasHeld keeps an investor's record to the payments it was
// actually part of.
func TestListForPartyReturnsOnlyWhatWasHeld(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := redemption.NewPostgresRepository()

	issuerID := seedParty(t, db, 1)
	investorID := seedParty(t, db, 2)
	stranger := seedParty(t, db, 3)

	held := seedInvoice(t, db, issuerID, "INV-2026-0103")
	other := seedInvoice(t, db, issuerID, "INV-2026-0104")

	mine, err := redemption.New(redemption.NewParams{
		ID: uuid.New(), InvoiceID: held, Face: usd("10000.00"), Amount: usd("9000.00"),
		Reference: "SWIFT-1", ReceivedAt: testNow.Add(-time.Hour), RecordedBy: issuerID,
		Holders: []redemption.Holder{{PartyID: investorID, Notional: usd("10000.00")}},
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, repo.Create(ctx, db.Querier(), mine))
	require.NoError(t, repo.Create(ctx, db.Querier(), storedRepayment(t, other, issuerID, "10000.00")))

	got, err := repo.ListForParty(ctx, db.Querier(), investorID, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, held, got[0].InvoiceID)
	assert.True(t, got[0].IsShortfall(), "a short payment reads as one when it comes back")
	assert.Equal(t, "1000.00", got[0].Shortfall().String())

	none, err := repo.ListForParty(ctx, db.Querier(), stranger, 0)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// storedRepayment builds a repayment whose single holder is the issuer.
func storedRepayment(t *testing.T, invoiceID, issuerID uuid.UUID, amount string) *redemption.Repayment {
	t.Helper()

	rep, err := redemption.New(redemption.NewParams{
		ID:         uuid.New(),
		InvoiceID:  invoiceID,
		Face:       usd("10000.00"),
		Amount:     usd(amount),
		Reference:  "SWIFT-2026-11-07-0043",
		ReceivedAt: testNow.Add(-time.Hour),
		RecordedBy: issuerID,
		Holders:    []redemption.Holder{{PartyID: issuerID, Notional: usd("10000.00")}},
	}, testNow)
	require.NoError(t, err)
	return rep
}
