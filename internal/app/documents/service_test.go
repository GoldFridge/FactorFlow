package documents_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/documents"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/objects"
	"github.com/GoldFridge/factorflow/internal/platform/pgtest"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

var testNow = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

/*
 * The upload is tested against the real database rather than a fake store.
 *
 * What it has to get right is a transaction spanning two tables and a state change, and a
 * fake with its own idea of transactions would be checking itself.
 */
type fixture struct {
	db      *postgres.DB
	service *documents.Service
	router  http.Handler
	issuer  documents.Actor
	other   documents.Actor
	invoice *invoice.Invoice
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	db := pgtest.New(t)
	ctx := context.Background()

	issuerID := seedOrganization(t, db, "0x00000000000000000000000000000000000000a1")
	otherID := seedOrganization(t, db, "0x00000000000000000000000000000000000000b2")

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  issuerID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0001",
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow.Add(-24 * time.Hour),
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, invoice.NewPostgresRepository().Create(ctx, db.Querier(), inv))

	service := documents.NewService(documents.Config{
		DB:       db,
		Invoices: invoice.NewPostgresRepository(),
		Objects:  objects.NewPostgresStore(),
		Audit:    audit.NewPostgresRecorder(),
		Now:      func() time.Time { return testNow },
	})

	f := &fixture{
		db:      db,
		service: service,
		issuer:  documents.Actor{OrganizationID: issuerID},
		other:   documents.Actor{OrganizationID: otherID},
		invoice: inv,
	}

	handler := documents.NewHandler(service)
	f.router = httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					actor := httpserver.Actor{OrganizationID: issuerID, Eligible: true}
					if req.Header.Get("X-Test-Actor") == "stranger" {
						actor = httpserver.Actor{OrganizationID: otherID, Eligible: true}
					}
					next.ServeHTTP(w, req.WithContext(httpserver.ContextWithActor(req.Context(), actor)))
				})
			})
			handler.Routes(r)
		},
	})
	return f
}

func seedOrganization(t *testing.T, db *postgres.DB, wallet string) uuid.UUID {
	t.Helper()

	org, err := organization.New(organization.NewParams{
		ID: uuid.New(), Type: organization.TypeIssuer, Name: "Issuer " + wallet[2:6], Wallet: wallet,
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, org.Approve(testNow))
	require.NoError(t, organization.NewPostgresRepository().Create(context.Background(), db.Querier(), org))
	return org.ID
}

func ciphertext() []byte {
	return []byte("not a pdf, and nothing here can tell: this is what the platform keeps")
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

/*
 * The platform stores bytes it cannot read, and the digest it records is of the bytes it
 * actually stored — not of what the uploader said it was sending.
 */
func TestUploadStoresTheCiphertextAndMovesTheInvoice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	blob := ciphertext()

	result, err := f.service.Upload(ctx, f.issuer, documents.UploadParams{
		InvoiceID:  f.invoice.ID,
		Ciphertext: blob,
		MIME:       "application/pdf",
		KeyRef:     "browser-local",
	})
	require.NoError(t, err)

	assert.Equal(t, invoice.StatusUploaded, result.Invoice.Status)
	assert.Equal(t, digestOf(blob), result.Document.CipherHash)
	assert.Equal(t, int64(len(blob)), result.Document.SizeBytes)
	assert.Contains(t, result.Document.ObjectKey, f.invoice.ID.String())

	stored, err := f.service.Fetch(ctx, f.issuer, f.invoice.ID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(blob, stored.Ciphertext), "what comes back is what went in")

	// The timeline records the digest, never the document.
	events, err := audit.NewPostgresRecorder().Timeline(ctx, f.db.Querier(),
		invoice.EntityType, f.invoice.ID.String(), 0)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, invoice.ActionDocumentAttached, events[0].Action)
	assert.Equal(t, digestOf(blob), events[0].Detail["cipher_hash"])
}

