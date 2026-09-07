package organization_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

var testNow = time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

const testWallet = "0x1234567890AbcdEF1234567890aBcdef12345678"

func validParams() organization.NewParams {
	return organization.NewParams{
		ID:     uuid.New(),
		Type:   organization.TypeIssuer,
		Name:   "Northwind Trading OU",
		Wallet: testWallet,
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	org, err := organization.New(validParams(), testNow)
	require.NoError(t, err)

	assert.Equal(t, organization.EligibilityPending, org.Eligibility, "a new organization has not been checked yet")
	assert.Equal(t, int64(1), org.Version)
	assert.Equal(t, "0x1234567890abcdef1234567890abcdef12345678", org.Wallet, "wallets are stored lowercase")
	assert.False(t, org.IsEligible())
	assert.False(t, org.CanIssue())
}

func TestNewRejectsInvalidFacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*organization.NewParams)
		wantField string
	}{
		{name: "nil id", mutate: func(p *organization.NewParams) { p.ID = uuid.Nil }, wantField: "id"},
		{name: "unknown type", mutate: func(p *organization.NewParams) { p.Type = organization.Type("BANK") }, wantField: "type"},
		{name: "empty name", mutate: func(p *organization.NewParams) { p.Name = "  " }, wantField: "name"},
		{name: "long name", mutate: func(p *organization.NewParams) { p.Name = longString(organization.MaxNameLen + 1) }, wantField: "name"},
		{name: "wallet without prefix", mutate: func(p *organization.NewParams) { p.Wallet = testWallet[2:] }, wantField: "wallet"},
		{name: "short wallet", mutate: func(p *organization.NewParams) { p.Wallet = "0xdeadbeef" }, wantField: "wallet"},
		{name: "wallet with bad characters", mutate: func(p *organization.NewParams) {
			p.Wallet = "0xZZ34567890abcdef1234567890abcdef12345678"
		}, wantField: "wallet"},
		{name: "empty wallet", mutate: func(p *organization.NewParams) { p.Wallet = "" }, wantField: "wallet"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := validParams()
			tc.mutate(&p)

			org, err := organization.New(p, testNow)
			require.Nil(t, org)
			require.ErrorIs(t, err, apperr.ErrValidation)

			fields := make([]string, 0)
			for _, f := range apperr.Fields(err) {
				fields = append(fields, f.Field)
			}
			assert.Contains(t, fields, tc.wantField)
		})
	}
}

func TestEligibilityDecisions(t *testing.T) {
	t.Parallel()

	org, err := organization.New(validParams(), testNow)
	require.NoError(t, err)

	require.NoError(t, org.Approve(testNow.Add(time.Minute)))
	assert.True(t, org.IsEligible())
	assert.True(t, org.CanIssue())
	assert.False(t, org.CanInvest(), "an issuer does not become an investor by being eligible")
	assert.Equal(t, int64(2), org.Version)

	require.ErrorIs(t, org.Approve(testNow.Add(2*time.Minute)), apperr.ErrConflict, "approving twice is a conflict")
	assert.Equal(t, int64(2), org.Version, "a refused command does not bump the version")

	require.NoError(t, org.Reject("demo eligibility withdrawn", testNow.Add(3*time.Minute)))
	assert.False(t, org.IsEligible())
	assert.Equal(t, "demo eligibility withdrawn", org.Reason)

	require.ErrorIs(t, org.Reject("again", testNow.Add(4*time.Minute)), apperr.ErrConflict)
	require.NoError(t, org.Approve(testNow.Add(5*time.Minute)), "a rejection can be reversed")
	assert.Empty(t, org.Reason, "approval clears the rejection reason")
}

func TestRejectValidatesItsReason(t *testing.T) {
	t.Parallel()

	org, err := organization.New(validParams(), testNow)
	require.NoError(t, err)

	require.ErrorIs(t, org.Reject("   ", testNow), apperr.ErrValidation)
	require.ErrorIs(t, org.Reject(longString(organization.MaxReasonLen+1), testNow), apperr.ErrValidation)
	assert.Equal(t, organization.EligibilityPending, org.Eligibility)
}

func TestRename(t *testing.T) {
	t.Parallel()

	org, err := organization.New(validParams(), testNow)
	require.NoError(t, err)

	require.NoError(t, org.Rename("  Northwind Trading OÜ  ", testNow.Add(time.Minute)))
	assert.Equal(t, "Northwind Trading OÜ", org.Name)
	assert.Equal(t, int64(2), org.Version)

	require.ErrorIs(t, org.Rename("", testNow), apperr.ErrValidation)
	require.ErrorIs(t, org.Rename(longString(organization.MaxNameLen+1), testNow), apperr.ErrValidation)
}

func TestInvestorPermissions(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.Type = organization.TypeInvestor

	org, err := organization.New(p, testNow)
	require.NoError(t, err)
	require.NoError(t, org.Approve(testNow))

	assert.True(t, org.CanInvest())
	assert.False(t, org.CanIssue())
}

func TestParsers(t *testing.T) {
	t.Parallel()

	orgType, err := organization.ParseType("INVESTOR")
	require.NoError(t, err)
	assert.Equal(t, organization.TypeInvestor, orgType)

	_, err = organization.ParseType("investor")
	require.ErrorIs(t, err, apperr.ErrValidation)

	eligibility, err := organization.ParseEligibility("ELIGIBLE")
	require.NoError(t, err)
	assert.Equal(t, organization.EligibilityEligible, eligibility)

	_, err = organization.ParseEligibility("MAYBE")
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
