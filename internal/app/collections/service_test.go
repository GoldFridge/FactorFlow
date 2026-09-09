package collections_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/collections"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/settlement"
	"github.com/GoldFridge/factorflow/internal/testsupport/memrepo"
)

var (
	testNow = time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	issued  = testNow.Add(-60 * 24 * time.Hour)
)

func usd(s string) money.Amount { return money.MustParse(s, money.USD) }

type fixture struct {
	service *collections.Service
	store   *memrepo.Store
	router  http.Handler

	issuer   collections.Actor
	investor collections.Actor
	other    collections.Actor
	operator collections.Actor
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store := memrepo.New()
	f := &fixture{
		store:    store,
		issuer:   collections.Actor{OrganizationID: uuid.MustParse("11111111-1111-4111-8111-111111111111")},
		investor: collections.Actor{OrganizationID: uuid.MustParse("22222222-2222-4222-8222-222222222222")},
		other:    collections.Actor{OrganizationID: uuid.MustParse("33333333-3333-4333-8333-333333333333")},
		operator: collections.Actor{OrganizationID: uuid.MustParse("44444444-4444-4444-8444-444444444444"), Operator: true},
	}

	f.service = collections.NewService(collections.Config{
		DB:          store,
		Invoices:    store.Invoices(),
		Settlements: store.Settlements(),
		Repayments:  store.Repayments(),
		Audit:       store.Audit(),
		Now:         func() time.Time { return testNow },
	})

	handler := collections.NewHandler(f.service)
	f.router = httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					next.ServeHTTP(w, req.WithContext(
						httpserver.ContextWithActor(req.Context(), f.actor(req))))
				})
			})
			handler.Routes(r)
		},
	})
	return f
}

// actor lets a test choose who is calling, standing in for the session middleware.
func (f *fixture) actor(r *http.Request) httpserver.Actor {
	switch r.Header.Get("X-Test-Actor") {
	case "issuer":
		return httpserver.Actor{OrganizationID: f.issuer.OrganizationID, Eligible: true}
	case "investor":
		return httpserver.Actor{OrganizationID: f.investor.OrganizationID, Eligible: true}
	case "other":
		return httpserver.Actor{OrganizationID: f.other.OrganizationID, Eligible: true}
	case "operator":
		return httpserver.Actor{OrganizationID: f.operator.OrganizationID, Eligible: true, Operator: true}
	default:
		return httpserver.Actor{}
	}
}

func (f *fixture) call(t *testing.T, method, path, actor, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// settledInvoice is a receivable that reached the end of the venue: sold, transferred, and
// waiting for the debtor. sold is how much of the face the investor ended up holding.
func (f *fixture) settledInvoice(t *testing.T, sold string) *invoice.Invoice {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  f.issuer.OrganizationID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0007",
		Face:      usd("10000.00"),
		IssuedAt:  issued,
		DueAt:     testNow,
	}, issued)
	require.NoError(t, err)

	// The lifecycle is walked rather than assigned: a test that sets a status directly
	// proves nothing about a path the application can actually reach.
	require.NoError(t, inv.MarkUploaded(issued))
	require.NoError(t, inv.StartAssessment(issued))
	require.NoError(t, inv.CompleteAssessment(uuid.New(), issued))
	require.NoError(t, inv.Approve(issued))
	require.NoError(t, inv.StartTokenization(issued))
	require.NoError(t, inv.CompleteTokenization(uuid.New(), issued))
	require.NoError(t, inv.OpenAuction(issued))
	require.NoError(t, inv.MarkAllocated(issued))
	require.NoError(t, inv.MarkSettled(issued))
	f.store.PutInvoice(inv)

	if sold != "" {
		f.transfer(t, inv, f.investor.OrganizationID, sold, true)
	}
	return inv
}

// transfer records a settlement of part of the receivable, finished or still in flight.
func (f *fixture) transfer(t *testing.T, inv *invoice.Invoice, investorID uuid.UUID, notional string, finished bool) {
	t.Helper()

	plan, err := settlement.New(settlement.NewParams{
		ID:         uuid.New(),
		AuctionID:  uuid.New(),
		LotID:      uuid.New(),
		BidID:      uuid.New(),
		InvoiceID:  inv.ID,
		AssetID:    inv.AssetID,
		InvestorID: investorID,
		FromWallet: "0x00000000000000000000000000000000000000a1",
		ToWallet:   "0x00000000000000000000000000000000000000a2",
		Notional:   usd(notional),
		Price:      usd("9000.00"),
	}, issued)
	require.NoError(t, err)

	if finished {
		require.NoError(t, plan.Submit("tx-"+plan.ID.String(), issued))
		require.NoError(t, plan.ConfirmConsensus(issued))
		require.NoError(t, plan.ConfirmMirror(issued))
		require.NoError(t, plan.Account(issued))
	}
	require.NoError(t, f.store.Settlements().Create(context.Background(), f.store.Querier(), plan))
}

