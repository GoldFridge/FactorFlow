package invoice_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/testsupport/audittest"
)

// serviceFixture wires a service over the in-memory fakes, with a fixed clock and id
// source so a test can assert exact values.
type serviceFixture struct {
	service *invoice.Service
	repo    *memRepository
	db      *fakeDB
	trail   *audittest.Recorder
	issuer  invoice.Actor
	other   invoice.Actor
	clock   time.Time
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()

	repo := newMemRepository()
	db := newFakeDB(repo)

	f := &serviceFixture{
		repo:   repo,
		db:     db,
		trail:  audittest.New(),
		issuer: invoice.Actor{OrganizationID: uuid.MustParse("11111111-1111-4111-8111-111111111111")},
		other:  invoice.Actor{OrganizationID: uuid.MustParse("22222222-2222-4222-8222-222222222222")},
		clock:  testNow,
	}
	f.service = invoice.NewService(db, repo, f.trail, func() time.Time { return f.clock }, uuid.New)
	return f
}

func (f *serviceFixture) createParams() invoice.CreateParams {
	return invoice.CreateParams{
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0042",
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testIssuedAt,
		DueAt:     testDueAt,
	}
}

func (f *serviceFixture) documentParams() invoice.NewDocumentParams {
	return invoice.NewDocumentParams{
		ObjectKey:  "invoices/2026/09/9f86d081.enc",
		CipherHash: testCipherHash,
		KeyRef:     "cre-secret://data-key/01J8Z9",
		MIME:       "application/pdf",
		SizeBytes:  482_113,
	}
}

// uploaded returns an invoice that has its encrypted document attached.
func (f *serviceFixture) uploaded(t *testing.T) *invoice.Invoice {
	t.Helper()

	ctx := context.Background()
	inv, err := f.service.Create(ctx, f.issuer, f.createParams())
	require.NoError(t, err)

	inv, err = f.service.AttachDocument(ctx, f.issuer, inv.ID, f.documentParams())
	require.NoError(t, err)
	return inv
}

func TestServiceCreate(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	inv, err := f.service.Create(context.Background(), f.issuer, f.createParams())
	require.NoError(t, err)

	assert.Equal(t, invoice.StatusDraft, inv.Status)
	assert.Equal(t, f.issuer.OrganizationID, inv.IssuerID, "the issuer is the caller, never a field in the body")
	assert.Equal(t, testNow, inv.CreatedAt)

	stored, err := f.service.Get(context.Background(), f.issuer, inv.ID)
	require.NoError(t, err)
	assert.Equal(t, inv.ID, stored.ID)
}

func TestServiceCreateRejectsInvalidFacts(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	params := f.createParams()
	params.Face = money.Zero(money.USD)

	_, err := f.service.Create(context.Background(), f.issuer, params)
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func TestServiceCreateNeedsAnOrganization(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)

	_, err := f.service.Create(context.Background(), invoice.Actor{}, f.createParams())
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

// TestAttachDocumentWritesBothOrNeither is the composition rule: an invoice marked UPLOADED
// with no document would break the assessment step that follows.
func TestAttachDocumentWritesBothOrNeither(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()

	inv, err := f.service.Create(ctx, f.issuer, f.createParams())
	require.NoError(t, err)

	updated, err := f.service.AttachDocument(ctx, f.issuer, inv.ID, f.documentParams())
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusUploaded, updated.Status)

	doc, err := f.service.GetDocument(ctx, f.issuer, inv.ID)
	require.NoError(t, err)
	assert.Equal(t, testCipherHash, doc.CipherHash)

	// Now make the commit fail and check that neither write survives.
	second, err := f.service.Create(ctx, f.issuer, invoice.CreateParams{
		DebtorRef: "BOLT SA", Number: "INV-2026-0043",
		Face: money.MustParse("5000.00", money.USD), IssuedAt: testIssuedAt, DueAt: testDueAt,
	})
	require.NoError(t, err)

	f.db.failCommit = errors.New("commit failed")
	_, err = f.service.AttachDocument(ctx, f.issuer, second.ID, f.documentParams())
	require.Error(t, err)
	f.db.failCommit = nil

	stored, err := f.service.Get(ctx, f.issuer, second.ID)
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusDraft, stored.Status, "the state change rolled back")

	_, err = f.service.GetDocument(ctx, f.issuer, second.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound, "and so did the document")
}

