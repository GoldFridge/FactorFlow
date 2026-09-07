package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// Durations that bound a login.
const (
	// ChallengeTTL is how long a wallet has to sign. Short, because the only thing waiting
	// buys an attacker is time to obtain a signature by other means.
	ChallengeTTL = 5 * time.Minute
	// SessionTTL is how long a session lasts before the wallet signs again.
	SessionTTL = 12 * time.Hour
)

// walletAddress matches the EVM-style addresses the Hedera-compatible wallets present.
var walletAddress = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Challenge is a one-time message a wallet must sign to prove it holds the key.
type Challenge struct {
	Nonce     string
	Wallet    string
	IssuedAt  time.Time
	ExpiresAt time.Time
	// ConsumedAt marks a challenge that was already exchanged for a session. A challenge is
	// good once: without that, a captured signature would be a permanent password.
	ConsumedAt *time.Time
}

// NewChallenge mints a challenge for a wallet.
func NewChallenge(wallet string, now time.Time) (*Challenge, error) {
	normalized, err := NormalizeWallet(wallet)
	if err != nil {
		return nil, err
	}

	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}

	return &Challenge{
		Nonce:     nonce,
		Wallet:    normalized,
		IssuedAt:  now.UTC(),
		ExpiresAt: now.UTC().Add(ChallengeTTL),
	}, nil
}

// IsExpired reports whether the signing window has closed.
func (c *Challenge) IsExpired(now time.Time) bool { return now.After(c.ExpiresAt) }

// IsConsumed reports whether the challenge was already used.
func (c *Challenge) IsConsumed() bool { return c.ConsumedAt != nil }

// Message is the text the wallet signs.
//
// It names the site, the wallet, the nonce and the expiry in plain language, so a person
// approving it in their wallet can read what they are agreeing to. It says nothing about
// value, because signing in must never look like authorizing a transfer.
func (c *Challenge) Message() string {
	return strings.Join([]string{
		"FactorFlow wants you to sign in with your wallet.",
		"",
		"This signature proves you control the address below.",
		"It authorizes no payment and moves no funds.",
		"",
		"Wallet: " + describeAddress(c.Wallet),
		"Nonce: " + c.Nonce,
		"Issued at: " + c.IssuedAt.Format(time.RFC3339),
		"Expires at: " + c.ExpiresAt.Format(time.RFC3339),
	}, "\n")
}

// Consume marks the challenge as used.
func (c *Challenge) Consume(now time.Time) error {
	if c.IsConsumed() {
		return apperr.Conflictf("challenge %s was already used", c.Nonce)
	}
	if c.IsExpired(now) {
		return apperr.Conflictf("challenge %s expired at %s", c.Nonce, c.ExpiresAt.Format(time.RFC3339))
	}
	consumed := now.UTC()
	c.ConsumedAt = &consumed
	return nil
}

// Session is an authenticated wallet's access to the API.
//
// The token itself is never stored. Only its hash is, so a database dump cannot be replayed
// as a login: the same reason a password would be hashed, applied to a bearer credential.
type Session struct {
	TokenHash      string
	OrganizationID uuid.UUID
	Wallet         string
	IssuedAt       time.Time
	ExpiresAt      time.Time
	RevokedAt      *time.Time
}

// NewSession mints a session and returns it with the token the caller must present.
func NewSession(organizationID uuid.UUID, wallet string, now time.Time) (*Session, string, error) {
	if organizationID == uuid.Nil {
		return nil, "", apperr.Invalid("organization_id", "must be a non-nil UUID")
	}
	normalized, err := NormalizeWallet(wallet)
	if err != nil {
		return nil, "", err
	}

	token, err := randomHex(32)
	if err != nil {
		return nil, "", err
	}

	return &Session{
		TokenHash:      HashToken(token),
		OrganizationID: organizationID,
		Wallet:         normalized,
		IssuedAt:       now.UTC(),
		ExpiresAt:      now.UTC().Add(SessionTTL),
	}, token, nil
}

// IsActive reports whether the session may still authenticate a request.
func (s *Session) IsActive(now time.Time) bool {
	return s.RevokedAt == nil && !now.After(s.ExpiresAt)
}

// Revoke ends the session.
func (s *Session) Revoke(now time.Time) {
	if s.RevokedAt == nil {
		revoked := now.UTC()
		s.RevokedAt = &revoked
	}
}

// HashToken maps a session token to the value stored for it.
func HashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func randomHex(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
