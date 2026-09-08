package reporting_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/reporting"
	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// offer puts the receivable in a batch, which is the act that makes its terms readable by
// the participants who are being asked to price it.
func (f *fixture) offer(t *testing.T, inv *invoice.Invoice, a *risk.Assessment, open bool) *auction.Auction {
	t.Helper()

	batch, err := auction.NewAuction(auction.NewAuctionParams{
		ID:       uuid.New(),
		IssuerID: inv.IssuerID,
		Lots: []auction.Lot{{
			ID:           uuid.New(),
			InvoiceID:    inv.ID,
			AssetID:      uuid.New(),
			IssuerID:     inv.IssuerID,
			DebtorRef:    inv.DebtorRef,
			Supply:       inv.Face,
			ReservePrice: a.ReservePrice,
			Grade:        a.Grade,
			TenorDays:    inv.TenorDays(),
		}},
		OpensAt:  testNow,
		ClosesAt: testNow.Add(24 * time.Hour),
	}, testNow)
	require.NoError(t, err)

	if open {
		require.NoError(t, batch.Open(testNow))
	}
	require.NoError(t, f.store.Auctions().CreateAuction(context.Background(), f.store.Querier(), batch))
	return batch
}

/*
 * TestListingIsReadableByTheVenue is the whole point of the endpoint: a bidder is asked for
 * money against terms it must be allowed to read, and the reasoning behind the price is
 * part of those terms rather than a courtesy.
 */
func TestListingIsReadableByTheVenue(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, assessment, _ := f.assessedInvoice(t, f.issuer.OrganizationID)
	batch := f.offer(t, inv, assessment, true)

	disclosed, err := f.service.ListingFor(t.Context(), f.stranger, inv.ID)
	require.NoError(t, err)

	assert.Equal(t, inv.ID, disclosed.Invoice.ID)
	assert.Equal(t, batch.ID, disclosed.Listing.AuctionID)
	assert.Equal(t, auction.StatusOpen, disclosed.Listing.Status)
	assert.False(t, disclosed.Own, "the reader is not the issuer")

	require.NotNil(t, disclosed.Report, "a bidder may see why the reserve price is what it is")
	assert.Equal(t, assessment.ReservePrice, disclosed.Report.Assessment.ReservePrice)
	require.NotNil(t, disclosed.Report.Snapshot, "and the market it was priced against")
}

// TestListingTellsTheIssuerItIsTheirs, because the issuer has a fuller record to open and
// the screen is the one place that can say so.
func TestListingTellsTheIssuerItIsTheirs(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, assessment, _ := f.assessedInvoice(t, f.issuer.OrganizationID)
	f.offer(t, inv, assessment, true)

	disclosed, err := f.service.ListingFor(t.Context(), f.issuer, inv.ID)
	require.NoError(t, err)
	assert.True(t, disclosed.Own)
}

/*
 * TestUnlistedReceivablesStayPrivate is the rule this feature must not erode. Disclosure
 * follows the offer and nothing else: a receivable its issuer never put on the board is as
 * invisible as it was before this endpoint existed, and so is one still sitting in a draft
 * batch that can still be abandoned.
 */
func TestUnlistedReceivablesStayPrivate(t *testing.T) {
	t.Parallel()

	t.Run("never offered", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		inv, _, _ := f.assessedInvoice(t, f.issuer.OrganizationID)

		_, err := f.service.ListingFor(t.Context(), f.stranger, inv.ID)
		require.ErrorIs(t, err, apperr.ErrNotFound)

		_, err = f.service.AssessmentFor(t.Context(), f.stranger, inv.ID)
		require.ErrorIs(t, err, apperr.ErrNotFound)
	})

	t.Run("still a draft batch", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		inv, assessment, _ := f.assessedInvoice(t, f.issuer.OrganizationID)
		f.offer(t, inv, assessment, false)

		_, err := f.service.ListingFor(t.Context(), f.stranger, inv.ID)
		require.ErrorIs(t, err, apperr.ErrNotFound)

		disclosed, err := f.service.ListingFor(t.Context(), f.issuer, inv.ID)
		require.NoError(t, err, "the issuer reads their own draft")
		assert.Equal(t, auction.StatusDraft, disclosed.Listing.Status)
	})
}

// TestListingRequiresSomeone keeps an anonymous caller out: the venue is not public.
func TestListingRequiresSomeone(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, assessment, _ := f.assessedInvoice(t, f.issuer.OrganizationID)
	f.offer(t, inv, assessment, true)

	_, err := f.service.ListingFor(t.Context(), reporting.Actor{}, inv.ID)
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

/*
 * TestListingHistoryStaysWithTheIssuer draws the line the disclosure stops at. Being asked
 * to price a receivable is not being handed the seller's file: the terms and the score are
 * on the board, the document and the history are not.
 */
func TestListingHistoryStaysWithTheIssuer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, assessment, _ := f.assessedInvoice(t, f.issuer.OrganizationID)
	f.offer(t, inv, assessment, true)

	_, err := f.service.TimelineFor(t.Context(), f.stranger, inv.ID, 100)
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestListingOverHTTP checks the wire shape a bidder actually receives.
func TestListingOverHTTP(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, assessment, _ := f.assessedInvoice(t, f.issuer.OrganizationID)
	batch := f.offer(t, inv, assessment, true)

	rec := f.get(t, "/api/v1/listings/"+inv.ID.String(), "stranger")
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, inv.ID.String(), body["invoice_id"])
	assert.Equal(t, inv.Number, body["number"])
	assert.Equal(t, inv.Face.String(), body["face"], "money crosses the wire as a decimal string")
	assert.Equal(t, "USD", body["currency"])
	assert.Equal(t, float64(inv.TenorDays()), body["tenor_days"])
	assert.Equal(t, batch.ID.String(), body["auction_id"])
	assert.Equal(t, "OPEN", body["auction_status"])
	assert.Equal(t, false, body["own"])

	priced, ok := body["assessment"].(map[string]any)
	require.True(t, ok, "the price comes with the terms")
	assert.Equal(t, assessment.ReservePrice.String(), priced["reserve_price"])

	// What is absent is the point: the disclosure carries no document and no way to ask
	// for one.
	for _, field := range []string{"object_key", "cipher_hash", "document", "version"} {
		assert.NotContains(t, body, field)
	}

	assert.Equal(t, http.StatusNotFound,
		f.get(t, "/api/v1/listings/"+uuid.New().String(), "stranger").Code)
	assert.Equal(t, http.StatusUnprocessableEntity,
		f.get(t, "/api/v1/listings/not-a-uuid", "stranger").Code)
}