func TestAttachDocumentValidatesMetadata(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()

	inv, err := f.service.Create(ctx, f.issuer, f.createParams())
	require.NoError(t, err)

	params := f.documentParams()
	params.CipherHash = "not-a-digest"

	_, err = f.service.AttachDocument(ctx, f.issuer, inv.ID, params)
	require.ErrorIs(t, err, apperr.ErrValidation)

	stored, err := f.service.Get(ctx, f.issuer, inv.ID)
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusDraft, stored.Status)
}

// TestRequestAssessmentQueuesTheCommand is the outbox rule: the state change and the intent
// to act on it are committed together.
func TestRequestAssessmentQueuesTheCommand(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	inv := f.uploaded(t)

	updated, err := f.service.RequestAssessment(ctx, f.issuer, inv.ID, "trace-42")
	require.NoError(t, err)

	assert.Equal(t, invoice.StatusExtracting, updated.Status)
	assert.True(t, f.db.querier.published("outbox_events"),
		"the assessment command was queued in the same transaction")
}

func TestRequestAssessmentNeedsADocument(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()

	inv, err := f.service.Create(ctx, f.issuer, f.createParams())
	require.NoError(t, err)

	_, err = f.service.RequestAssessment(ctx, f.issuer, inv.ID, "")
	require.ErrorIs(t, err, apperr.ErrNotFound, "there is nothing to assess without an upload")

	stored, err := f.service.Get(ctx, f.issuer, inv.ID)
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusDraft, stored.Status)
}

func TestRequestAssessmentRefusesTheWrongState(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	inv := f.uploaded(t)

	_, err := f.service.RequestAssessment(ctx, f.issuer, inv.ID, "")
	require.NoError(t, err)

	_, err = f.service.RequestAssessment(ctx, f.issuer, inv.ID, "")
	require.ErrorIs(t, err, apperr.ErrConflict, "an assessment already running is not queued twice")
}

func TestApproveAndReject(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	inv := f.uploaded(t)

	// Approval requires an assessment, which only the workflow can record.
	_, err := f.service.Approve(ctx, f.issuer, inv.ID)
	require.ErrorIs(t, err, apperr.ErrConflict)

	updated, err := f.service.Reject(ctx, f.issuer, inv.ID, "withdrawn by issuer")
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusRejected, updated.Status)
	assert.Equal(t, "withdrawn by issuer", updated.Reason)

	_, err = f.service.Reject(ctx, f.issuer, inv.ID, "again")
	require.ErrorIs(t, err, apperr.ErrConflict, "a terminal invoice does not move")
}

func TestRejectValidatesTheReason(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	inv := f.uploaded(t)

	_, err := f.service.Reject(ctx, f.issuer, inv.ID, "   ")
	require.ErrorIs(t, err, apperr.ErrValidation)
}

// TestAnotherOrganizationSeesNothing is the disclosure rule: telling a stranger that an
// invoice exists is itself a leak, so the answer is "not found", not "forbidden".
func TestAnotherOrganizationSeesNothing(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	inv := f.uploaded(t)

	_, err := f.service.Get(ctx, f.other, inv.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)
	assert.NotErrorIs(t, err, apperr.ErrForbidden)

	_, err = f.service.GetDocument(ctx, f.other, inv.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = f.service.Reject(ctx, f.other, inv.ID, "not mine to reject")
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = f.service.RequestAssessment(ctx, f.other, inv.ID, "")
	require.ErrorIs(t, err, apperr.ErrNotFound)

	stored, err := f.service.Get(ctx, f.issuer, inv.ID)
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusUploaded, stored.Status, "nothing the stranger tried took effect")
}

func TestOperatorMaySeeAnyInvoice(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	inv := f.uploaded(t)

	operator := invoice.Actor{OrganizationID: uuid.New(), Operator: true}
	stored, err := f.service.Get(ctx, operator, inv.ID)
	require.NoError(t, err)
	assert.Equal(t, inv.ID, stored.ID)
}

