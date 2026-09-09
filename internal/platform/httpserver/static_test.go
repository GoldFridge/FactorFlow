package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
)

// builtApp writes what a production build leaves behind: an index that names fingerprinted
// assets, and the assets themselves.
func builtApp(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte(`<!doctype html><script src="/assets/index-abc123.js"></script>`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "index-abc123.js"),
		[]byte("console.log('factorflow')"), 0o600))
	return dir
}

func serve(t *testing.T, dir string) http.Handler {
	t.Helper()

	web, err := httpserver.Static(dir)
	require.NoError(t, err)

	return httpserver.NewRouter(httpserver.Dependencies{
		Web: web,
		Routes: func(r chi.Router) {
			r.Get("/invoices", func(w http.ResponseWriter, req *http.Request) {
				httpserver.WriteJSON(w, req, http.StatusOK, map[string]string{"items": "none"})
			})
		},
	})
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return rec
}

/*
 * TestTheApplicationOwnsItsOwnRoutes is the property a single-page application needs from
 * the server: a deep link is a page, not a missing file. Someone who was sent a link to one
 * receivable has to land on it rather than on a 404 the application never sees.
 */
func TestTheApplicationOwnsItsOwnRoutes(t *testing.T) {
	t.Parallel()

	handler := serve(t, builtApp(t))

	for _, path := range []string{"/", "/invoices", "/listings/9f0c", "/operator"} {
		rec := get(t, handler, path)
		require.Equalf(t, http.StatusOK, rec.Code, "GET %s", path)
		assert.Contains(t, rec.Body.String(), "<!doctype html>")
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"),
			"the index names the fingerprinted assets, so it must not be kept")
	}
}

// TestAssetsAreServedAndKept: a fingerprinted file never changes under its name, so it is
// cached for a year — that is what the fingerprint is for.
func TestAssetsAreServedAndKept(t *testing.T) {
	t.Parallel()

	rec := get(t, serve(t, builtApp(t)), "/assets/index-abc123.js")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "factorflow")
	assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
}

/*
 * TestAMissingFileIsAMissingFile. Answering a missing script with the index is how a broken
 * deployment looks like a working one until the browser tries to parse HTML as JavaScript.
 */
func TestAMissingFileIsAMissingFile(t *testing.T) {
	t.Parallel()

	rec := get(t, serve(t, builtApp(t)), "/assets/index-deadbeef.js")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/problem+json")
}

// TestTheAPIStillAnswersAsAnAPI: an unknown endpoint under the API prefix is a mistake a
// client has to read, not a page.
func TestTheAPIStillAnswersAsAnAPI(t *testing.T) {
	t.Parallel()

	handler := serve(t, builtApp(t))

	ok := get(t, handler, "/api/v1/invoices")
	require.Equal(t, http.StatusOK, ok.Code)
	assert.Contains(t, ok.Header().Get("Content-Type"), "application/json")

	missing := get(t, handler, "/api/v1/nothing-here")
	assert.Equal(t, http.StatusNotFound, missing.Code)
	assert.Contains(t, missing.Header().Get("Content-Type"), "application/problem+json")
	assert.NotContains(t, missing.Body.String(), "<!doctype html>")
}

/*
 * TestThePageMayLoadItself. The API's policy forbids everything, which is right for JSON
 * and would stop the application loading its own script. The page gets its own policy, and
 * it is still a whitelist of one origin.
 */
func TestThePageMayLoadItself(t *testing.T) {
	t.Parallel()

	page := get(t, serve(t, builtApp(t)), "/")
	assert.Equal(t, httpserver.AppCSP, page.Header().Get("Content-Security-Policy"))
	assert.Contains(t, page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")

	api := get(t, serve(t, builtApp(t)), "/api/v1/invoices")
	assert.Equal(t, "default-src 'none'; frame-ancestors 'none'",
		api.Header().Get("Content-Security-Policy"))
}

// TestATraversalReadsNothing: the one attack a file server invites.
func TestATraversalReadsNothing(t *testing.T) {
	t.Parallel()

	dir := builtApp(t)
	secret := filepath.Join(filepath.Dir(dir), "secret.env")
	require.NoError(t, os.WriteFile(secret, []byte("FF_HEDERA_PRIVATE_KEY=0xdead"), 0o600))

	handler := serve(t, dir)

	for _, path := range []string{
		"/../secret.env",
		"/assets/../../secret.env",
		"/%2e%2e/secret.env",
	} {
		rec := get(t, handler, path)
		assert.NotContains(t, rec.Body.String(), "FF_HEDERA_PRIVATE_KEY", "GET %s", path)
	}
}

// TestServingRefusesADirectoryWithoutAnIndex fails at startup rather than at the first
// visitor: a mistyped path in a deployment should stop the process, not serve nothing.
func TestServingRefusesADirectoryWithoutAnIndex(t *testing.T) {
	t.Parallel()

	_, err := httpserver.Static(t.TempDir())
	require.Error(t, err)
}
