// Package objects stores ciphertexts the platform cannot read.
//
// It is deliberately ignorant of what it holds: an object has a key, an owner and bytes.
// The bytes were encrypted in a browser with a key that was never sent, so storing them is
// storing something a leak cannot turn back into a document — which only stays true if
// nothing here is ever tempted to look inside.
package objects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// MaxObjectBytes bounds one stored object. It is generous for a scanned document and small
// enough that a request cannot be used to fill the database.
const MaxObjectBytes = 8 << 20

// Object is one stored ciphertext.
type Object struct {
	Key     string
	OwnerID uuid.UUID
	// Ciphertext is opaque. Nothing in this package interprets it.
	Ciphertext []byte
	SizeBytes  int64
	// CipherHash is the digest of the stored bytes, computed here rather than accepted from
	// a caller: a hash supplied by an uploader proves only what the uploader claimed.
	CipherHash string
	CreatedAt  time.Time
}

// Store keeps ciphertexts.
type Store interface {
	Put(ctx context.Context, q postgres.Querier, object *Object) error
	Get(ctx context.Context, q postgres.Querier, key string) (*Object, error)
}

// New builds an object, deriving its key from what it holds.
//
// The key is a function of the owner and the digest, so uploading identical bytes twice
// produces one object rather than two: a retried upload is then indistinguishable from the
// first, which is what makes the write safe to repeat.
func New(ownerID uuid.UUID, ciphertext []byte, now time.Time) (*Object, error) {
	if ownerID == uuid.Nil {
		return nil, apperr.Invalid("owner_id", "must be a non-nil UUID")
	}
	switch {
	case len(ciphertext) == 0:
		return nil, apperr.Invalid("ciphertext", "must not be empty")
	case len(ciphertext) > MaxObjectBytes:
		return nil, apperr.Invalid("ciphertext", "must be at most %d bytes", MaxObjectBytes)
	}

	sum := sha256.Sum256(ciphertext)
	digest := hex.EncodeToString(sum[:])

	return &Object{
		Key:        fmt.Sprintf("%s/%s.enc", ownerID, digest[:16]),
		OwnerID:    ownerID,
		Ciphertext: ciphertext,
		SizeBytes:  int64(len(ciphertext)),
		CipherHash: digest,
		CreatedAt:  now.UTC(),
	}, nil
}

// PostgresStore is the PostgreSQL implementation of Store.
type PostgresStore struct{}

// NewPostgresStore returns the store.
func NewPostgresStore() *PostgresStore { return &PostgresStore{} }

// Put writes the object, and does nothing when those exact bytes are already stored under
// that key.
func (s *PostgresStore) Put(ctx context.Context, q postgres.Querier, object *Object) error {
	if object == nil {
		return apperr.Invalid("object", "must not be nil")
	}
	if strings.TrimSpace(object.Key) == "" {
		return apperr.Invalid("object_key", "must not be empty")
	}

	const query = `
		INSERT INTO encrypted_objects (object_key, owner_id, ciphertext, size_bytes, cipher_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (object_key) DO NOTHING`

	if _, err := q.Exec(ctx, query, object.Key, object.OwnerID, object.Ciphertext,
		object.SizeBytes, object.CipherHash, object.CreatedAt); err != nil {
		return fmt.Errorf("storing object %s: %w", object.Key, err)
	}
	return nil
}

// Get returns one object.
func (s *PostgresStore) Get(ctx context.Context, q postgres.Querier, key string) (*Object, error) {
	const query = `
		SELECT object_key, owner_id, ciphertext, size_bytes, cipher_hash, created_at
		FROM encrypted_objects WHERE object_key = $1`

	var object Object
	err := q.QueryRow(ctx, query, key).Scan(&object.Key, &object.OwnerID, &object.Ciphertext,
		&object.SizeBytes, &object.CipherHash, &object.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("object %s", key)
		}
		return nil, fmt.Errorf("reading object %s: %w", key, err)
	}

	object.CreatedAt = object.CreatedAt.UTC()
	return &object, nil
}
