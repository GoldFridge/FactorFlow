-- +goose Up
--
-- The statements below are plain DDL with no function bodies, so goose splits them on
-- semicolons and sends them one at a time. Wrapping them in StatementBegin/StatementEnd
-- would ask the driver to run the whole file as a single prepared statement, which
-- PostgreSQL refuses.

-- Money is stored as integer minor units plus its currency, never as a floating point
-- number. Rates keep twelve decimal places, well beyond the six the risk model publishes,
-- so a stored assessment can be recomputed exactly.

CREATE TABLE organizations (
    id               UUID PRIMARY KEY,
    type             TEXT        NOT NULL CHECK (type IN ('ISSUER', 'INVESTOR', 'OPERATOR')),
    name             TEXT        NOT NULL CHECK (length(btrim(name)) > 0),
    wallet           TEXT        NOT NULL,
    eligibility      TEXT        NOT NULL DEFAULT 'PENDING'
                                 CHECK (eligibility IN ('PENDING', 'ELIGIBLE', 'REJECTED')),
    -- Why a demo eligibility check was refused. Operator-facing text, never evidence.
    reason           TEXT        NOT NULL DEFAULT '',
    version          BIGINT      NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);

-- One wallet acts for one organization in the MVP, so a demo participant cannot bid
-- against themselves from two profiles.
CREATE UNIQUE INDEX organizations_wallet_key ON organizations (lower(wallet));

CREATE TABLE memberships (
    organization_id UUID        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    wallet          TEXT        NOT NULL,
    role            TEXT        NOT NULL CHECK (role IN ('OWNER', 'MEMBER', 'OPERATOR')),
    created_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id, wallet)
);

-- A wallet proves who it is by signing a one-time challenge. The nonce is the primary key
-- because its uniqueness is what makes a captured signature useless twice.
CREATE TABLE auth_challenges (
    nonce       TEXT PRIMARY KEY,
    wallet      TEXT        NOT NULL,
    issued_at   TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ
);

CREATE INDEX auth_challenges_expiry_idx ON auth_challenges (expires_at);

-- Only the hash of a session token is stored, for the same reason a password would be
-- hashed: a database dump must not be replayable as a login.
CREATE TABLE sessions (
    token_hash      TEXT PRIMARY KEY,
    organization_id UUID        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    wallet          TEXT        NOT NULL,
    issued_at       TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ
);

CREATE INDEX sessions_organization_idx ON sessions (organization_id, expires_at DESC);

CREATE TABLE invoices (
    id            UUID PRIMARY KEY,
    issuer_id     UUID        NOT NULL REFERENCES organizations (id),
    debtor_ref    TEXT        NOT NULL CHECK (length(btrim(debtor_ref)) > 0),
    number        TEXT        NOT NULL CHECK (length(btrim(number)) > 0),
    face_minor    BIGINT      NOT NULL CHECK (face_minor > 0),
    currency      CHAR(3)     NOT NULL,
    issued_at     TIMESTAMPTZ NOT NULL,
    due_at        TIMESTAMPTZ NOT NULL,
    status        TEXT        NOT NULL,
    failed_from   TEXT        NOT NULL DEFAULT '',
    reason        TEXT        NOT NULL DEFAULT '',
    assessment_id UUID,
    asset_id      UUID,
    version       BIGINT      NOT NULL CHECK (version > 0),
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    CONSTRAINT invoices_due_after_issue CHECK (due_at > issued_at)
);

CREATE INDEX invoices_issuer_idx ON invoices (issuer_id, created_at DESC);
CREATE INDEX invoices_status_idx ON invoices (status);

-- An issuer cannot submit the same invoice number for the same debtor twice while one is
-- still alive. This is the demo's double-financing guard; production needs a debtor or
-- registry integration, not a uniqueness constraint.
CREATE UNIQUE INDEX invoices_active_number_key
    ON invoices (issuer_id, debtor_ref, number)
    WHERE status NOT IN ('REJECTED', 'MATURED', 'DEFAULTED');

