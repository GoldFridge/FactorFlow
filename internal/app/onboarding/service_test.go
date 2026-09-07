package onboarding_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/onboarding"
	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/testsupport/memrepo"
	"github.com/GoldFridge/factorflow/internal/testsupport/wallettest"
)

var testNow = time.Date(2026, time.September, 7, 10, 0, 0, 0, time.UTC)

type fixture struct {
	service  *onboarding.Service
	identity *identity.Service
	store    *memrepo.Store
	router   http.Handler

	operator       organization.Organization
	operatorWallet *wallettest.Wallet
	clock          time.Time
}

// newFixture wires onboarding over the in-memory store. autoApprove is the development
// switch: with it off, a new organization has to be approved by an operator.
func newFixture(t *testing.T, autoApprove bool) *fixture {
	t.Helper()

	store := memrepo.New()
	f := &fixture{store: store, clock: testNow}

	f.identity = identity.NewService(store, store.Identity(), accounts{store: store}, func() time.Time { return f.clock })
	f.service = onboarding.NewService(onboarding.Config{
		DB:            store,
		Organizations: store.Organizations(),
		Challenges:    store.Identity(),
		Sessions:      f.identity,
		AutoApprove:   autoApprove,
		Now:           func() time.Time { return f.clock },
		IDs:           uuid.New,
	})

	f.operator = *f.seedOperator(t)

	handler := onboarding.NewHandler(f.service)
	f.router = httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) {
			r.Use(httpserver.Authenticate(identity.NewResolver(f.identity, identity.SessionCookie)))
			handler.PublicRoutes(r)
			handler.Routes(r)
		},
	})
	return f
}

// accounts is the adapter the application layer gives identity so it can answer its one
// question about an organization without importing the module itself.
type accounts struct {
	store *memrepo.Store
}