func TestListReturnsOnlyTheCallersInvoices(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()

	for i, number := range []string{"INV-1", "INV-2", "INV-3"} {
		params := f.createParams()
		params.Number = number
		actor := f.issuer
		if i == 2 {
			actor = f.other
		}
		_, err := f.service.Create(ctx, actor, params)
		require.NoError(t, err)
	}

	mine, err := f.service.List(ctx, f.issuer, 10)
	require.NoError(t, err)
	assert.Len(t, mine, 2)
	for _, inv := range mine {
		assert.Equal(t, f.issuer.OrganizationID, inv.IssuerID)
	}

	_, err = f.service.List(ctx, invoice.Actor{}, 10)
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

// TestLostRaceIsReported keeps the version check visible through the service: a write that
// lost a race is a conflict the caller must retry, not a silent no-op.
func TestLostRaceIsReported(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	inv := f.uploaded(t)

	f.repo.failUpdate = apperr.Conflictf("invoice %s was modified by another writer", inv.ID)

	_, err := f.service.Reject(ctx, f.issuer, inv.ID, "reason")
	require.ErrorIs(t, err, apperr.ErrConflict)
}

func TestGetMissingInvoiceThroughTheService(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)

	_, err := f.service.Get(context.Background(), f.issuer, uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestLifecycleIsRecorded is what makes the timeline usable: every step a person took is
// on it, attributed to them, with the status it produced.
func TestLifecycleIsRecorded(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := t.Context()

	inv, err := f.service.Create(ctx, f.issuer, f.createParams())
	require.NoError(t, err)
	_, err = f.service.AttachDocument(ctx, f.issuer, inv.ID, f.documentParams())
	require.NoError(t, err)
	_, err = f.service.RequestAssessment(ctx, f.issuer, inv.ID, "trace-1")
	require.NoError(t, err)

	assert.Equal(t, []string{
		invoice.ActionCreated,
		invoice.ActionDocumentAttached,
		invoice.ActionAssessmentRequested,
	}, f.trail.Actions())

	events := f.trail.For(invoice.EntityType, inv.ID.String())
	require.Len(t, events, 3)

	created := events[2]
	assert.Equal(t, f.issuer.OrganizationID.String(), created.Actor, "the issuer is on record")
	assert.Empty(t, created.BeforeHash, "a new invoice has no prior state")
	assert.Equal(t, invoice.StatusDraft.String(), created.Detail["status"])

	attached := events[1]
	assert.Equal(t, testCipherHash, attached.Detail["cipher_hash"],
		"the timeline names the ciphertext without carrying the document")
	assert.NotEqual(t, attached.BeforeHash, attached.AfterHash, "the state moved")
}

// TestNothingIsRecordedWhenTheChangeIsNotCommitted keeps the timeline from claiming
// something that never happened.
func TestNothingIsRecordedWhenTheChangeIsNotCommitted(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)

	f.db.failCommit = errors.New("commit failed")
	_, err := f.service.Create(t.Context(), f.issuer, f.createParams())
	require.Error(t, err)

	// The in-memory recorder is not transactional, so this asserts the other half: the
	// invoice is gone too, and a timeline entry without its invoice is what the real
	// recorder's shared transaction prevents.
	assert.Empty(t, f.repo.invoices, "the invoice was rolled back with the entry it produced")
}

// TestARefusedChangeRecordsNothing covers the ordinary path: a rejected command never
// reaches the recorder at all.
func TestARefusedChangeRecordsNothing(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)

	inv, err := f.service.Create(t.Context(), f.issuer, f.createParams())
	require.NoError(t, err)
	f.trail.Reset()

	_, err = f.service.Approve(t.Context(), f.issuer, inv.ID)
	require.Error(t, err, "a draft cannot be approved")
	assert.Empty(t, f.trail.Events())

	_, err = f.service.Approve(t.Context(), f.other, inv.ID)
	require.Error(t, err)
	assert.Empty(t, f.trail.Events(), "a refused caller leaves no trace on someone else's invoice")
}
