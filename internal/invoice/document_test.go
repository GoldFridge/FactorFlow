package invoice_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

const testCipherHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func validDocumentParams() invoice.NewDocumentParams {
	return invoice.NewDocumentParams{
		InvoiceID:  uuid.New(),
		ObjectKey:  "invoices/2026/09/9f86d081.enc",
		CipherHash: testCipherHash,
		KeyRef:     "cre-secret://data-key/01J8Z9",
		MIME:       "application/pdf",
		SizeBytes:  482_113,
	}
}

func TestNewDocument(t *testing.T) {
	t.Parallel()

	p := validDocumentParams()
	doc, err := invoice.NewDocument(p, testNow)
	require.NoError(t, err)

	assert.Equal(t, p.InvoiceID, doc.InvoiceID)
	assert.Equal(t, p.CipherHash, doc.CipherHash)
	assert.Equal(t, testNow, doc.UploadedAt)
}

func TestNewDocumentNormalizes(t *testing.T) {
	t.Parallel()

	p := validDocumentParams()
	p.CipherHash = "  " + strings.ToUpper(testCipherHash) + "  "
	p.MIME = " APPLICATION/PDF "
	p.ObjectKey = " invoices/2026/09/9f86d081.enc "

	doc, err := invoice.NewDocument(p, testNow)
	require.NoError(t, err)

	assert.Equal(t, testCipherHash, doc.CipherHash, "digests are compared lowercase")
	assert.Equal(t, "application/pdf", doc.MIME)
	assert.Equal(t, "invoices/2026/09/9f86d081.enc", doc.ObjectKey)
}

func TestNewDocumentRejectsBadMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*invoice.NewDocumentParams)
		wantField string
	}{
		{name: "nil invoice", mutate: func(p *invoice.NewDocumentParams) { p.InvoiceID = uuid.Nil }, wantField: "invoice_id"},
		{name: "empty object key", mutate: func(p *invoice.NewDocumentParams) { p.ObjectKey = " " }, wantField: "object_key"},
		{name: "long object key", mutate: func(p *invoice.NewDocumentParams) { p.ObjectKey = longString(invoice.MaxObjectKeyLen + 1) }, wantField: "object_key"},
		{name: "short hash", mutate: func(p *invoice.NewDocumentParams) { p.CipherHash = "abc123" }, wantField: "cipher_hash"},
		{name: "non-hex hash", mutate: func(p *invoice.NewDocumentParams) { p.CipherHash = strings.Repeat("z", 64) }, wantField: "cipher_hash"},
		{name: "missing hash", mutate: func(p *invoice.NewDocumentParams) { p.CipherHash = "" }, wantField: "cipher_hash"},
		{name: "empty key ref", mutate: func(p *invoice.NewDocumentParams) { p.KeyRef = "" }, wantField: "key_ref"},
		{name: "long key ref", mutate: func(p *invoice.NewDocumentParams) { p.KeyRef = longString(invoice.MaxKeyRefLen + 1) }, wantField: "key_ref"},
		{name: "unsupported type", mutate: func(p *invoice.NewDocumentParams) { p.MIME = "image/png" }, wantField: "mime"},
		{name: "zero size", mutate: func(p *invoice.NewDocumentParams) { p.SizeBytes = 0 }, wantField: "size_bytes"},
		{name: "negative size", mutate: func(p *invoice.NewDocumentParams) { p.SizeBytes = -1 }, wantField: "size_bytes"},
		{name: "oversized upload", mutate: func(p *invoice.NewDocumentParams) { p.SizeBytes = invoice.MaxDocumentBytes + 1 }, wantField: "size_bytes"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := validDocumentParams()
			tc.mutate(&p)

			doc, err := invoice.NewDocument(p, testNow)
			require.Nil(t, doc)
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Contains(t, fieldNames(apperr.Fields(err)), tc.wantField)
		})
	}
}

func TestMatchesCipherHash(t *testing.T) {
	t.Parallel()

	doc, err := invoice.NewDocument(validDocumentParams(), testNow)
	require.NoError(t, err)

	assert.True(t, doc.MatchesCipherHash(testCipherHash))
	assert.True(t, doc.MatchesCipherHash(" "+strings.ToUpper(testCipherHash)+" "))
	assert.False(t, doc.MatchesCipherHash(strings.Repeat("0", 64)), "a substituted document is detected")
	assert.False(t, doc.MatchesCipherHash(""))
}
