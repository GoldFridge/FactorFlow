package organization_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/organization"
	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/pgtest"
	"github.com/skimer2king/factorflow/internal/platform/postgres"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// newOrg builds an organization with a unique wallet, since a wallet acts for exactly one
// organization.
func newOrg(t *testing.T, orgType organization.Type, seq int) *organization.Organization {
	t.Helper()

	org, err := organization.New(organization.NewParams{
		ID:     uuid.New(),
		Type:   orgType,
		Name:   fmt.Sprintf("Demo %s %d", orgType, seq),
		Wallet: fmt.Sprintf("0x%040x", seq),
	}, testNow)
	require.NoError(t, err)
	return org
}

func TestRepositoryRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := organization.NewPostgresRepository()

	org := newOrg(t, organization.TypeIssuer, 1)
	require.NoError(t, repo.Create(ctx, db.Querier(), org))

	got, err := repo.Get(ctx, db.Querier(), org.ID)
	require.NoError(t, err)
	assert.Equal(t, org.ID, got.ID)
	assert.Equal(t, org.Type, got.Type)
	assert.Equal(t, org.Name, got.Name)
	assert.Equal(t, org.Wallet, got.Wallet)
	assert.Equal(t, organization.EligibilityPending, got.Eligibility)
	assert.Equal(t, int64(1), got.Version)
	assert.True(t, got.CreatedAt.Equal(org.CreatedAt), "timestamps survive the round trip")

	byWallet, err := repo.GetByWallet(ctx, db.Querier(), "0X"+org.Wallet[2:])
	require.NoError(t, err)
	assert.Equal(t, org.ID, byWallet.ID, "wallet lookup is case insensitive")
}

func TestGetMissingOrganization(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := organization.NewPostgresRepository()

	_, err := repo.Get(ctx, db.Querier(), uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = repo.GetByWallet(ctx, db.Querier(), "0x0000000000000000000000000000000000000099")
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestWalletIsUnique covers the MVP rule that one wallet acts for one organization, so a
// demo participant cannot bid against themselves from two profiles.
func TestWalletIsUnique(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := organization.NewPostgresRepository()

	first := newOrg(t, organization.TypeIssuer, 7)
	require.NoError(t, repo.Create(ctx, db.Querier(), first))

	second := newOrg(t, organization.TypeInvestor, 7)
	err := repo.Create(ctx, db.Querier(), second)
	require.ErrorIs(t, err, apperr.ErrConflict)
}

// TestOptimisticConcurrency is the version check the specification requires: the second
// writer is told to reload rather than silently overwriting the first.
func TestOptimisticConcurrency(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := organization.NewPostgresRepository()

	org := newOrg(t, organization.TypeInvestor, 3)
	require.NoError(t, repo.Create(ctx, db.Querier(), org))

	// Two operators read the same version and both decide eligibility.
	writerA, err := repo.Get(ctx, db.Querier(), org.ID)
	require.NoError(t, err)
	writerB, err := repo.Get(ctx, db.Querier(), org.ID)
	require.NoError(t, err)

	require.NoError(t, writerA.Approve(testNow.Add(time.Minute)))
	require.NoError(t, repo.Update(ctx, db.Querier(), writerA, 1))

	require.NoError(t, writerB.Reject("stale decision", testNow.Add(2*time.Minute)))
	err = repo.Update(ctx, db.Querier(), writerB, 1)
	require.ErrorIs(t, err, apperr.ErrConflict)

	stored, err := repo.Get(ctx, db.Querier(), org.ID)
	require.NoError(t, err)
	assert.Equal(t, organization.EligibilityEligible, stored.Eligibility, "the first writer's decision stands")
	assert.Equal(t, int64(2), stored.Version)
}

func TestList(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := organization.NewPostgresRepository()

	for seq := 10; seq < 13; seq++ {
		require.NoError(t, repo.Create(ctx, db.Querier(), newOrg(t, organization.TypeInvestor, seq)))
	}
	require.NoError(t, repo.Create(ctx, db.Querier(), newOrg(t, organization.TypeIssuer, 20)))

	investors, err := repo.List(ctx, db.Querier(), organization.TypeInvestor, 10)
	require.NoError(t, err)
	assert.Len(t, investors, 3)
	for _, org := range investors {
		assert.Equal(t, organization.TypeInvestor, org.Type)
	}

	all, err := repo.List(ctx, db.Querier(), "", 10)
	require.NoError(t, err)
	assert.Len(t, all, 4, "an empty type lists every organization")

	limited, err := repo.List(ctx, db.Querier(), "", 2)
	require.NoError(t, err)
	assert.Len(t, limited, 2)
}

// TestTransactionRollsBack proves the transaction helper actually isolates: a failed unit
// of work leaves nothing behind.
func TestTransactionRollsBack(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := organization.NewPostgresRepository()

	org := newOrg(t, organization.TypeIssuer, 30)
	wantErr := fmt.Errorf("deliberate failure")

	err := db.InTx(ctx, func(q postgres.Querier) error {
		if err := repo.Create(ctx, q, org); err != nil {
			return err
		}
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)

	_, err = repo.Get(ctx, db.Querier(), org.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound, "the rolled back insert left no row")
}

func TestTransactionCommits(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := organization.NewPostgresRepository()

	org := newOrg(t, organization.TypeIssuer, 31)
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		return repo.Create(ctx, q, org)
	}))

	stored, err := repo.Get(ctx, db.Querier(), org.ID)
	require.NoError(t, err)
	assert.Equal(t, org.ID, stored.ID)
}
