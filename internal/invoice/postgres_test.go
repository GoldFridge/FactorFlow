package invoice_test

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
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// seedIssuer inserts an eligible issuer, since every invoice references one.
func seedIssuer(t *testing.T, db *postgres.DB, seq int) *organization.Organization {
	t.Helper()

	org, err := organization.New(organization.NewParams{
		ID:     uuid.New(),
		Type:   organization.TypeIssuer,
		Name:   fmt.Sprintf("Issuer %d", seq),
		Wallet: fmt.Sprintf("0x%040x", seq),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, org.Approve(testNow))
	require.NoError(t, organization.NewPostgresRepository().Create(context.Background(), db.Querier(), org))
	return org
}

func seedInvoice(t *testing.T, db *postgres.DB, issuerID uuid.UUID, number string) *invoice.Invoice {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  issuerID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    number,
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testIssuedAt,
		DueAt:     testDueAt,
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, invoice.NewPostgresRepository().Create(context.Background(), db.Querier(), inv))
	return inv
}

func TestInvoiceRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := invoice.NewPostgresRepository()

	issuer := seedIssuer(t, db, 1)
	inv := seedInvoice(t, db, issuer.ID, "INV-2026-0001")

	got, err := repo.Get(ctx, db.Querier(), inv.ID)
	require.NoError(t, err)

	assert.Equal(t, inv.ID, got.ID)
	assert.Equal(t, inv.IssuerID, got.IssuerID)
	assert.Equal(t, "ACME Logistics GmbH", got.DebtorRef)
	assert.Equal(t, "10000.00", got.Face.String(), "money survives as integer minor units")
	assert.Equal(t, money.USD, got.Face.Currency())
	assert.Equal(t, invoice.StatusDraft, got.Status)
	assert.Equal(t, int64(60), got.TenorDays())
	assert.Equal(t, uuid.Nil, got.AssessmentID, "an unassessed invoice stores no assessment id")
	assert.True(t, got.DueAt.Equal(inv.DueAt))
}

func TestGetMissingInvoice(t *testing.T) {
	db := pgtest.New(t)

	_, err := invoice.NewPostgresRepository().Get(context.Background(), db.Querier(), uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestLifecyclePersists walks the invoice through the states the demo uses and checks that
// each transition is what comes back out of the database.
func TestLifecyclePersists(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := invoice.NewPostgresRepository()

	issuer := seedIssuer(t, db, 2)
	inv := seedInvoice(t, db, issuer.ID, "INV-2026-0002")
	assessmentID, assetID := uuid.New(), uuid.New()

	steps := []struct {
		name   string
		apply  func() error
		expect invoice.Status
	}{
		{"upload", func() error { return inv.MarkUploaded(testNow.Add(1 * time.Minute)) }, invoice.StatusUploaded},
		{"assess", func() error { return inv.StartAssessment(testNow.Add(2 * time.Minute)) }, invoice.StatusExtracting},
		{"assessed", func() error { return inv.CompleteAssessment(assessmentID, testNow.Add(3*time.Minute)) }, invoice.StatusAssessed},
		{"approve", func() error { return inv.Approve(testNow.Add(4 * time.Minute)) }, invoice.StatusApproved},
		{"tokenize", func() error { return inv.StartTokenization(testNow.Add(5 * time.Minute)) }, invoice.StatusTokenizing},
		{"tokenized", func() error { return inv.CompleteTokenization(assetID, testNow.Add(6*time.Minute)) }, invoice.StatusTokenized},
	}

	for _, step := range steps {
		expectedVersion := inv.Version
		require.NoError(t, step.apply(), step.name)
		require.NoError(t, repo.Update(ctx, db.Querier(), inv, expectedVersion), step.name)

		stored, err := repo.Get(ctx, db.Querier(), inv.ID)
		require.NoError(t, err)
		assert.Equalf(t, step.expect, stored.Status, "after %s", step.name)
		assert.Equalf(t, inv.Version, stored.Version, "after %s", step.name)
	}

	stored, err := repo.Get(ctx, db.Querier(), inv.ID)
	require.NoError(t, err)
	assert.Equal(t, assessmentID, stored.AssessmentID)
	assert.Equal(t, assetID, stored.AssetID)
}

func TestFailureStagePersists(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := invoice.NewPostgresRepository()

	issuer := seedIssuer(t, db, 3)
	inv := seedInvoice(t, db, issuer.ID, "INV-2026-0003")

	require.NoError(t, inv.MarkUploaded(testNow.Add(time.Minute)))
	require.NoError(t, repo.Update(ctx, db.Querier(), inv, inv.Version-1))
	require.NoError(t, inv.StartAssessment(testNow.Add(2*time.Minute)))
	require.NoError(t, repo.Update(ctx, db.Querier(), inv, inv.Version-1))
	require.NoError(t, inv.FailAssessment("TEE did not respond after 3 attempts", testNow.Add(3*time.Minute)))
	require.NoError(t, repo.Update(ctx, db.Querier(), inv, inv.Version-1))

	stored, err := repo.Get(ctx, db.Querier(), inv.ID)
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusFailed, stored.Status)
	assert.Equal(t, invoice.StatusExtracting, stored.FailedFrom, "the retry knows which stage to resume")
	assert.Equal(t, "TEE did not respond after 3 attempts", stored.Reason)
}

func TestInvoiceOptimisticConcurrency(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := invoice.NewPostgresRepository()

	issuer := seedIssuer(t, db, 4)
	inv := seedInvoice(t, db, issuer.ID, "INV-2026-0004")

	writerA, err := repo.Get(ctx, db.Querier(), inv.ID)
	require.NoError(t, err)
	writerB, err := repo.Get(ctx, db.Querier(), inv.ID)
	require.NoError(t, err)

	require.NoError(t, writerA.MarkUploaded(testNow.Add(time.Minute)))
	require.NoError(t, repo.Update(ctx, db.Querier(), writerA, 1))

	require.NoError(t, writerB.Reject("stale rejection", testNow.Add(2*time.Minute)))
	require.ErrorIs(t, repo.Update(ctx, db.Querier(), writerB, 1), apperr.ErrConflict)

	stored, err := repo.Get(ctx, db.Querier(), inv.ID)
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusUploaded, stored.Status)
}