func (f *fixture) record(t *testing.T, inv *invoice.Invoice, amount string) (*collections.Result, error) {
	t.Helper()

	return f.service.Record(t.Context(), f.operator, collections.RecordParams{
		InvoiceID:  inv.ID,
		Amount:     usd(amount),
		Reference:  "SWIFT-2026-11-07-0042",
		ReceivedAt: testNow.Add(-time.Hour),
	})
}

/*
 * TestPaymentIsDividedAmongTheHolders is the whole feature: the debtor pays once, and every
 * party that ended up holding part of the receivable is credited its part — including the
 * issuer, for whatever it never sold.
 */
func TestPaymentIsDividedAmongTheHolders(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "6000.00")

	result, err := f.record(t, inv, "10000.00")
	require.NoError(t, err)

	assert.Equal(t, invoice.StatusMatured, result.Invoice.Status, "a receivable paid in full matures")
	assert.False(t, result.Repayment.IsShortfall())

	investorShare, ok := result.Repayment.ShareOf(f.investor.OrganizationID)
	require.True(t, ok)
	assert.Equal(t, "6000.00", investorShare.Amount.String())

	issuerShare, ok := result.Repayment.ShareOf(f.issuer.OrganizationID)
	require.True(t, ok, "the unsold part of the receivable is still the issuer's")
	assert.Equal(t, "4000.00", issuerShare.Amount.String())

	// The audit trail carries what happened, in the numbers a person would check.
	events := f.store.Audit().Events()
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	assert.Equal(t, collections.ActionRepaid, last.Action)
	assert.Equal(t, "10000.00", last.Detail["amount"])
	assert.Equal(t, "0.00", last.Detail["shortfall"])
}

// TestAnUnsoldReceivableIsPaidEntirelyToItsIssuer covers the batch that cleared nothing:
// the money still has an owner.
func TestAnUnsoldReceivableIsPaidEntirelyToItsIssuer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "")

	result, err := f.record(t, inv, "10000.00")
	require.NoError(t, err)

	require.Len(t, result.Repayment.Shares, 1)
	assert.Equal(t, f.issuer.OrganizationID, result.Repayment.Shares[0].PartyID)
	assert.Equal(t, "10000.00", result.Repayment.Shares[0].Amount.String())
}

/*
 * TestATransferInFlightMakesNobodyAHolder is the rule that keeps money from being paid to
 * an investor who never received the asset. A settlement that has not finished moved
 * nothing, so the notional behind it is still the issuer's.
 */
func TestATransferInFlightMakesNobodyAHolder(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "")
	f.transfer(t, inv, f.investor.OrganizationID, "6000.00", false)

	result, err := f.record(t, inv, "10000.00")
	require.NoError(t, err)

	require.Len(t, result.Repayment.Shares, 1)
	assert.Equal(t, f.issuer.OrganizationID, result.Repayment.Shares[0].PartyID)
	assert.Equal(t, "10000.00", result.Repayment.Shares[0].Amount.String())
}

// TestOneInvestorWithTwoAllocationsIsOneHolder: several winning bids on the same receivable
// are one position, not two claims on the whole.
func TestOneInvestorWithTwoAllocationsIsOneHolder(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "4000.00")
	f.transfer(t, inv, f.investor.OrganizationID, "3000.00", true)

	result, err := f.record(t, inv, "10000.00")
	require.NoError(t, err)

	require.Len(t, result.Repayment.Shares, 2)
	share, ok := result.Repayment.ShareOf(f.investor.OrganizationID)
	require.True(t, ok)
	assert.Equal(t, "7000.00", share.Amount.String())
}

/*
 * TestAShortPaymentDefaultsTheReceivable is the honest half of maturity. The money that
 * arrived is still divided, and the receivable is still not one that came good — an
 * investor reading its position has to be able to tell those apart.
 */
