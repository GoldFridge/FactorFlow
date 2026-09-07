package identity_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

var testNow = time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)

type fakeQuerier struct{}

func (fakeQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row        { panic("not used") }

// memRepo is an in-memory identity store with the same one-shot semantics as the SQL one.
type memRepo struct {
	mu         sync.Mutex
	challenges map[string]identity.Challenge
	sessions   map[string]identity.Session
}

func newMemRepo() *memRepo {
	return &memRepo{
		challenges: map[string]identity.Challenge{},
		sessions:   map[string]identity.Session{},
	}
}

func (m *memRepo) InTx(_ context.Context, fn func(postgres.Querier) error) error {
	return fn(fakeQuerier{})
}

func (m *memRepo) Querier() postgres.Querier { return fakeQuerier{} }

func (m *memRepo) SaveChallenge(_ context.Context, _ postgres.Querier, c *identity.Challenge) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.challenges[c.Nonce] = *c
	return nil
}

func (m *memRepo) GetChallenge(_ context.Context, _ postgres.Querier, nonce string) (*identity.Challenge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.challenges[nonce]
	if !ok {
		return nil, apperr.NotFoundf("login challenge")
	}
	copied := stored
	return &copied, nil
}

func (m *memRepo) ConsumeChallenge(_ context.Context, _ postgres.Querier, c *identity.Challenge) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.challenges[c.Nonce]
	if !ok {
		return apperr.NotFoundf("login challenge")
	}
	if stored.ConsumedAt != nil {
		return apperr.Conflictf("challenge was already used")
	}
	m.challenges[c.Nonce] = *c
	return nil
}

func (m *memRepo) DeleteExpiredChallenges(_ context.Context, _ postgres.Querier, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var removed int64
	for nonce, stored := range m.challenges {
		if stored.ExpiresAt.Before(before) {
			delete(m.challenges, nonce)
			removed++
		}
	}
	return removed, nil
}

func (m *memRepo) SaveSession(_ context.Context, _ postgres.Querier, s *identity.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions[s.TokenHash] = *s
	return nil
}

func (m *memRepo) GetSession(_ context.Context, _ postgres.Querier, tokenHash string) (*identity.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.sessions[tokenHash]
	if !ok {
		return nil, apperr.NotFoundf("session")
	}
	copied := stored
	return &copied, nil
}

func (m *memRepo) RevokeSession(_ context.Context, _ postgres.Querier, s *identity.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions[s.TokenHash] = *s
	return nil
}

// staticAccounts resolves one wallet to one organization.
type staticAccounts struct {
	mu       sync.Mutex
	accounts map[string]identity.Account
}

func (s *staticAccounts) ByWallet(_ context.Context, _ postgres.Querier, wallet string) (identity.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	account, ok := s.accounts[wallet]
	if !ok {
		return identity.Account{}, apperr.NotFoundf("organization for wallet %s", wallet)
	}
	return account, nil
}

type fixture struct {
	service  *identity.Service
	repo     *memRepo
	accounts *staticAccounts
	wallet   *wallet
	orgID    uuid.UUID
	clock    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	w := newWallet(t)
	orgID := uuid.New()

	normalized, err := identity.NormalizeWallet(w.address)
	require.NoError(t, err)

	f := &fixture{
		repo:     newMemRepo(),
		accounts: &staticAccounts{accounts: map[string]identity.Account{normalized: {OrganizationID: orgID, Wallet: normalized, Eligible: true}}},
		wallet:   w,
		orgID:    orgID,
		clock:    testNow,
	}
	f.service = identity.NewService(f.repo, f.repo, f.accounts, func() time.Time { return f.clock })
	return f
}

// login walks the whole flow and returns the session token.
func (f *fixture) login(t *testing.T) string {
	t.Helper()

	challenge, err := f.service.Challenge(context.Background(), f.wallet.address)
	require.NoError(t, err)

	session, err := f.service.Verify(context.Background(), challenge.Nonce, f.wallet.sign(t, challenge.Message))
	require.NoError(t, err)
	return session.Token
}

