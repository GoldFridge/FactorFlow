package invoice

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
)

// Document limits. Size and type are checked before the confidential workflow is invoked,
// so an oversized or unexpected blob is rejected at the cheap end of the pipeline.
const (
	// MaxDocumentBytes bounds an uploaded ciphertext (10 MiB).
	MaxDocumentBytes = 10 << 20
	// MaxObjectKeyLen bounds the server-generated object key.
	MaxObjectKeyLen = 512
	// MaxKeyRefLen bounds the reference to the wrapped data key.
	MaxKeyRefLen = 512
)

// allowedMIMETypes are the document types the confidential workflow can parse.
var allowedMIMETypes = map[string]struct{}{
	"application/pdf":  {},
	"application/json": {},
}

// sha256Hex matches the lowercase hex digest of the uploaded ciphertext.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Document is the metadata of one encrypted invoice document.
//
// It deliberately cannot hold the document: FactorFlow stores ciphertext in object
// storage and plaintext nowhere. CipherHash binds the stored bytes to the invoice, and
// KeyRef points at the wrapped data key, never at the key itself.
type Document struct {
	InvoiceID  uuid.UUID
	ObjectKey  string
	CipherHash string
	KeyRef     string
	MIME       string
	SizeBytes  int64
	UploadedAt time.Time
}

// NewDocumentParams carries the metadata recorded after a client-side encrypted upload.
type NewDocumentParams struct {
	InvoiceID  uuid.UUID
	ObjectKey  string
	CipherHash string
	KeyRef     string
	MIME       string
	SizeBytes  int64
}

// NewDocument validates and builds document metadata.
func NewDocument(p NewDocumentParams, now time.Time) (*Document, error) {
	var violations []error

	if p.InvoiceID == uuid.Nil {
		violations = append(violations, apperr.Invalid("invoice_id", "must be a non-nil UUID"))
	}

	objectKey := strings.TrimSpace(p.ObjectKey)
	switch {
	case objectKey == "":
		violations = append(violations, apperr.Invalid("object_key", "must not be empty"))
	case len(objectKey) > MaxObjectKeyLen:
		violations = append(violations, apperr.Invalid("object_key", "must be at most %d characters", MaxObjectKeyLen))
	}

	cipherHash := strings.ToLower(strings.TrimSpace(p.CipherHash))
	if !sha256Hex.MatchString(cipherHash) {
		violations = append(violations, apperr.Invalid("cipher_hash", "must be a lowercase hex SHA-256 digest"))
	}

	keyRef := strings.TrimSpace(p.KeyRef)
	switch {
	case keyRef == "":
		violations = append(violations, apperr.Invalid("key_ref", "must not be empty"))
	case len(keyRef) > MaxKeyRefLen:
		violations = append(violations, apperr.Invalid("key_ref", "must be at most %d characters", MaxKeyRefLen))
	}

	mime := strings.ToLower(strings.TrimSpace(p.MIME))
	if _, ok := allowedMIMETypes[mime]; !ok {
		violations = append(violations, apperr.Invalid("mime", "must be one of application/pdf, application/json"))
	}

	switch {
	case p.SizeBytes <= 0:
		violations = append(violations, apperr.Invalid("size_bytes", "must be greater than zero"))
	case p.SizeBytes > MaxDocumentBytes:
		violations = append(violations, apperr.Invalid("size_bytes", "must be at most %d bytes", int64(MaxDocumentBytes)))
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	return &Document{
		InvoiceID:  p.InvoiceID,
		ObjectKey:  objectKey,
		CipherHash: cipherHash,
		KeyRef:     keyRef,
		MIME:       mime,
		SizeBytes:  p.SizeBytes,
		UploadedAt: now.UTC(),
	}, nil
}

// MatchesCipherHash reports whether a digest observed elsewhere, such as inside the TEE
// after download, is the one recorded at upload time.
func (d *Document) MatchesCipherHash(digest string) bool {
	return d.CipherHash == strings.ToLower(strings.TrimSpace(digest))
}
