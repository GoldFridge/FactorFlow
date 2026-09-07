package onboarding_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/testsupport/wallettest"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// registerBody asks for a challenge and returns the registration body a browser would post.
func (f *fixture) registerBody(t *testing.T, w *wallettest.Wallet, orgType, name string) string {
	t.Helper()

	challenge, err := f.identity.Challenge(t.Context(), w.Address)
	require.NoError(t, err)

	body, err := json.Marshal(map[string]string{
		"nonce":     challenge.Nonce,
		"signature": w.Sign(t, challenge.Message),
		"type":      orgType,
		"name":      name,
	})
	require.NoError(t, err)
	return string(body)
}

// TestRegisterEndpointIsReachableWithoutASession is the point of the public route: a wallet
// with no organization cannot authenticate, so it proves itself with a signature instead.
func TestRegisterEndpointIsReachableWithoutASession(t *testing.T) {
	t.Parallel()

	f := newFixture(t, true)
	w := wallettest.New(t)

	rec := f.do(t, http.MethodPost, "/api/v1/organizations", "", f.registerBody(t, w, "ISSUER", "Northwind Trading"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	body := decode(t, rec)
	assert.Equal(t, "ISSUER", body["type"])
	assert.Equal(t, "Northwind Trading", body["name"])
	assert.Equal(t, "ELIGIBLE", body["eligibility"], "the development build approves on registration")
	assert.True(t, body["can_issue"].(bool))
	assert.False(t, body["can_invest"].(bool))

	// The new organization can now sign in and read itself back.
	token := f.login(t, w)
	rec = f.do(t, http.MethodGet, "/api/v1/organizations/"+body["id"].(string), token, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, body["id"], decode(t, rec)["id"])
}

func TestRegisterEndpointValidation(t *testing.T) {
	t.Parallel()

	f := newFixture(t, true)
	w := wallettest.New(t)

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "unknown type", body: f.registerBody(t, w, "BANK", "Northwind"), wantStatus: http.StatusUnprocessableEntity},
		{name: "empty name", body: f.registerBody(t, wallettest.New(t), "ISSUER", "  "), wantStatus: http.StatusUnprocessableEntity},
		{name: "unknown nonce", body: `{"nonce":"nope","signature":"0xdead","type":"ISSUER","name":"Northwind"}`, wantStatus: http.StatusForbidden},
		{name: "unknown field", body: `{"nonce":"a","signature":"0xdead","type":"ISSUER","name":"N","chain":1}`, wantStatus: http.StatusBadRequest},
		{name: "not json", body: `{`, wantStatus: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/api/v1/organizations", "", tc.body)
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
		})
	}
}

func TestEligibilityEndpointsAreOperatorOnly(t *testing.T) {
	t.Parallel()

	f := newFixture(t, false)
	w := wallettest.New(t)

	org, err := f.register(t, w, organization.TypeIssuer, "Northwind Trading")
	require.NoError(t, err)

	// A pending participant can still sign in; what it cannot do is decide anything.
	participant := f.login(t, w)
	operator := f.login(t, f.operatorWallet)
	path := "/api/v1/organizations/" + org.ID.String()

	assert.Equal(t, http.StatusForbidden, f.do(t, http.MethodPost, path+"/approve", participant, "").Code)
	assert.Equal(t, http.StatusForbidden, f.do(t, http.MethodGet, "/api/v1/organizations", participant, "").Code)

	rec := f.do(t, http.MethodPost, path+"/approve", operator, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "ELIGIBLE", decode(t, rec)["eligibility"])

	rec = f.do(t, http.MethodPost, path+"/reject", operator, `{"reason":"documents did not match"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "REJECTED", decode(t, rec)["eligibility"])
	assert.Equal(t, "documents did not match", decode(t, rec)["reason"])

	assert.Equal(t, http.StatusUnprocessableEntity,
		f.do(t, http.MethodPost, path+"/reject", operator, `{"reason":""}`).Code)
	assert.Equal(t, http.StatusNotFound,
		f.do(t, http.MethodPost, "/api/v1/organizations/"+uuid.NewString()+"/approve", operator, "").Code)
	assert.Equal(t, http.StatusUnprocessableEntity,
		f.do(t, http.MethodPost, "/api/v1/organizations/not-a-uuid/approve", operator, "").Code)
}

func TestListEndpointFiltersAndBounds(t *testing.T) {
	t.Parallel()

	f := newFixture(t, true)
	operator := f.login(t, f.operatorWallet)

	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusCreated, f.do(t, http.MethodPost, "/api/v1/organizations", "",
			f.registerBody(t, wallettest.New(t), "ISSUER", "Northwind Trading")).Code)
	}

	rec := f.do(t, http.MethodGet, "/api/v1/organizations?type=ISSUER", operator, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, decode(t, rec)["items"], 2)

	rec = f.do(t, http.MethodGet, "/api/v1/organizations?limit=1", operator, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, decode(t, rec)["items"], 1)

	assert.Equal(t, http.StatusUnprocessableEntity,
		f.do(t, http.MethodGet, "/api/v1/organizations?limit=0", operator, "").Code)
	assert.Equal(t, http.StatusUnprocessableEntity,
		f.do(t, http.MethodGet, "/api/v1/organizations?type=BANK", operator, "").Code)
}

// TestGetEndpointHidesOtherParticipants keeps the refusal from confirming that an
// organization exists at all.
func TestGetEndpointHidesOtherParticipants(t *testing.T) {
	t.Parallel()

	f := newFixture(t, true)
	mine := wallettest.New(t)

	_, err := f.register(t, mine, organization.TypeIssuer, "Northwind Trading")
	require.NoError(t, err)
	theirs, err := f.register(t, wallettest.New(t), organization.TypeInvestor, "Alpine Treasury")
	require.NoError(t, err)

	token := f.login(t, mine)
	assert.Equal(t, http.StatusNotFound,
		f.do(t, http.MethodGet, "/api/v1/organizations/"+theirs.ID.String(), token, "").Code)
	assert.Equal(t, http.StatusOK,
		f.do(t, http.MethodGet, "/api/v1/organizations/"+theirs.ID.String(), f.login(t, f.operatorWallet), "").Code)
}