func TestChallengeAndVerify(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()

	challenge, err := f.service.Challenge(ctx, f.wallet.address)
	require.NoError(t, err)

	assert.NotEmpty(t, challenge.Nonce)
	assert.Contains(t, challenge.Message, challenge.Nonce, "the nonce is what the wallet signs")
	assert.Contains(t, challenge.Message, "authorizes no payment",
		"a person approving this in their wallet is told it moves no funds")
	assert.Equal(t, testNow.Add(identity.ChallengeTTL), challenge.ExpiresAt)

	session, err := f.service.Verify(ctx, challenge.Nonce, f.wallet.sign(t, challenge.Message))
	require.NoError(t, err)

	assert.NotEmpty(t, session.Token)
	assert.Equal(t, f.orgID, session.OrganizationID)
	assert.Equal(t, testNow.Add(identity.SessionTTL), session.ExpiresAt)

	actor, err := f.service.Resolve(ctx, session.Token)
	require.NoError(t, err)
	assert.Equal(t, f.orgID, actor.OrganizationID)
	assert.True(t, actor.Eligible)
}

// TestAChallengeIsGoodOnce is what stops a captured signature from being a password.
func TestAChallengeIsGoodOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()

	challenge, err := f.service.Challenge(ctx, f.wallet.address)
	require.NoError(t, err)
	signature := f.wallet.sign(t, challenge.Message)

	_, err = f.service.Verify(ctx, challenge.Nonce, signature)
	require.NoError(t, err)

	_, err = f.service.Verify(ctx, challenge.Nonce, signature)
	require.ErrorIs(t, err, httpserver.ErrUnauthorized, "the same signature cannot be replayed")
}

func TestExpiredChallengeIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()

	challenge, err := f.service.Challenge(ctx, f.wallet.address)
	require.NoError(t, err)
	signature := f.wallet.sign(t, challenge.Message)

	f.clock = testNow.Add(identity.ChallengeTTL + time.Second)
	_, err = f.service.Verify(ctx, challenge.Nonce, signature)
	require.ErrorIs(t, err, httpserver.ErrUnauthorized)
}

// TestUnknownNonceLooksLikeAnExpiredOne keeps the endpoint from confirming which nonces
// exist.
func TestUnknownNonceLooksLikeAnExpiredOne(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	unknown := f.service
	_, err := unknown.Verify(context.Background(), "0123456789abcdef", "0xdeadbeef")
	require.ErrorIs(t, err, httpserver.ErrUnauthorized)
	assert.NotContains(t, err.Error(), "not found")
}

func TestSignatureFromAnotherWalletIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	other := newWallet(t)
	ctx := context.Background()

	challenge, err := f.service.Challenge(ctx, f.wallet.address)
	require.NoError(t, err)

	_, err = f.service.Verify(ctx, challenge.Nonce, other.sign(t, challenge.Message))
	require.ErrorIs(t, err, apperr.ErrValidation)
}