CREATE TABLE invoice_documents (
    invoice_id  UUID PRIMARY KEY REFERENCES invoices (id) ON DELETE CASCADE,
    object_key  TEXT        NOT NULL CHECK (length(btrim(object_key)) > 0),
    cipher_hash TEXT        NOT NULL CHECK (cipher_hash ~ '^[0-9a-f]{64}$'),
    key_ref     TEXT        NOT NULL CHECK (length(btrim(key_ref)) > 0),
    mime        TEXT        NOT NULL,
    size_bytes  BIGINT      NOT NULL CHECK (size_bytes > 0),
    uploaded_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE market_snapshots (
    payload_hash      TEXT PRIMARY KEY CHECK (payload_hash ~ '^0x[0-9a-f]{64}$'),
    provider          TEXT           NOT NULL,
    network           TEXT           NOT NULL,
    asset             TEXT           NOT NULL,
    query_hash        TEXT           NOT NULL CHECK (query_hash ~ '^0x[0-9a-f]{64}$'),
    benchmark_apr     NUMERIC(24, 12) NOT NULL,
    liquidity_premium NUMERIC(24, 12) NOT NULL,
    volatility        NUMERIC(24, 12) NOT NULL,
    total_liquidity_minor BIGINT      NOT NULL CHECK (total_liquidity_minor >= 0),
    currency          CHAR(3)        NOT NULL,
    subgraph_ids      TEXT[]         NOT NULL,
    block_numbers     BIGINT[]       NOT NULL,
    markets           JSONB          NOT NULL,
    observed_at       TIMESTAMPTZ    NOT NULL,
    ttl_seconds       INTEGER        NOT NULL CHECK (ttl_seconds > 0)
);

CREATE INDEX market_snapshots_observed_idx ON market_snapshots (observed_at DESC);

-- Assessments are immutable: a re-run inserts a new row rather than updating one, so the
-- inputs behind a published price never change after the fact.
CREATE TABLE risk_assessments (
    id                      UUID PRIMARY KEY,
    invoice_id              UUID            NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    model_version           TEXT            NOT NULL,
    features                JSONB           NOT NULL,
    contributions           JSONB           NOT NULL,
    confidence              NUMERIC(24, 12) NOT NULL,
    pd                      NUMERIC(24, 12) NOT NULL,
    lgd                     NUMERIC(24, 12) NOT NULL,
    expected_loss_minor     BIGINT          NOT NULL,
    grade                   TEXT            NOT NULL CHECK (grade IN ('A', 'B', 'C', 'D', 'E')),
    benchmark_apr           NUMERIC(24, 12) NOT NULL,
    risk_premium            NUMERIC(24, 12) NOT NULL,
    liquidity_premium       NUMERIC(24, 12) NOT NULL,
    concentration_premium   NUMERIC(24, 12) NOT NULL,
    discount_apr            NUMERIC(24, 12) NOT NULL,
    platform_fee_minor      BIGINT          NOT NULL,
    reserve_price_minor     BIGINT          NOT NULL,
    currency                CHAR(3)         NOT NULL,
    market_snapshot_hash    TEXT            NOT NULL REFERENCES market_snapshots (payload_hash),
    confidential_commitment TEXT            NOT NULL CHECK (confidential_commitment ~ '^0x[0-9a-f]{64}$'),
    -- The nonce the commitment was computed over. A commitment nobody can recompute proves
    -- nothing, so the input that binds it to one workflow run is stored beside it.
    confidential_nonce      TEXT            NOT NULL DEFAULT '',
    requires_manual_review  BOOLEAN         NOT NULL,
    created_at              TIMESTAMPTZ     NOT NULL
);

CREATE INDEX risk_assessments_invoice_idx ON risk_assessments (invoice_id, created_at DESC);

-- One approved invoice becomes one tokenized asset. The chain identifiers are text because
-- they are whatever the network calls them: a Hedera token id, an EVM contract address, or
-- the local identifier the in-process issuer mints when no network is configured.
CREATE TABLE tokenized_assets (
    id            UUID PRIMARY KEY,
    invoice_id    UUID        NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    issuer_id     UUID        NOT NULL REFERENCES organizations (id),
    network       TEXT        NOT NULL,
    token_id      TEXT        NOT NULL,
    contract_id   TEXT        NOT NULL DEFAULT '',
    supply_minor  BIGINT      NOT NULL CHECK (supply_minor > 0),
    currency      CHAR(3)     NOT NULL,
    chain_status  TEXT        NOT NULL CHECK (chain_status IN ('PENDING', 'ISSUED', 'FROZEN', 'REDEEMED')),
    transaction_id TEXT       NOT NULL DEFAULT '',
    explorer_url  TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

-- An approved invoice is issued once. A retry after a failed issuance must find the asset
-- that already exists rather than mint a second one.
CREATE UNIQUE INDEX tokenized_assets_invoice_key ON tokenized_assets (invoice_id);
CREATE INDEX tokenized_assets_issuer_idx ON tokenized_assets (issuer_id, created_at DESC);

CREATE TABLE auctions (
    id               UUID PRIMARY KEY,
    issuer_id        UUID        NOT NULL REFERENCES organizations (id),
    status           TEXT        NOT NULL,
    solver_version   TEXT        NOT NULL DEFAULT '',
    certificate_hash TEXT        NOT NULL DEFAULT '',
    reason           TEXT        NOT NULL DEFAULT '',
    opens_at         TIMESTAMPTZ NOT NULL,
    closes_at        TIMESTAMPTZ NOT NULL,
    version          BIGINT      NOT NULL CHECK (version > 0),
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    CONSTRAINT auctions_closes_after_opens CHECK (closes_at > opens_at)
);

CREATE INDEX auctions_status_idx ON auctions (status, closes_at);

CREATE TABLE auction_lots (
    id                  UUID PRIMARY KEY,
    auction_id          UUID    NOT NULL REFERENCES auctions (id) ON DELETE CASCADE,
    invoice_id          UUID    NOT NULL REFERENCES invoices (id),
    asset_id            UUID    NOT NULL,
    issuer_id           UUID    NOT NULL REFERENCES organizations (id),
    debtor_ref          TEXT    NOT NULL,
    supply_minor        BIGINT  NOT NULL CHECK (supply_minor > 0),
    reserve_price_minor BIGINT  NOT NULL CHECK (reserve_price_minor > 0),
    currency            CHAR(3) NOT NULL,
    grade               TEXT    NOT NULL CHECK (grade IN ('A', 'B', 'C', 'D', 'E')),
    tenor_days          BIGINT  NOT NULL CHECK (tenor_days > 0),
    CONSTRAINT auction_lots_price_below_face CHECK (reserve_price_minor < supply_minor)
);

-- An asset belongs to at most one live auction, which is what makes an exposure limit
-- meaningful: the same receivable cannot be sold twice in parallel batches.
CREATE UNIQUE INDEX auction_lots_asset_key ON auction_lots (asset_id);
CREATE INDEX auction_lots_auction_idx ON auction_lots (auction_id);

CREATE TABLE bids (
    id                UUID PRIMARY KEY,
    auction_id        UUID            NOT NULL REFERENCES auctions (id) ON DELETE CASCADE,
    investor_id       UUID            NOT NULL REFERENCES organizations (id),
    budget_minor      BIGINT          NOT NULL CHECK (budget_minor > 0),
    currency          CHAR(3)         NOT NULL,
    min_yield         NUMERIC(24, 12) NOT NULL CHECK (min_yield >= 0),
    max_grade         TEXT            NOT NULL CHECK (max_grade IN ('A', 'B', 'C', 'D', 'E')),
    max_tenor_days    BIGINT          NOT NULL CHECK (max_tenor_days > 0),
    minimum_lot_minor BIGINT          NOT NULL CHECK (minimum_lot_minor >= 0),
    max_issuer_share  NUMERIC(24, 12) NOT NULL CHECK (max_issuer_share >= 0 AND max_issuer_share <= 1),
    max_debtor_share  NUMERIC(24, 12) NOT NULL CHECK (max_debtor_share >= 0 AND max_debtor_share <= 1),
    max_grade_share   JSONB           NOT NULL DEFAULT '{}'::jsonb,
    status            TEXT            NOT NULL CHECK (status IN ('ACTIVE', 'CANCELLED', 'ALLOCATED', 'REJECTED')),
    version           BIGINT          NOT NULL CHECK (version > 0),
    created_at        TIMESTAMPTZ     NOT NULL
);

CREATE INDEX bids_auction_idx ON bids (auction_id, created_at, id);
CREATE INDEX bids_investor_idx ON bids (investor_id, created_at DESC);

CREATE TABLE allocations (
    auction_id     UUID    NOT NULL REFERENCES auctions (id) ON DELETE CASCADE,
    lot_id         UUID    NOT NULL REFERENCES auction_lots (id) ON DELETE CASCADE,
    bid_id         UUID    NOT NULL REFERENCES bids (id) ON DELETE CASCADE,
    investor_id    UUID    NOT NULL REFERENCES organizations (id),
    notional_minor BIGINT  NOT NULL CHECK (notional_minor > 0),
    price_minor    BIGINT  NOT NULL CHECK (price_minor > 0),
    currency       CHAR(3) NOT NULL,
    rank           INTEGER NOT NULL CHECK (rank >= 0),
    PRIMARY KEY (auction_id, lot_id, bid_id)
);

CREATE INDEX allocations_bid_idx ON allocations (bid_id);

CREATE TABLE allocation_rejections (
    auction_id UUID NOT NULL REFERENCES auctions (id) ON DELETE CASCADE,
    bid_id     UUID NOT NULL REFERENCES bids (id) ON DELETE CASCADE,
    lot_id     UUID,
    constraint_name TEXT NOT NULL CHECK (length(btrim(constraint_name)) > 0),
    PRIMARY KEY (auction_id, bid_id)
);

CREATE TABLE allocation_certificates (
    auction_id           UUID PRIMARY KEY REFERENCES auctions (id) ON DELETE CASCADE,
    certificate_hash     TEXT        NOT NULL CHECK (certificate_hash ~ '^0x[0-9a-f]{64}$'),
    solver_version       TEXT        NOT NULL,
    objective            BIGINT      NOT NULL,
    total_notional_minor BIGINT      NOT NULL CHECK (total_notional_minor >= 0),
    total_cash_minor     BIGINT      NOT NULL CHECK (total_cash_minor >= 0),
    currency             CHAR(3)     NOT NULL,
    branch_nodes         INTEGER     NOT NULL,
    repair_rounds        INTEGER     NOT NULL,
    limit_reached        BOOLEAN     NOT NULL,
    verified             BOOLEAN     NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL
);

-- The append-only audit trail. There is no update or delete path in the application, and
-- the trigger below refuses one at the database level as well.
CREATE TABLE audit_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor       TEXT        NOT NULL,
    action      TEXT        NOT NULL,
    entity_type TEXT        NOT NULL,
    entity_id   TEXT        NOT NULL,
    before_hash TEXT        NOT NULL DEFAULT '',
    after_hash  TEXT        NOT NULL DEFAULT '',
    trace_id    TEXT        NOT NULL DEFAULT '',
    detail      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX audit_events_entity_idx ON audit_events (entity_type, entity_id, occurred_at DESC);
CREATE INDEX audit_events_trace_idx ON audit_events (trace_id);

-- The transactional outbox: a row is written inside the same transaction as the domain
-- change, and a worker delivers it afterwards. That is what makes an external side effect
-- exactly-once in business terms even though the chain call happens outside the
-- transaction.
CREATE TABLE outbox_events (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    topic        TEXT        NOT NULL CHECK (length(btrim(topic)) > 0),
    payload      JSONB       NOT NULL,
    attempts     INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error   TEXT        NOT NULL DEFAULT '',
    trace_id     TEXT        NOT NULL DEFAULT '',
    available_at TIMESTAMPTZ NOT NULL,
    delivered_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX outbox_events_pending_idx ON outbox_events (available_at, id) WHERE delivered_at IS NULL;

-- Idempotency keys make a repeated write return the first response instead of performing
-- the effect twice; the request fingerprint catches a key reused for a different body.
CREATE TABLE idempotency_keys (
    key             TEXT        NOT NULL,
    organization_id UUID        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    endpoint        TEXT        NOT NULL,
    request_hash    TEXT        NOT NULL,
    status_code     INTEGER,
    response_body   JSONB,
    created_at      TIMESTAMPTZ NOT NULL,
    completed_at    TIMESTAMPTZ,
    PRIMARY KEY (organization_id, endpoint, key)
);

-- +goose Down
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS allocation_certificates;
DROP TABLE IF EXISTS allocation_rejections;
DROP TABLE IF EXISTS allocations;
DROP TABLE IF EXISTS bids;
DROP TABLE IF EXISTS auction_lots;
DROP TABLE IF EXISTS auctions;
DROP TABLE IF EXISTS tokenized_assets;
DROP TABLE IF EXISTS risk_assessments;
DROP TABLE IF EXISTS market_snapshots;
DROP TABLE IF EXISTS invoice_documents;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS auth_challenges;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS organizations;
