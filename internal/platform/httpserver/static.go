package httpserver

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// AppCSP is the content security policy the single-page application runs under.
//
// It is looser than the API's, because a page has to load its own script and stylesheet,
// and it is still a whitelist of one origin: nothing may be pulled from anywhere else, and
// nothing may frame it. Inline styles are allowed because the interface sets a few widths
// from data — a bar's fill, a meter's share — which are values rather than code.
const AppCSP = "default-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; font-src 'self'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

// Static serves a built single-page application from a directory.
//
// The interesting part is what happens to a path that is not a file. A single-page
// application owns its own routes, so /invoices/... is a page rather than a missing asset,
// and the server answers it with index.html and lets the application read the address. A
// request that names a file and does not find it is still a 404: pretending a missing
// script is the index is how a broken deployment looks like a working one.
func Static(dir string) (http.Handler, error) {
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		return nil, fmt.Errorf("serving the web application from %s: %w", dir, err)
	}

	files := http.FileServer(http.Dir(dir))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			WriteProblem(w, r, apperr.NotFoundf("method %s is not allowed here", r.Method))
			return
		}

		clean := path.Clean("/" + r.URL.Path)
		header := w.Header()
		header.Set("Content-Security-Policy", AppCSP)

		if name, ok := exists(dir, clean); ok {
			// Vite fingerprints what it builds, so an asset under /assets is safe to keep
			// for a year: a new build is a new name rather than a new version of one.
			if strings.HasPrefix(clean, "/assets/") {
				header.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				header.Set("Cache-Control", "no-cache")
			}
			r.URL.Path = name
			files.ServeHTTP(w, r)
			return
		}

		if path.Ext(clean) != "" {
			WriteProblem(w, r, apperr.NotFoundf("no file at %s", clean))
			return
		}

		// The index is never cached: it names the fingerprinted assets, so a stale copy is
		// how a browser keeps running last week's application against this week's API.
		header.Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	}), nil
}

// exists reports whether a request path names a readable file inside dir.
func exists(dir, clean string) (string, bool) {
	if clean == "/" {
		return "", false
	}
	// filepath.Join cleans the result, so a path that climbed out of dir cannot come back
	// looking like one that did not.
	full := filepath.Join(dir, filepath.FromSlash(clean))
	if !strings.HasPrefix(full, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", false
	}

	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return "", false
	}
	return clean, true
}