// TestUnregisteredWalletCannotSignIn checks the order of the checks: the signature is
// verified first, so an anonymous caller cannot use this endpoint to discover which
// addresses are registered.
func TestUnregisteredWalletCannotSignIn(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	stranger := newWallet(t)
	ctx := context.Background()

	challenge, err := f.service.Challenge(ctx, stranger.address)
	require.NoError(t, err, "a challenge is minted for any address")

	_, err = f.service.Verify(ctx, challenge.Nonce, stranger.sign(t, challenge.Message))
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

func TestChallengeRejectsAMalformedWallet(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.service.Challenge(context.Background(), "not-an-address")
	require.ErrorIs(t, err, apperr.ErrValidation)
}

// TestEligibilityIsReadFresh matters because a session outlives a decision: an organization
// whose eligibility is withdrawn must lose it immediately, not when its session expires.
func TestEligibilityIsReadFresh(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	token := f.login(t)

	actor, err := f.service.Resolve(context.Background(), token)
	require.NoError(t, err)
	require.True(t, actor.Eligible)

	normalized, err := identity.NormalizeWallet(f.wallet.address)
	require.NoError(t, err)
	f.accounts.accounts[normalized] = identity.Account{OrganizationID: f.orgID, Wallet: normalized, Eligible: false}

	actor, err = f.service.Resolve(context.Background(), token)
	require.NoError(t, err)
	assert.False(t, actor.Eligible, "the withdrawn decision took effect on the next request")
}

// TestWalletMovedToAnotherOrganizationEndsTheSession stops a session from outliving the
// relationship it was issued for.
func TestWalletMovedToAnotherOrganizationEndsTheSession(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	token := f.login(t)

	normalized, err := identity.NormalizeWallet(f.wallet.address)
	require.NoError(t, err)
	f.accounts.accounts[normalized] = identity.Account{OrganizationID: uuid.New(), Wallet: normalized, Eligible: true}

	_, err = f.service.Resolve(context.Background(), token)
	require.ErrorIs(t, err, httpserver.ErrUnauthorized)
}

func TestExpiredAndRevokedSessions(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()
	token := f.login(t)

	f.clock = testNow.Add(identity.SessionTTL + time.Second)
	_, err := f.service.Resolve(ctx, token)
	require.ErrorIs(t, err, httpserver.ErrUnauthorized, "an expired session stops working")

	f.clock = testNow
	fresh := f.login(t)
	require.NoError(t, f.service.Logout(ctx, fresh))

	_, err = f.service.Resolve(ctx, fresh)
	require.ErrorIs(t, err, httpserver.ErrUnauthorized, "a revoked session stops working")

	require.NoError(t, f.service.Logout(ctx, "unknown-token"), "logging out twice is not an error")
	require.NoError(t, f.service.Logout(ctx, ""))
}

func TestForgedTokenIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.login(t)

	_, err := f.service.Resolve(context.Background(), "0000000000000000000000000000000000000000000000000000000000000000")
	require.ErrorIs(t, err, httpserver.ErrUnauthorized)

	_, err = f.service.Resolve(context.Background(), "")
	require.ErrorIs(t, err, httpserver.ErrUnauthorized)
}

// TestTokenIsNotStored is the property that makes a database dump useless as a login.
func TestTokenIsNotStored(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	token := f.login(t)

	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()

	require.Len(t, f.repo.sessions, 1)
	for storedHash := range f.repo.sessions {
		assert.NotEqual(t, token, storedHash)
		assert.Equal(t, identity.HashToken(token), storedHash)
	}
}

func TestPurgeExpiredChallenges(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()

	_, err := f.service.Challenge(ctx, f.wallet.address)
	require.NoError(t, err)

	removed, err := f.service.PurgeExpiredChallenges(ctx)
	require.NoError(t, err)
	assert.Zero(t, removed, "a live challenge is left alone")

	f.clock = testNow.Add(identity.ChallengeTTL + time.Minute)
	removed, err = f.service.PurgeExpiredChallenges(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)
}

// TestResolverReadsBearerAndCookie covers both ways a caller can present a session: a
// machine client sends a header, a browser sends the cookie.
func TestResolverReadsBearerAndCookie(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	token := f.login(t)
	resolver := identity.NewResolver(f.service, identity.SessionCookie)

	withHeader := httptest.NewRequest(http.MethodGet, "/api/v1/invoices", http.NoBody)
	withHeader.Header.Set("Authorization", "Bearer "+token)
	actor, err := resolver.Resolve(withHeader)
	require.NoError(t, err)
	assert.Equal(t, f.orgID, actor.OrganizationID)

	withCookie := httptest.NewRequest(http.MethodGet, "/api/v1/invoices", http.NoBody)
	withCookie.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: token})
	actor, err = resolver.Resolve(withCookie)
	require.NoError(t, err)
	assert.Equal(t, f.orgID, actor.OrganizationID)

	anonymous := httptest.NewRequest(http.MethodGet, "/api/v1/invoices", http.NoBody)
	actor, err = resolver.Resolve(anonymous)
	require.NoError(t, err, "an anonymous request is not an error, it is simply anonymous")
	assert.True(t, actor.IsZero())

	malformed := httptest.NewRequest(http.MethodGet, "/api/v1/invoices", http.NoBody)
	malformed.Header.Set("Authorization", "Basic abc")
	actor, err = resolver.Resolve(malformed)
	require.NoError(t, err)
	assert.True(t, actor.IsZero())
}
