package identity_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
)

// apiFixture wires the login endpoints behind the real router, including the resolver that
// later requests are authenticated by.
type apiFixture struct {
	*fixture
	router http.Handler
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()

	f := &apiFixture{fixture: newFixture(t)}
	handler := identity.NewHandler(f.service, false)
	resolver := identity.NewResolver(f.service, identity.SessionCookie)

	f.router = httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) {
			r.Use(httpserver.Authenticate(resolver))
			handler.Routes(r)

			r.Group(func(protected chi.Router) {
				protected.Use(httpserver.RequireActor)
				protected.Get("/invoices", func(w http.ResponseWriter, req *http.Request) {
					httpserver.WriteJSON(w, req, http.StatusOK, map[string]string{"status": "ok"})
				})
			})
		},
	})
	return f
}

func (f *apiFixture) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *apiFixture) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	return f.do(t, httptest.NewRequest(http.MethodPost, path, reader))
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// TestLoginFlow walks the whole exchange the way a browser does it.
func TestLoginFlow(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.post(t, "/api/v1/auth/challenge", `{"wallet":"`+f.wallet.address+`"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	challenge := decode(t, rec)
	message := challenge["message"].(string)
	nonce := challenge["nonce"].(string)
	assert.Contains(t, message, nonce)

	rec = f.post(t, "/api/v1/auth/verify",
		`{"nonce":"`+nonce+`","signature":"`+f.wallet.sign(t, message)+`"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	session := decode(t, rec)
	assert.Equal(t, f.orgID.String(), session["organization_id"])

	// The cookie a browser will send back on its own.
	var sessionCookie *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == identity.SessionCookie {
			sessionCookie = cookie
		}
	}
	require.NotNil(t, sessionCookie)
	assert.True(t, sessionCookie.HttpOnly, "script must not be able to read the session")
	assert.Equal(t, http.SameSiteStrictMode, sessionCookie.SameSite,
		"a cookie session needs this, or another site could act as the user")

	// The cookie authenticates a protected route.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/invoices", http.NoBody)
	req.AddCookie(sessionCookie)
	assert.Equal(t, http.StatusOK, f.do(t, req).Code)

	// And so does the token, for a client that holds it itself.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/invoices", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+session["token"].(string))
	assert.Equal(t, http.StatusOK, f.do(t, req).Code)
}

func TestMeReportsTheCaller(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	token := f.login(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := f.do(t, req)
	require.Equal(t, http.StatusOK, rec.Code)

	me := decode(t, rec)
	assert.Equal(t, f.orgID.String(), me["organization_id"])
	assert.True(t, me["eligible"].(bool))

	// A page loaded without a session finds out here rather than by guessing.
	rec = f.do(t, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestLogoutEndsTheSession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	token := f.login(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := f.do(t, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	cleared := false
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == identity.SessionCookie && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared, "the browser is told to drop the cookie")

	req = httptest.NewRequest(http.MethodGet, "/api/v1/invoices", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	assert.Equal(t, http.StatusUnauthorized, f.do(t, req).Code)
}

func TestLoginValidation(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	tests := []struct {
		name       string
		path       string
		body       string
		wantStatus int
	}{
		{name: "malformed wallet", path: "/api/v1/auth/challenge", body: `{"wallet":"nope"}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "missing wallet", path: "/api/v1/auth/challenge", body: `{}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "unknown field", path: "/api/v1/auth/challenge", body: `{"wallet":"0x00","chain":1}`, wantStatus: http.StatusBadRequest},
		{name: "missing nonce", path: "/api/v1/auth/verify", body: `{"signature":"0xdeadbeef"}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "unknown nonce", path: "/api/v1/auth/verify", body: `{"nonce":"abc","signature":"0xdeadbeef"}`, wantStatus: http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.post(t, tc.path, tc.body)
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
		})
	}
}

// TestChallengeDoesNotRevealRegistration keeps the endpoint from becoming a directory of
// participants: any well-formed address gets a challenge.
func TestChallengeDoesNotRevealRegistration(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	stranger := newWallet(t)

	rec := f.post(t, "/api/v1/auth/challenge", `{"wallet":"`+stranger.address+`"}`)
	assert.Equal(t, http.StatusCreated, rec.Code)

	// The refusal comes only after the signature proves who is asking.
	challenge := decode(t, rec)
	rec = f.post(t, "/api/v1/auth/verify",
		`{"nonce":"`+challenge["nonce"].(string)+`","signature":"`+stranger.sign(t, challenge["message"].(string))+`"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSecureCookieOutsideDevelopment(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	handler := identity.NewHandler(f.service, true)

	router := httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) { handler.Routes(r) },
	})

	challenge, err := f.service.Challenge(t.Context(), f.wallet.address)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verify",
		strings.NewReader(`{"nonce":"`+challenge.Nonce+`","signature":"`+f.wallet.sign(t, challenge.Message)+`"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == identity.SessionCookie {
			assert.True(t, cookie.Secure, "a session cookie must not travel over plain HTTP")
		}
	}
}
