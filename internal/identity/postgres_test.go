package identity_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/pgtest"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

func seedOrganization(t *testing.T, db *postgres.DB, seq int) (uuid.UUID, string) {
	t.Helper()

	wallet := fmt.Sprintf("0x%040x", seq)
	org, err := organization.New(organization.NewParams{
		ID:     uuid.New(),
		Type:   organization.TypeIssuer,
		Name:   fmt.Sprintf("Issuer %d", seq),
		Wallet: wallet,
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, organization.NewPostgresRepository().Create(context.Background(), db.Querier(), org))
	return org.ID, org.Wallet
}

func TestChallengeRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := identity.NewPostgresRepository()

	challenge, err := identity.NewChallenge("0x1111111111111111111111111111111111111111", testNow)
	require.NoError(t, err)
	require.NoError(t, repo.SaveChallenge(ctx, db.Querier(), challenge))

	got, err := repo.GetChallenge(ctx, db.Querier(), challenge.Nonce)
	require.NoError(t, err)

	assert.Equal(t, challenge.Wallet, got.Wallet)
	assert.True(t, got.ExpiresAt.Equal(challenge.ExpiresAt))
	assert.False(t, got.IsConsumed())
	assert.Equal(t, challenge.Message(), got.Message(), "the message a wallet signs survives storage")

	_, err = repo.GetChallenge(ctx, db.Querier(), "unknown-nonce")
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestChallengeIsConsumedOnce is the database half of replay protection: two requests
// racing with the same signature, and only one may win.
func TestChallengeIsConsumedOnce(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := identity.NewPostgresRepository()

	challenge, err := identity.NewChallenge("0x2222222222222222222222222222222222222222", testNow)
	require.NoError(t, err)
	require.NoError(t, repo.SaveChallenge(ctx, db.Querier(), challenge))

	first, err := repo.GetChallenge(ctx, db.Querier(), challenge.Nonce)
	require.NoError(t, err)
	second, err := repo.GetChallenge(ctx, db.Querier(), challenge.Nonce)
	require.NoError(t, err)

	require.NoError(t, first.Consume(testNow))
	require.NoError(t, repo.ConsumeChallenge(ctx, db.Querier(), first))

	require.NoError(t, second.Consume(testNow))
	err = repo.ConsumeChallenge(ctx, db.Querier(), second)
	require.ErrorIs(t, err, apperr.ErrConflict, "the second request is told it lost the race")

	stored, err := repo.GetChallenge(ctx, db.Querier(), challenge.Nonce)
	require.NoError(t, err)
	assert.True(t, stored.IsConsumed())
}

func TestSessionRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := identity.NewPostgresRepository()

	orgID, wallet := seedOrganization(t, db, 1)

	session, token, err := identity.NewSession(orgID, wallet, testNow)
	require.NoError(t, err)
	require.NoError(t, repo.SaveSession(ctx, db.Querier(), session))

	got, err := repo.GetSession(ctx, db.Querier(), identity.HashToken(token))
	require.NoError(t, err)

	assert.Equal(t, orgID, got.OrganizationID)
	assert.Equal(t, wallet, got.Wallet)
	assert.True(t, got.IsActive(testNow))
	assert.False(t, got.IsActive(testNow.Add(identity.SessionTTL+time.Second)))

	// The token itself is nowhere in the database: only its hash can be looked up.
	_, err = repo.GetSession(ctx, db.Querier(), token)
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

func TestRevokeSession(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := identity.NewPostgresRepository()

	orgID, wallet := seedOrganization(t, db, 2)
	session, token, err := identity.NewSession(orgID, wallet, testNow)
	require.NoError(t, err)
	require.NoError(t, repo.SaveSession(ctx, db.Querier(), session))

	session.Revoke(testNow.Add(time.Minute))
	require.NoError(t, repo.RevokeSession(ctx, db.Querier(), session))

	got, err := repo.GetSession(ctx, db.Querier(), identity.HashToken(token))
	require.NoError(t, err)
	assert.False(t, got.IsActive(testNow.Add(2*time.Minute)))
}

func TestSessionRequiresAKnownOrganization(t *testing.T) {
	db := pgtest.New(t)

	session, _, err := identity.NewSession(uuid.New(), "0x3333333333333333333333333333333333333333", testNow)
	require.NoError(t, err)

	err = identity.NewPostgresRepository().SaveSession(context.Background(), db.Querier(), session)
	require.ErrorIs(t, err, apperr.ErrConflict)
}

func TestDeleteExpiredChallenges(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := identity.NewPostgresRepository()

	fresh, err := identity.NewChallenge("0x4444444444444444444444444444444444444444", testNow)
	require.NoError(t, err)
	require.NoError(t, repo.SaveChallenge(ctx, db.Querier(), fresh))

	stale, err := identity.NewChallenge("0x5555555555555555555555555555555555555555", testNow.Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, repo.SaveChallenge(ctx, db.Querier(), stale))

	removed, err := repo.DeleteExpiredChallenges(ctx, db.Querier(), testNow)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	_, err = repo.GetChallenge(ctx, db.Querier(), fresh.Nonce)
	require.NoError(t, err, "a live challenge is left alone")

	_, err = repo.GetChallenge(ctx, db.Querier(), stale.Nonce)
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestEndToEndLoginAgainstPostgres runs the whole exchange against the real store, which is
// where the one-shot nonce and the hashed token actually have to hold.
func TestEndToEndLoginAgainstPostgres(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	w := newWallet(t)
	normalized, err := identity.NormalizeWallet(w.address)
	require.NoError(t, err)

	org, err := organization.New(organization.NewParams{
		ID: uuid.New(), Type: organization.TypeInvestor, Name: "Alpine Treasury", Wallet: normalized,
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, org.Approve(testNow))
	require.NoError(t, organization.NewPostgresRepository().Create(ctx, db.Querier(), org))

	service := identity.NewService(db, identity.NewPostgresRepository(),
		accountsOverRepository{repo: organization.NewPostgresRepository()},
		func() time.Time { return testNow })

	challenge, err := service.Challenge(ctx, w.address)
	require.NoError(t, err)

	session, err := service.Verify(ctx, challenge.Nonce, w.sign(t, challenge.Message))
	require.NoError(t, err)
	assert.Equal(t, org.ID, session.OrganizationID)

	actor, err := service.Resolve(ctx, session.Token)
	require.NoError(t, err)
	assert.Equal(t, org.ID, actor.OrganizationID)
	assert.True(t, actor.Eligible)

	_, err = service.Verify(ctx, challenge.Nonce, w.sign(t, challenge.Message))
	require.Error(t, err, "the nonce is spent")

	require.NoError(t, service.Logout(ctx, session.Token))
	_, err = service.Resolve(ctx, session.Token)
	require.Error(t, err)
}

// accountsOverRepository is the adapter the application layer uses to answer identity's one
// question about an organization.
type accountsOverRepository struct {
	repo organization.Repository
}

func (a accountsOverRepository) ByWallet(ctx context.Context, q postgres.Querier, wallet string) (identity.Account, error) {
	org, err := a.repo.GetByWallet(ctx, q, wallet)
	if err != nil {
		return identity.Account{}, err
	}
	return identity.Account{
		OrganizationID: org.ID,
		Wallet:         org.Wallet,
		Eligible:       org.IsEligible(),
		Operator:       org.Type == organization.TypeOperator,
	}, nil
}
