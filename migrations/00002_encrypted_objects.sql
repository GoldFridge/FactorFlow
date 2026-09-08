-- +goose Up

-- Encrypted objects are the ciphertexts themselves.
--
-- The platform stores bytes it cannot read: the key is generated in the browser and never
-- sent, so this table is the one place where a document exists at all, and it exists as
-- something no operator, no backup and no leak can turn back into an invoice.
--
-- The digest is computed by the server over the bytes it actually stored, rather than taken
-- from the uploader. A hash supplied by the client would only prove what the client claimed.
CREATE TABLE encrypted_objects (
    object_key  TEXT PRIMARY KEY CHECK (length(btrim(object_key)) > 0),
    -- owner_id is the record the object belongs to. It is deliberately not a foreign key to
    -- any one table: this is platform storage, and it must not know what it is holding.
    owner_id    UUID        NOT NULL,
    ciphertext  BYTEA       NOT NULL,
    size_bytes  BIGINT      NOT NULL CHECK (size_bytes > 0),
    cipher_hash TEXT        NOT NULL CHECK (cipher_hash ~ '^[0-9a-f]{64}$'),
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX encrypted_objects_owner_idx ON encrypted_objects (owner_id);

-- +goose Down
DROP TABLE IF EXISTS encrypted_objects;