func TestAShortPaymentDefaultsTheReceivable(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "6000.00")

	result, err := f.record(t, inv, "7500.00")
	require.NoError(t, err)

	assert.Equal(t, invoice.StatusDefaulted, result.Invoice.Status)
	assert.Contains(t, result.Invoice.Reason, "7500.00")
	assert.True(t, result.Repayment.IsShortfall())
	assert.Equal(t, "2500.00", result.Repayment.Shortfall().String())

	share, ok := result.Repayment.ShareOf(f.investor.OrganizationID)
	require.True(t, ok)
	assert.Equal(t, "4500.00", share.Amount.String(), "the shortfall is shared, not absorbed")
}

/*
 * TestAReceivableIsRepaidOnce: a retried request returns the payment that was recorded and
 * credits nobody a second time, and a different payment against the same receivable is
 * refused rather than quietly replacing the first.
 */
func TestAReceivableIsRepaidOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "6000.00")

	first, err := f.record(t, inv, "10000.00")
	require.NoError(t, err)

	again, err := f.record(t, inv, "10000.00")
	require.NoError(t, err, "the same payment recorded twice is the same payment")
	assert.Equal(t, first.Repayment.ID, again.Repayment.ID)

	_, err = f.record(t, inv, "9000.00")
	require.ErrorIs(t, err, apperr.ErrConflict)
}

// TestOnlyAnOperatorRecordsAPayment: the debtor pays the platform, so the seller does not
// get to declare that its own obligation ended.
func TestOnlyAnOperatorRecordsAPayment(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "6000.00")

	_, err := f.service.Record(t.Context(), f.issuer, collections.RecordParams{
		InvoiceID: inv.ID, Amount: usd("10000.00"),
		Reference: "SWIFT-1", ReceivedAt: testNow.Add(-time.Hour),
	})
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

// TestOnlyASettledReceivableIsRepaid keeps the lifecycle honest: money cannot arrive
// against paper that was never sold.
func TestOnlyASettledReceivableIsRepaid(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  f.issuer.OrganizationID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0008",
		Face:      usd("10000.00"),
		IssuedAt:  issued,
		DueAt:     testNow,
	}, issued)
	require.NoError(t, err)
	f.store.PutInvoice(inv)

	_, err = f.record(t, inv, "10000.00")
	require.ErrorIs(t, err, apperr.ErrConflict)
}

/*
 * TestDefaultWaitsForTheDueDate. A receivable is not in default because somebody is
 * impatient, and the date it was due is on the invoice for exactly this reason.
 */
func TestDefaultWaitsForTheDueDate(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "6000.00")

	_, err := f.service.Default(t.Context(), f.operator, inv.ID, "the debtor stopped answering")
	require.ErrorIs(t, err, apperr.ErrConflict, "the receivable is due today, not overdue")

	late := collections.NewService(collections.Config{
		DB: f.store, Invoices: f.store.Invoices(), Settlements: f.store.Settlements(),
		Repayments: f.store.Repayments(), Audit: f.store.Audit(),
		Now: func() time.Time { return testNow.Add(30 * 24 * time.Hour) },
	})

	closed, err := late.Default(t.Context(), f.operator, inv.ID, "the debtor stopped answering")
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusDefaulted, closed.Status)
	assert.Equal(t, "the debtor stopped answering", closed.Reason)

	_, err = late.Default(t.Context(), f.issuer, inv.ID, "not mine to declare")
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

/*
 * TestARepaymentIsReadableByTheHolders and by nobody else: the issuer that sold the paper,
 * the parties it was divided among, and an operator. A stranger is told it does not exist,
 * because confirming that a receivable was repaid says something about it.
 */
func TestARepaymentIsReadableByTheHolders(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "6000.00")
	_, err := f.record(t, inv, "10000.00")
	require.NoError(t, err)

	for _, reader := range []collections.Actor{f.issuer, f.investor, f.operator} {
		_, err := f.service.Get(t.Context(), reader, inv.ID)
		require.NoError(t, err)
	}

	_, err = f.service.Get(t.Context(), f.other, inv.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)

	received, err := f.service.Received(t.Context(), f.investor, 0)
	require.NoError(t, err)
	require.Len(t, received, 1)
	assert.Equal(t, inv.ID, received[0].InvoiceID)

	none, err := f.service.Received(t.Context(), f.other, 0)
	require.NoError(t, err)
	assert.Empty(t, none, "an investor sees the payments it held a share of")
}

/*
 * TestHoldingsAnswerWhatDoIOwn. Until this existed, an investor could see what it had bid
 * and what had come back, and nothing in between — the position itself, which is the thing
 * it actually holds.
 */
