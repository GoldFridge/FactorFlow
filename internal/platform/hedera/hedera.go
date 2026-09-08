// Package hedera is a thin client over the Hedera SDK.
//
// It knows nothing about receivables. What it offers is the two operations the platform
// actually performs on a network — create a fungible token, and move some of it — plus the
// links a person needs to check that either happened. Keeping it free of business types is
// what lets it sit in the platform, underneath the modules that use it.
package hedera

import (
	"context"
	"fmt"
	"strings"
	"time"

	hiero "github.com/hiero-ledger/hiero-sdk-go/v2/sdk"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// Networks this client understands.
const (
	Testnet    = "testnet"
	Previewnet = "previewnet"
	Mainnet    = "mainnet"
)

// requestTimeout bounds a single network call.
//
// The SDK's calls do not take a context, so a caller's deadline cannot cancel one already in
// flight; this at least stops a worker from waiting on a network that has stopped answering.
const requestTimeout = 30 * time.Second

// Config is what connecting needs.
type Config struct {
	// Network is testnet, previewnet or mainnet.
	Network string
	// AccountID is the operator, and the treasury every token is created against.
	AccountID string
	// PrivateKey is the operator's key, with or without a 0x prefix.
	PrivateKey string
}

// Client talks to one network as one account.
type Client struct {
	inner    *hiero.Client
	operator hiero.AccountID
	key      hiero.PrivateKey
	network  string
}

// Connect opens a client and verifies that the credentials parse.
//
// It does not reach the network: a constructor that blocks on a remote system turns a
// configuration mistake into a startup hang, and the first real call reports a broken
// network perfectly well.
func Connect(cfg Config) (*Client, error) {
	network := strings.ToLower(strings.TrimSpace(cfg.Network))
	if network == "" {
		network = Testnet
	}
	switch network {
	case Testnet, Previewnet, Mainnet:
	default:
		return nil, apperr.Invalid("network", "must be testnet, previewnet or mainnet, got %q", network)
	}

	account, err := hiero.AccountIDFromString(strings.TrimSpace(cfg.AccountID))
	if err != nil {
		return nil, apperr.Invalid("account_id", "must be a Hedera account such as 0.0.1234: %v", err)
	}

	key, err := parseKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}

	inner, err := hiero.ClientForName(network)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", network, err)
	}
	inner.SetOperator(account, key)
	inner.SetRequestTimeout(requestTimeout)

	return &Client{inner: inner, operator: account, key: key, network: network}, nil
}

// parseKey accepts the two shapes a Hedera key is handed out in: a raw ECDSA hex string,
// which is what an EVM-style export looks like, and DER, which is what the portal shows.
func parseKey(raw string) (hiero.PrivateKey, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return hiero.PrivateKey{}, apperr.Invalid("private_key", "must not be empty")
	}

	hexKey := strings.TrimPrefix(strings.TrimPrefix(trimmed, "0x"), "0X")
	if key, err := hiero.PrivateKeyFromStringECDSA(hexKey); err == nil {
		return key, nil
	}
	key, err := hiero.PrivateKeyFromString(trimmed)
	if err != nil {
		return hiero.PrivateKey{}, apperr.Invalid("private_key", "is neither an ECDSA hex key nor DER: %v", err)
	}
	return key, nil
}

// Close releases the client's connections.
func (c *Client) Close() error { return c.inner.Close() }

// Network is the network this client is pointed at.
func (c *Client) Network() string { return c.network }

// Operator is the account that pays for and signs everything this client does.
func (c *Client) Operator() string { return c.operator.String() }

// Token describes a fungible token to create.
type Token struct {
	Name   string
	Symbol string
	// Memo is public and permanent. It carries a commitment, never a document.
	Memo string
	// Decimals and Supply are in the token's own units: a receivable is minted in the minor
	// units of its currency, so cents become whole tokens with two decimals.
	Decimals uint
	Supply   uint64
}

// Receipt is what a network confirmed.
type Receipt struct {
	TokenID       string
	TransactionID string
	ExplorerURL   string
	Status        string
}

