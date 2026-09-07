package tokenization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// IssueRequest is what issuing one receivable as an asset needs.
type IssueRequest struct {
	InvoiceID uuid.UUID
	IssuerID  uuid.UUID
	// IssuerWallet receives the initial supply.
	IssuerWallet string
	// Supply is the notional to mint, in the invoice's currency.
	Supply money.Amount
	// Reference is a stable identifier for this issuance, so a retried request finds the
	// asset that already exists instead of minting a second one.
	Reference string
	// Metadata carries the asset facts the specification puts on chain: the invoice
	// commitment, maturity, grade and the hash of the terms.
	Metadata Metadata
}

// Metadata is the public description of an asset. It deliberately holds no document
// content: a commitment identifies the receivable without revealing it.
type Metadata struct {
	InvoiceCommitment string
	Currency          string
	FaceValue         string
	MaturityDate      string
	Grade             string
	TermsHash         string
}

// IssueResult is what an issuer reports back.
type IssueResult struct {
	Network       string
	TokenID       string
	ContractID    string
	TransactionID string
	ExplorerURL   string
	// Confirmed reports whether the network already finalized the issuance. A live chain
	// adapter may return false and let the reconciler confirm it later.
	Confirmed bool
}

// Validate checks a result before it is trusted, because an issuer is an external system.
func (r IssueResult) Validate() error {
	if strings.TrimSpace(r.Network) == "" {
		return apperr.Invalid("network", "the issuer returned no network")
	}
	if strings.TrimSpace(r.TokenID) == "" {
		return apperr.Invalid("token_id", "the issuer returned no token identifier")
	}
	return nil
}

// Issuer creates the on-chain asset for an approved receivable.
//
// The live implementation drives Hedera Asset Tokenization Studio. Everything behind this
// interface is replaceable; what the rest of the system depends on is that an approved
// invoice ends up with an identifier, a transaction and a link a judge can follow.
type Issuer interface {
	Issue(ctx context.Context, request IssueRequest) (IssueResult, error)
}

// LocalNetwork is the network name the in-process issuer reports. It is deliberately not a
// real network name: nothing should be able to mistake a local demo asset for a testnet one.
const LocalNetwork = "local"

// LocalIssuer mints deterministic identifiers without touching a network.
//
// It backs local development and the offline demo, and it is honest about what it is: no
// transaction exists, so it reports none, and its explorer link is empty rather than a URL
// that leads nowhere. Selected only when no Hedera credentials are configured.
type LocalIssuer struct{}

// NewLocalIssuer returns the in-process issuer.
func NewLocalIssuer() *LocalIssuer { return &LocalIssuer{} }

// Issue mints an identifier derived from the request.
//
// The identifier is a function of the invoice and the reference, so re-issuing the same
// receivable produces the same token: a retry after a failure is then indistinguishable
// from the first attempt, which is what makes the handler safe to redeliver.
func (i *LocalIssuer) Issue(_ context.Context, request IssueRequest) (IssueResult, error) {
	if request.InvoiceID == uuid.Nil {
		return IssueResult{}, apperr.Invalid("invoice_id", "must be a non-nil UUID")
	}
	if !request.Supply.IsPositive() {
		return IssueResult{}, apperr.Invalid("supply", "must be greater than zero")
	}

	seed := sha256.Sum256([]byte(request.InvoiceID.String() + "|" + request.Reference))
	digest := hex.EncodeToString(seed[:8])

	return IssueResult{
		Network:    LocalNetwork,
		TokenID:    "local-token-" + digest,
		ContractID: "local-compliance-" + hex.EncodeToString(seed[8:12]),
		// No transaction and no explorer link: there is no chain to point at, and a
		// plausible-looking fake would be worse than an empty field.
		Confirmed: true,
	}, nil
}
