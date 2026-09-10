-- +goose Up

-- The fingerprint is the economic identity of a receivable: which debtor owes how much,
-- under which number, by when. It deliberately excludes the issuer.
--
-- The existing guard stops one issuer submitting the same invoice twice. The fraud it
-- cannot see is the one factoring actually suffers from: the same receivable sold to two
-- financiers, which here is two accounts. Both would be lending against one payment that
-- can only arrive once.
--
-- Production needs the debtor's confirmation or a receivables registry. A hash of the terms
-- is the honest demo version of that, and it says so.
ALTER TABLE invoices ADD COLUMN fingerprint TEXT;

-- Rows written before this column existed are fingerprinted here rather than left outside
-- the rule. The expression mirrors invoice.Fingerprint in Go, and a test recomputes a
-- stored row's fingerprint to prove the two still agree.
-- +goose StatementBegin
UPDATE invoices
   SET fingerprint = encode(sha256(convert_to(
           lower(btrim(debtor_ref)) || '|' ||
           btrim(number) || '|' ||
           face_minor::text || '|' ||
           currency || '|' ||
           to_char(due_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), 'UTF8')), 'hex');
-- +goose StatementEnd

ALTER TABLE invoices ALTER COLUMN fingerprint SET NOT NULL;
ALTER TABLE invoices ADD CONSTRAINT invoices_fingerprint_format
    CHECK (fingerprint ~ '^[0-9a-f]{64}$');

-- One live receivable per set of terms, across the whole venue. A receivable that was
-- rejected, matured or defaulted releases its terms: the payment it referred to has either
-- arrived or been written off, so the same paper cannot be double-financed through it.
CREATE UNIQUE INDEX invoices_active_fingerprint_key
    ON invoices (fingerprint)
    WHERE status NOT IN ('REJECTED', 'MATURED', 'DEFAULTED');

-- +goose Down
DROP INDEX IF EXISTS invoices_active_fingerprint_key;
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_fingerprint_format;
ALTER TABLE invoices DROP COLUMN IF EXISTS fingerprint;