func (a accounts) ByWallet(ctx context.Context, q postgres.Querier, wallet string) (identity.Account, error) {
	org, err := a.store.Organizations().GetByWallet(ctx, q, wallet)
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

func (f *fixture) seedOperator(t *testing.T) *organization.Organization {
	t.Helper()

	f.operatorWallet = wallettest.New(t)
	org, err := organization.New(organization.NewParams{
		ID: uuid.New(), Type: organization.TypeOperator, Name: "FactorFlow Operations",
		Wallet: f.operatorWallet.Address,
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, org.Approve(testNow))
	f.store.PutOrganization(org)
	return org
}

// register runs the whole registration a browser does: ask for a challenge, sign it, post
// the signature with the organization's facts.
func (f *fixture) register(t *testing.T, w *wallettest.Wallet, orgType organization.Type, name string) (*organization.Organization, error) {
	t.Helper()

	challenge, err := f.identity.Challenge(context.Background(), w.Address)
	require.NoError(t, err)

	return f.service.Register(context.Background(), onboarding.RegisterParams{
		Nonce:     challenge.Nonce,
		Signature: w.Sign(t, challenge.Message),
		Type:      orgType,
		Name:      name,
	})
}

func (f *fixture) actorFor(org *organization.Organization) onboarding.Actor {
	return onboarding.Actor{OrganizationID: org.ID, Operator: org.Type == organization.TypeOperator}
}

// TestRegistrationProvesTheWallet is the reason registration is not a plain create: without
// the signature, anyone could claim someone else's address and lock its owner out.
func TestRegistrationProvesTheWallet(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	w := wallettest.New(t)

	org, err := f.register(t, w, organization.TypeIssuer, "Northwind Trading")
	require.NoError(t, err)

	assert.Equal(t, organization.TypeIssuer, org.Type)
	assert.Equal(t, strings.ToLower(w.Address), org.Wallet)
	assert.Equal(t, organization.EligibilityPending, org.Eligibility,
		"a new participant waits for an eligibility decision")

	stored, ok := f.store.Organization(org.ID)
	require.True(t, ok)
	assert.Equal(t, org.Wallet, stored.Wallet)

	// And the wallet can now sign in as the organization it just created.
	challenge, err := f.identity.Challenge(t.Context(), w.Address)
	require.NoError(t, err)
	session, err := f.identity.Verify(t.Context(), challenge.Nonce, w.Sign(t, challenge.Message))
	require.NoError(t, err)
	assert.Equal(t, org.ID, session.OrganizationID)
}

// TestRegistrationRefusesAForgedSignature covers the case the whole design is aimed at:
// registering an address whose key the caller does not hold.
func TestRegistrationRefusesAForgedSignature(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	victim := wallettest.New(t)
	attacker := wallettest.New(t)

	challenge, err := f.identity.Challenge(t.Context(), victim.Address)
	require.NoError(t, err)

	_, err = f.service.Register(t.Context(), onboarding.RegisterParams{
		Nonce:     challenge.Nonce,
		Signature: attacker.Sign(t, challenge.Message),
		Type:      organization.TypeIssuer,
		Name:      "Not Mine",
	})
	require.Error(t, err)
	assert.Equal(t, 1, f.store.OrganizationCount(), "only the seeded operator exists")
}

func TestRegistrationRejectsAnUnknownOrExpiredChallenge(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	w := wallettest.New(t)

	_, err := f.service.Register(t.Context(), onboarding.RegisterParams{
		Nonce: "", Signature: "0xdead", Type: organization.TypeIssuer, Name: "Northwind",
	})
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.service.Register(t.Context(), onboarding.RegisterParams{
		Nonce: "unknown-nonce", Signature: "0xdead", Type: organization.TypeIssuer, Name: "Northwind",
	})
	require.ErrorIs(t, err, apperr.ErrForbidden)

	challenge, err := f.identity.Challenge(t.Context(), w.Address)
	require.NoError(t, err)
	signature := w.Sign(t, challenge.Message)

	f.clock = testNow.Add(identity.ChallengeTTL + time.Second)
	_, err = f.service.Register(t.Context(), onboarding.RegisterParams{
		Nonce: challenge.Nonce, Signature: signature, Type: organization.TypeIssuer, Name: "Northwind",
	})
	require.ErrorIs(t, err, apperr.ErrForbidden, "a stale challenge is not a proof of anything")
	assert.Equal(t, 1, f.store.OrganizationCount())
}

// TestRegistrationSpendsItsChallenge stops one signature from creating a second
// organization, which would let a wallet register itself twice.
func TestRegistrationSpendsItsChallenge(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	w := wallettest.New(t)

	challenge, err := f.identity.Challenge(t.Context(), w.Address)
	require.NoError(t, err)
	signature := w.Sign(t, challenge.Message)

	params := onboarding.RegisterParams{
		Nonce: challenge.Nonce, Signature: signature, Type: organization.TypeIssuer, Name: "Northwind",
	}
	_, err = f.service.Register(t.Context(), params)
	require.NoError(t, err)

	_, err = f.service.Register(t.Context(), params)
	require.Error(t, err, "the nonce is spent")
	assert.Equal(t, 2, f.store.OrganizationCount(), "the operator and one registration")
}

// TestRegistrationRollsBackOnAConflict proves the challenge and the organization are one
// unit of work: a rejected write must not leave a spent nonce behind, or the caller would
// be unable to retry.
func TestRegistrationRollsBackOnAConflict(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	w := wallettest.New(t)

	_, err := f.register(t, w, organization.TypeIssuer, "Northwind Trading")
	require.NoError(t, err)

	// The same wallet registering again is refused by the unique wallet constraint.
	_, err = f.register(t, w, organization.TypeInvestor, "Northwind Capital")
	require.ErrorIs(t, err, apperr.ErrConflict)
	assert.Equal(t, 2, f.store.OrganizationCount(), "the failed registration wrote nothing")
}

func TestRegistrationValidatesItsFacts(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	w := wallettest.New(t)

	_, err := f.register(t, w, organization.TypeIssuer, "   ")
	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.Equal(t, 1, f.store.OrganizationCount())
}

// TestAutoApproveIsDevelopmentOnly documents the one behavioural difference between the
// demo build and a deployed one.
func TestAutoApproveIsDevelopmentOnly(t *testing.T) {
	t.Parallel()

	development := newFixture(t, true)
	org, err := development.register(t, wallettest.New(t), organization.TypeInvestor, "Alpine Treasury")
	require.NoError(t, err)
	assert.True(t, org.IsEligible(), "a demo participant can trade straight away")

	production := newFixture(t, false)
	org, err = production.register(t, wallettest.New(t), organization.TypeInvestor, "Alpine Treasury")
	require.NoError(t, err)
	assert.False(t, org.IsEligible())
}

func TestEligibilityDecisionsAreOperatorOnly(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	org, err := f.register(t, wallettest.New(t), organization.TypeIssuer, "Northwind Trading")
	require.NoError(t, err)

	self := f.actorFor(org)
	_, err = f.service.Approve(t.Context(), self, org.ID)
	require.ErrorIs(t, err, apperr.ErrForbidden, "nobody approves themselves")

	_, err = f.service.Reject(t.Context(), self, org.ID, "no")
	require.ErrorIs(t, err, apperr.ErrForbidden)

	_, err = f.service.List(t.Context(), self, organization.TypeIssuer, 10)
	require.ErrorIs(t, err, apperr.ErrForbidden, "a participant directory is not public")
}

func TestApproveAndReject(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	operator := f.actorFor(&f.operator)

	org, err := f.register(t, wallettest.New(t), organization.TypeIssuer, "Northwind Trading")
	require.NoError(t, err)

	approved, err := f.service.Approve(t.Context(), operator, org.ID)
	require.NoError(t, err)
	assert.True(t, approved.IsEligible())
	assert.True(t, approved.CanIssue())
	assert.Greater(t, approved.Version, org.Version, "the stored version moved with the decision")

	_, err = f.service.Approve(t.Context(), operator, org.ID)
	require.ErrorIs(t, err, apperr.ErrConflict, "approving twice is a mistake worth reporting")

	rejected, err := f.service.Reject(t.Context(), operator, org.ID, "documents did not match")
	require.NoError(t, err)
	assert.False(t, rejected.IsEligible(), "a rejection takes an eligible participant out of the market")
	assert.Equal(t, "documents did not match", rejected.Reason)

	_, err = f.service.Reject(t.Context(), operator, org.ID, "")
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.service.Approve(t.Context(), operator, uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestRejectionTakesEffectOnTheNextRequest is why the resolver re-reads the organization:
// a session minted while eligible must not outlive the eligibility it was minted under.
func TestRejectionTakesEffectOnTheNextRequest(t *testing.T) {
	t.Parallel()

	f := newFixture(t, true)
	w := wallettest.New(t)

	org, err := f.register(t, w, organization.TypeInvestor, "Alpine Treasury")
	require.NoError(t, err)

	challenge, err := f.identity.Challenge(t.Context(), w.Address)
	require.NoError(t, err)
	session, err := f.identity.Verify(t.Context(), challenge.Nonce, w.Sign(t, challenge.Message))
	require.NoError(t, err)

	actor, err := f.identity.Resolve(t.Context(), session.Token)
	require.NoError(t, err)
	require.True(t, actor.Eligible)

	_, err = f.service.Reject(t.Context(), f.actorFor(&f.operator), org.ID, "withdrawn from the demo")
	require.NoError(t, err)

	actor, err = f.identity.Resolve(t.Context(), session.Token)
	require.NoError(t, err)
	assert.False(t, actor.Eligible, "the existing session stops being eligible immediately")
}

func TestGetIsScopedToTheCaller(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	mine, err := f.register(t, wallettest.New(t), organization.TypeIssuer, "Northwind Trading")
	require.NoError(t, err)
	theirs, err := f.register(t, wallettest.New(t), organization.TypeInvestor, "Alpine Treasury")
	require.NoError(t, err)

	got, err := f.service.Get(t.Context(), f.actorFor(mine), mine.ID)
	require.NoError(t, err)
	assert.Equal(t, mine.ID, got.ID)

	_, err = f.service.Get(t.Context(), f.actorFor(mine), theirs.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)

	got, err = f.service.Get(t.Context(), f.actorFor(&f.operator), theirs.ID)
	require.NoError(t, err, "an operator sees every participant")
	assert.Equal(t, theirs.ID, got.ID)
}

func TestListIsFilteredAndBounded(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	operator := f.actorFor(&f.operator)

	for i := 0; i < 3; i++ {
		_, err := f.register(t, wallettest.New(t), organization.TypeIssuer, "Issuer")
		require.NoError(t, err)
	}
	_, err := f.register(t, wallettest.New(t), organization.TypeInvestor, "Investor")
	require.NoError(t, err)

	issuers, err := f.service.List(t.Context(), operator, organization.TypeIssuer, 10)
	require.NoError(t, err)
	assert.Len(t, issuers, 3)

	all, err := f.service.List(t.Context(), operator, "", 10)
	require.NoError(t, err)
	assert.Len(t, all, 5, "three issuers, one investor and the operator")

	limited, err := f.service.List(t.Context(), operator, "", 2)
	require.NoError(t, err)
	assert.Len(t, limited, 2)

	_, err = f.service.List(t.Context(), operator, organization.Type("BANK"), 10)
	require.ErrorIs(t, err, apperr.ErrValidation)
}

// login returns a session token for a wallet that already has an organization.
func (f *fixture) login(t *testing.T, w *wallettest.Wallet) string {
	t.Helper()

	challenge, err := f.identity.Challenge(context.Background(), w.Address)
	require.NoError(t, err)
	session, err := f.identity.Verify(context.Background(), challenge.Nonce, w.Sign(t, challenge.Message))
	require.NoError(t, err)
	return session.Token
}

func (f *fixture) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader = http.NoBody
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}
