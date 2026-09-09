-- +goose Up

-- A repayment is what the debtor finally paid against one receivable.
--
-- The unique constraint on invoice_id is the rule rather than an optimisation: a
-- receivable is repaid once, and crediting the holders twice for one payment is the kind
-- of mistake that must be impossible in storage rather than merely unlikely in code.
--
-- Face is kept beside the amount because the difference is the whole question at maturity:
-- a payment short of the face is a credit event, not a rounding note, and reading the
-- invoice later to find out what was owed would report today's record of an old event.
CREATE TABLE repayments (
    id           UUID PRIMARY KEY,
    invoice_id   UUID        NOT NULL UNIQUE REFERENCES invoices (id) ON DELETE CASCADE,
    face_minor   BIGINT      NOT NULL CHECK (face_minor > 0),
    amount_minor BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency     CHAR(3)     NOT NULL,
    reference    TEXT        NOT NULL CHECK (length(btrim(reference)) > 0),
    received_at  TIMESTAMPTZ NOT NULL,
    recorded_by  UUID        NOT NULL REFERENCES organizations (id),
    created_at   TIMESTAMPTZ NOT NULL,

    CONSTRAINT repayments_within_face CHECK (amount_minor <= face_minor)
);

-- The division of one payment among the parties that held the receivable.
--
-- A share may be zero: a holding too small to earn a whole minor unit of a small payment
-- still exists, and recording it as nothing paid is more honest than dropping the row and
-- leaving a holder wondering whether they were considered at all.
CREATE TABLE repayment_shares (
    repayment_id   UUID    NOT NULL REFERENCES repayments (id) ON DELETE CASCADE,
    party_id       UUID    NOT NULL REFERENCES organizations (id),
    notional_minor BIGINT  NOT NULL CHECK (notional_minor > 0),
    amount_minor   BIGINT  NOT NULL CHECK (amount_minor >= 0),
    currency       CHAR(3) NOT NULL,

    PRIMARY KEY (repayment_id, party_id)
);

CREATE INDEX repayment_shares_party_idx ON repayment_shares (party_id);

-- +goose Down
DROP TABLE IF EXISTS repayment_shares;
DROP TABLE IF EXISTS repayments;