// CreateToken creates a fungible token whose treasury is the operator.
//
// The supply key is kept by the operator so the asset can be burned when a receivable is
// settled or written off; without one the supply would be fixed for the life of the token,
// and a receivable that no longer exists would still have tokens standing for it.
func (c *Client) CreateToken(ctx context.Context, token Token) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if token.Supply == 0 {
		return Receipt{}, apperr.Invalid("supply", "must be greater than zero")
	}

	response, err := hiero.NewTokenCreateTransaction().
		SetTokenName(clamp(token.Name, 100)).
		SetTokenSymbol(clamp(token.Symbol, 100)).
		SetTokenMemo(clamp(token.Memo, 100)).
		SetTokenType(hiero.TokenTypeFungibleCommon).
		SetDecimals(token.Decimals).
		SetInitialSupply(token.Supply).
		SetTreasuryAccountID(c.operator).
		SetAdminKey(c.key.PublicKey()).
		SetSupplyKey(c.key.PublicKey()).
		Execute(c.inner)
	if err != nil {
		return Receipt{}, fmt.Errorf("submitting the token creation: %w", err)
	}

	receipt, err := response.GetReceiptQueryWithClient(c.inner).Execute(c.inner)
	if err != nil {
		// The transaction may well have succeeded; the receipt is what we could not read.
		// Reporting the id lets an operator look it up rather than guess.
		return Receipt{TransactionID: response.TransactionID.String()},
			fmt.Errorf("reading the receipt for %s: %w", response.TransactionID, err)
	}
	if receipt.TokenID == nil {
		return Receipt{}, apperr.Unavailablef("the network accepted the transaction but returned no token id")
	}

	return Receipt{
		TokenID:       receipt.TokenID.String(),
		TransactionID: response.TransactionID.String(),
		ExplorerURL:   c.TokenURL(receipt.TokenID.String()),
		Status:        receipt.Status.String(),
	}, nil
}

// Transfer moves an amount of a token between two accounts.
//
// Hedera requires the receiving account to have associated the token, or to have an
// automatic association slot free. That is the recipient's decision, not ours, so a refusal
// here is reported as it comes back rather than worked around.
func (c *Client) Transfer(ctx context.Context, tokenID, to string, amount int64) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if amount <= 0 {
		return Receipt{}, apperr.Invalid("amount", "must be greater than zero")
	}

	token, err := hiero.TokenIDFromString(strings.TrimSpace(tokenID))
	if err != nil {
		return Receipt{}, apperr.Invalid("token_id", "must be a Hedera token such as 0.0.1234: %v", err)
	}
	recipient, err := accountFrom(to)
	if err != nil {
		return Receipt{}, err
	}

	response, err := hiero.NewTransferTransaction().
		AddTokenTransfer(token, c.operator, -amount).
		AddTokenTransfer(token, recipient, amount).
		Execute(c.inner)
	if err != nil {
		return Receipt{}, fmt.Errorf("submitting the transfer: %w", err)
	}

	receipt, err := response.GetReceiptQueryWithClient(c.inner).Execute(c.inner)
	if err != nil {
		return Receipt{TransactionID: response.TransactionID.String()},
			fmt.Errorf("reading the receipt for %s: %w", response.TransactionID, err)
	}

	return Receipt{
		TokenID:       token.String(),
		TransactionID: response.TransactionID.String(),
		ExplorerURL:   c.TransactionURL(response.TransactionID.String()),
		Status:        receipt.Status.String(),
	}, nil
}

// accountFrom accepts either a Hedera account id or the EVM address a wallet shows, because
// a participant who signed in with a browser wallet knows only the second one.
func accountFrom(value string) (hiero.AccountID, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return hiero.AccountID{}, apperr.Invalid("account", "must not be empty")
	}

	if strings.Contains(trimmed, ".") {
		account, err := hiero.AccountIDFromString(trimmed)
		if err != nil {
			return hiero.AccountID{}, apperr.Invalid("account", "must be a Hedera account: %v", err)
		}
		return account, nil
	}

	account, err := hiero.AccountIDFromEvmPublicAddress(strings.TrimPrefix(trimmed, "0x"))
	if err != nil {
		return hiero.AccountID{}, apperr.Invalid("account",
			"must be a Hedera account id or an EVM address: %v", err)
	}
	return account, nil
}

// TokenURL is where a person can see the token.
func (c *Client) TokenURL(tokenID string) string {
	return fmt.Sprintf("https://hashscan.io/%s/token/%s", c.network, tokenID)
}

// TransactionURL is where a person can see one transaction.
func (c *Client) TransactionURL(transactionID string) string {
	return fmt.Sprintf("https://hashscan.io/%s/transaction/%s", c.network, transactionID)
}

// clamp keeps a field inside the network's limit. Truncating is better than a rejected
// transaction: these are display fields, and the commitment they accompany is what binds.
func clamp(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
