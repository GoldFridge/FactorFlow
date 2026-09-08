package tokenization

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/hedera"
)

// Chain is the network client the live issuer needs. It is an interface rather than the
// concrete client so this package can be tested without a network, and so the platform
// stays underneath the domain rather than beside it.
type Chain interface {
	CreateToken(ctx context.Context, token hedera.Token) (hedera.Receipt, error)
	Network() string
	Operator() string
}

/*
HederaIssuer mints a real fungible token for a receivable.

One receivable is one token, and its supply is the face value in minor units — a 15,000.00
USD invoice becomes 1,500,000 units of a two-decimal token. That mapping is the point:
a holder's balance is the notional they own, in cents, with no conversion to get wrong.

The treasury is the platform's own account, not the issuer's wallet, and that is a real
limitation rather than a shortcut. A treasury must sign the transactions that create and
move its tokens, and the platform does not hold an issuer's key — nor should it. So the
supply is minted into custody and moves to a buyer at settlement, and the issuer's claim
before then is the record in this system rather than a balance on the network.
*/
type HederaIssuer struct {
	chain Chain
}

// NewHederaIssuer returns the live issuer.
func NewHederaIssuer(chain Chain) *HederaIssuer { return &HederaIssuer{chain: chain} }

// Issue creates the token for one approved receivable.
func (i *HederaIssuer) Issue(ctx context.Context, request IssueRequest) (IssueResult, error) {
	if request.InvoiceID == uuid.Nil {
		return IssueResult{}, apperr.Invalid("invoice_id", "must be a non-nil UUID")
	}
	if !request.Supply.IsPositive() {
		return IssueResult{}, apperr.Invalid("supply", "must be greater than zero")
	}

	currency := request.Supply.Currency()
	exponent := currency.Exponent()
	if exponent < 0 {
		return IssueResult{}, apperr.Invalid("supply", "currency %s has no minor units", currency)
	}

	receipt, err := i.chain.CreateToken(ctx, hedera.Token{
		Name:   tokenName(request),
		Symbol: tokenSymbol(request),
		// The memo is public and permanent, so it carries the commitment to the invoice and
		// nothing else. A commitment identifies the receivable to anyone holding it and
		// discloses nothing to anyone who is not.
		Memo:     request.Metadata.InvoiceCommitment,
		Decimals: uint(exponent),
		Supply:   uint64(request.Supply.Minor()),
	})
	if err != nil {
		return IssueResult{}, err
	}

	return IssueResult{
		Network:       i.chain.Network(),
		TokenID:       receipt.TokenID,
		TransactionID: receipt.TransactionID,
		ExplorerURL:   receipt.ExplorerURL,
		// The receipt is the network's own confirmation that the token exists, so there is
		// nothing left for a reconciler to wait for.
		Confirmed: receipt.Status == "SUCCESS",
	}, nil
}

// tokenName is what a person sees in a wallet or an explorer.
func tokenName(request IssueRequest) string {
	grade := strings.TrimSpace(request.Metadata.Grade)
	if grade == "" {
		return fmt.Sprintf("FactorFlow receivable %s", short(request.InvoiceID))
	}
	return fmt.Sprintf("FactorFlow receivable %s grade %s", short(request.InvoiceID), grade)
}

// tokenSymbol is short and unique enough to tell two receivables apart at a glance.
func tokenSymbol(request IssueRequest) string {
	return "FF" + strings.ToUpper(short(request.InvoiceID))
}

func short(id uuid.UUID) string { return id.String()[:8] }