// TestDuplicateInvoiceNumberIsRefused covers the demo's double-financing guard: the same
// receivable cannot be submitted twice while one submission is still alive.
func TestDuplicateInvoiceNumberIsRefused(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := invoice.NewPostgresRepository()

	issuer := seedIssuer(t, db, 5)
	first := seedInvoice(t, db, issuer.ID, "INV-2026-0005")

	duplicate, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  issuer.ID,
		DebtorRef: first.DebtorRef,
		Number:    first.Number,
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testIssuedAt,
		DueAt:     testDueAt,
	}, testNow)
	require.NoError(t, err)

	require.ErrorIs(t, repo.Create(ctx, db.Querier(), duplicate), apperr.ErrConflict)

	// Once the first submission is rejected, the number is free again.
	require.NoError(t, first.Reject("withdrawn by issuer", testNow.Add(time.Minute)))
	require.NoError(t, repo.Update(ctx, db.Querier(), first, 1))
	require.NoError(t, repo.Create(ctx, db.Querier(), duplicate))
}

func TestListInvoices(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := invoice.NewPostgresRepository()

	issuer := seedIssuer(t, db, 6)
	other := seedIssuer(t, db, 7)

	for i := range 3 {
		seedInvoice(t, db, issuer.ID, fmt.Sprintf("INV-2026-10%02d", i))
	}
	seedInvoice(t, db, other.ID, "INV-2026-2000")

	mine, err := repo.ListByIssuer(ctx, db.Querier(), issuer.ID, 10)
	require.NoError(t, err)
	assert.Len(t, mine, 3, "an issuer sees only its own invoices")

	drafts, err := repo.ListByStatus(ctx, db.Querier(), invoice.StatusDraft, 10)
	require.NoError(t, err)
	assert.Len(t, drafts, 4)

	none, err := repo.ListByStatus(ctx, db.Querier(), invoice.StatusSettled, 10)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestDocumentMetadata(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := invoice.NewPostgresRepository()

	issuer := seedIssuer(t, db, 8)
	inv := seedInvoice(t, db, issuer.ID, "INV-2026-0008")

	doc, err := invoice.NewDocument(invoice.NewDocumentParams{
		InvoiceID:  inv.ID,
		ObjectKey:  "invoices/2026/09/" + inv.ID.String() + ".enc",
		CipherHash: testCipherHash,
		KeyRef:     "cre-secret://data-key/01J8Z9",
		MIME:       "application/pdf",
		SizeBytes:  482_113,
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, repo.SaveDocument(ctx, db.Querier(), doc))

	stored, err := repo.GetDocument(ctx, db.Querier(), inv.ID)
	require.NoError(t, err)
	assert.Equal(t, testCipherHash, stored.CipherHash)
	assert.Equal(t, "application/pdf", stored.MIME)
	assert.Equal(t, int64(482_113), stored.SizeBytes)
	assert.True(t, stored.MatchesCipherHash(testCipherHash))

	// A re-upload replaces the metadata; the new digest is what later steps bind to.
	replacement, err := invoice.NewDocument(invoice.NewDocumentParams{
		InvoiceID:  inv.ID,
		ObjectKey:  "invoices/2026/09/" + inv.ID.String() + "-v2.enc",
		CipherHash: "1111111111111111111111111111111111111111111111111111111111111111",
		KeyRef:     "cre-secret://data-key/01J8ZA",
		MIME:       "application/json",
		SizeBytes:  1024,
	}, testNow.Add(time.Hour))
	require.NoError(t, err)
	require.NoError(t, repo.SaveDocument(ctx, db.Querier(), replacement))

	stored, err = repo.GetDocument(ctx, db.Querier(), inv.ID)
	require.NoError(t, err)
	assert.Equal(t, replacement.CipherHash, stored.CipherHash)
	assert.False(t, stored.MatchesCipherHash(testCipherHash), "the superseded digest no longer matches")
}

func TestGetMissingDocument(t *testing.T) {
	db := pgtest.New(t)

	_, err := invoice.NewPostgresRepository().GetDocument(context.Background(), db.Querier(), uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestInvoiceRequiresAKnownIssuer checks the foreign key: an invoice cannot exist without
// the organization that issued it.
func TestInvoiceRequiresAKnownIssuer(t *testing.T) {
	db := pgtest.New(t)

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  uuid.New(),
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-9999",
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testIssuedAt,
		DueAt:     testDueAt,
	}, testNow)
	require.NoError(t, err)

	err = invoice.NewPostgresRepository().Create(context.Background(), db.Querier(), inv)
	require.ErrorIs(t, err, apperr.ErrConflict)
}

// terms builds a receivable with the given terms, for whichever issuer is selling it.
func terms(t *testing.T, issuerID uuid.UUID, number string) *invoice.Invoice {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  issuerID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    number,
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)
	return inv
}

/*
 * TestOneLiveReceivablePerTermsAcrossTheVenue is the double-financing guard the
 * specification asks for. The existing rule stops one issuer submitting the same invoice
 * twice; this is the fraud factoring actually suffers from — the same receivable sold to a
 * second financier, here a second account, each lending against one payment that can only
 * arrive once.
 */
func TestOneLiveReceivablePerTermsAcrossTheVenue(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := invoice.NewPostgresRepository()

	first := seedIssuer(t, db, 41)
	second := seedIssuer(t, db, 42)

	original := terms(t, first.ID, "INV-2026-0500")
	require.NoError(t, repo.Create(ctx, db.Querier(), original))

	// A different account, the same paper.
	duplicate := terms(t, second.ID, "INV-2026-0500")
	require.ErrorIs(t, repo.Create(ctx, db.Querier(), duplicate), apperr.ErrConflict)

	// Different terms are a different receivable and go through.
	other := terms(t, second.ID, "INV-2026-0501")
	require.NoError(t, repo.Create(ctx, db.Querier(), other))

	// A receivable that was rejected releases its terms: whatever it referred to is no
	// longer being financed here.
	require.NoError(t, original.Reject("the debtor disputes it", testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), original, 1))
	require.NoError(t, repo.Create(ctx, db.Querier(), terms(t, second.ID, "INV-2026-0500")))
}

/*
 * TestStoredFingerprintMatchesTheDomain pins the migration's backfill to the Go
 * implementation. Two expressions of one rule drift silently, and the drift would only
 * surface as a guard that quietly stopped guarding.
 */
func TestStoredFingerprintMatchesTheDomain(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	issuer := seedIssuer(t, db, 43)
	inv := terms(t, issuer.ID, "INV-2026-0502")
	require.NoError(t, invoice.NewPostgresRepository().Create(ctx, db.Querier(), inv))

	var stored, recomputed string
	require.NoError(t, db.Querier().QueryRow(ctx, `
		SELECT fingerprint,
		       encode(sha256(convert_to(
		           lower(btrim(debtor_ref)) || '|' ||
		           btrim(number) || '|' ||
		           face_minor::text || '|' ||
		           currency || '|' ||
		           to_char(due_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), 'UTF8')), 'hex')
		  FROM invoices WHERE id = $1`, inv.ID).Scan(&stored, &recomputed))

	assert.Equal(t, inv.Fingerprint(), stored, "what Go wrote is what the domain computes")
	assert.Equal(t, stored, recomputed, "and the migration's expression agrees with it")
}