func TestHoldingsAnswerWhatDoIOwn(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "6000.00")

	held, err := f.service.Holdings(t.Context(), f.investor, 0)
	require.NoError(t, err)
	require.Len(t, held, 1)

	assert.Equal(t, inv.ID, held[0].Invoice.ID)
	assert.Equal(t, "6000.00", held[0].Settlement.Notional.String())
	assert.True(t, held[0].Settlement.IsFinished())
	assert.Nil(t, held[0].Repayment, "the debtor has not paid yet")

	// Once the debtor pays, the same position carries what it returned.
	_, err = f.record(t, inv, "10000.00")
	require.NoError(t, err)

	held, err = f.service.Holdings(t.Context(), f.investor, 0)
	require.NoError(t, err)
	require.Len(t, held, 1)
	require.NotNil(t, held[0].Repayment)

	share, ok := held[0].Repayment.ShareOf(f.investor.OrganizationID)
	require.True(t, ok)
	assert.Equal(t, "6000.00", share.Amount.String())

	none, err := f.service.Holdings(t.Context(), f.other, 0)
	require.NoError(t, err)
	assert.Empty(t, none, "an investor holds what it bought and nothing else")
}

/*
 * TestAHoldingInFlightIsStillShown. An investor whose transfer is stuck is looking at money
 * it has committed, and a venue that hid the position until it completed would be silent at
 * exactly the moment it matters.
 */
func TestAHoldingInFlightIsStillShown(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "")
	f.transfer(t, inv, f.investor.OrganizationID, "6000.00", false)

	held, err := f.service.Holdings(t.Context(), f.investor, 0)
	require.NoError(t, err)
	require.Len(t, held, 1)
	assert.False(t, held[0].Settlement.IsFinished(), "and it says so rather than looking done")
}

// TestRepaymentOverHTTP checks the wire shape and the refusals a caller actually meets.
func TestRepaymentOverHTTP(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.settledInvoice(t, "6000.00")
	path := "/api/v1/invoices/" + inv.ID.String() + "/repayment"

	body := `{"amount":"10000.00","currency":"USD","reference":"SWIFT-2026-11-07-0042",` +
		`"received_at":"2026-11-07T11:00:00Z"}`

	assert.Equal(t, http.StatusForbidden, f.call(t, http.MethodPost, path, "issuer", body).Code,
		"only an operator records a payment")

	rec := f.call(t, http.MethodPost, path, "operator", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	var created map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "10000.00", created["amount"], "money crosses the wire as a decimal string")
	assert.Equal(t, "0.00", created["shortfall"])
	assert.Equal(t, false, created["is_shortfall"])
	assert.Equal(t, "USD", created["currency"])

	shares, ok := created["shares"].([]any)
	require.True(t, ok)
	require.Len(t, shares, 2)

	read := f.call(t, http.MethodGet, path, "investor", "")
	require.Equal(t, http.StatusOK, read.Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, http.MethodGet, path, "other", "").Code)

	list := f.call(t, http.MethodGet, "/api/v1/repayments", "investor", "")
	require.Equal(t, http.StatusOK, list.Code)
	var listed struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listed))
	require.Len(t, listed.Items, 1)
	assert.Equal(t, inv.ID.String(), listed.Items[0]["invoice_id"])

	assert.Equal(t, http.StatusUnprocessableEntity,
		f.call(t, http.MethodPost, path, "operator", `{"amount":"nope","currency":"USD","reference":"x"}`).Code)
	assert.Equal(t, http.StatusUnprocessableEntity,
		f.call(t, http.MethodPost, "/api/v1/invoices/not-a-uuid/repayment", "operator", body).Code)

	// A holding carries the terms and the transfer, and the reader's own share once paid.
	holdings := f.call(t, http.MethodGet, "/api/v1/holdings", "investor", "")
	require.Equal(t, http.StatusOK, holdings.Code)

	var owned struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(holdings.Body.Bytes(), &owned))
	require.Len(t, owned.Items, 1)
	assert.Equal(t, "INV-2026-0007", owned.Items[0]["number"])
	assert.Equal(t, "6000.00", owned.Items[0]["notional"])
	assert.Equal(t, "6000.00", owned.Items[0]["received"])
	assert.Equal(t, true, owned.Items[0]["settled"])
	assert.Equal(t, "MATURED", owned.Items[0]["status"])
}