// Uploading the same bytes twice is one object, so a retried upload is indistinguishable
// from the first attempt.
func TestASecondUploadOfTheSameBytesIsTheSameObject(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first, err := f.service.Upload(ctx, f.issuer, documents.UploadParams{
		InvoiceID: f.invoice.ID, Ciphertext: ciphertext(), MIME: "application/pdf", KeyRef: "browser-local",
	})
	require.NoError(t, err)

	// The invoice has left DRAFT, so the second attempt is refused by the state machine
	// rather than quietly writing a second document.
	_, err = f.service.Upload(ctx, f.issuer, documents.UploadParams{
		InvoiceID: f.invoice.ID, Ciphertext: ciphertext(), MIME: "application/pdf", KeyRef: "browser-local",
	})
	require.ErrorIs(t, err, apperr.ErrConflict)

	var stored int
	require.NoError(t, f.db.Querier().QueryRow(ctx,
		`SELECT count(*) FROM encrypted_objects WHERE owner_id = $1`, f.invoice.ID).Scan(&stored))
	assert.Equal(t, 1, stored)
	assert.NotEmpty(t, first.Document.ObjectKey)
}

// A failed upload leaves nothing behind: no blob, no metadata, no state change.
func TestARefusedUploadStoresNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.service.Upload(ctx, f.issuer, documents.UploadParams{
		InvoiceID: f.invoice.ID, Ciphertext: ciphertext(), MIME: "image/png", KeyRef: "browser-local",
	})
	require.ErrorIs(t, err, apperr.ErrValidation)

	var stored int
	require.NoError(t, f.db.Querier().QueryRow(ctx,
		`SELECT count(*) FROM encrypted_objects`).Scan(&stored))
	assert.Zero(t, stored, "the blob was rolled back with the metadata that failed")

	inv, err := invoice.NewPostgresRepository().Get(ctx, f.db.Querier(), f.invoice.ID)
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusDraft, inv.Status)
}

func TestAnotherOrganizationCanNeitherUploadNorRead(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.service.Upload(ctx, f.other, documents.UploadParams{
		InvoiceID: f.invoice.ID, Ciphertext: ciphertext(), MIME: "application/pdf", KeyRef: "browser-local",
	})
	require.ErrorIs(t, err, apperr.ErrNotFound, "a stranger is not told the invoice exists")

	_, err = f.service.Upload(ctx, f.issuer, documents.UploadParams{
		InvoiceID: f.invoice.ID, Ciphertext: ciphertext(), MIME: "application/pdf", KeyRef: "browser-local",
	})
	require.NoError(t, err)

	_, err = f.service.Fetch(ctx, f.other, f.invoice.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

func TestUploadValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.service.Upload(ctx, f.issuer, documents.UploadParams{
		InvoiceID: f.invoice.ID, MIME: "application/pdf", KeyRef: "browser-local",
	})
	assert.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.service.Upload(ctx, f.issuer, documents.UploadParams{
		InvoiceID: f.invoice.ID, Ciphertext: ciphertext(), MIME: "application/pdf",
	})
	assert.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.service.Upload(ctx, f.issuer, documents.UploadParams{
		InvoiceID: uuid.New(), Ciphertext: ciphertext(), MIME: "application/pdf", KeyRef: "k",
	})
	assert.ErrorIs(t, err, apperr.ErrNotFound)
}

func TestUploadOverHTTP(t *testing.T) {
	f := newFixture(t)
	blob := ciphertext()

	body, err := json.Marshal(map[string]string{
		"ciphertext": base64.StdEncoding.EncodeToString(blob),
		"mime":       "application/pdf",
		"key_ref":    "browser-local",
	})
	require.NoError(t, err)

	path := "/api/v1/invoices/" + f.invoice.ID.String() + "/document/content"
	rec := f.do(t, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)), "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var answer map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &answer))
	assert.Equal(t, digestOf(blob), answer["cipher_hash"])
	assert.Equal(t, "UPLOADED", answer["status"])

	// The ciphertext comes back as bytes, unchanged and still unreadable.
	rec = f.do(t, httptest.NewRequest(http.MethodGet, path, http.NoBody), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, bytes.Equal(blob, rec.Body.Bytes()))
	assert.Equal(t, digestOf(blob), rec.Header().Get("X-Cipher-Hash"))
	assert.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))

	// And not to anyone else.
	rec = f.do(t, httptest.NewRequest(http.MethodGet, path, http.NoBody), "stranger")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestUploadRejectsWhatIsNotBase64(t *testing.T) {
	f := newFixture(t)

	path := "/api/v1/invoices/" + f.invoice.ID.String() + "/document/content"
	rec := f.do(t, httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(`{"ciphertext":"not base64!","mime":"application/pdf","key_ref":"k"}`)), "")

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
}

func (f *fixture) do(t *testing.T, req *http.Request, actor string) *httptest.ResponseRecorder {
	t.Helper()

	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}
