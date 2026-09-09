// Package internal_test enforces the module boundaries the specification draws.
//
// The rules below are the reason the code is a modular monolith rather than a large
// package: a domain module may not reach into another domain module, and the platform may
// not reach into any of them. Both are easy to break by adding one convenient import, and
// impossible to notice later, so they are checked here rather than trusted to review.
package internal_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const modulePath = "github.com/GoldFridge/factorflow"

// layer names the architectural role of a package.
type layer int

const (
	// layerPlatform is infrastructure with no business rules: storage, transport, money.
	layerPlatform layer = iota
	// layerDomain is a business module: invoice, risk, auction and friends.
	layerDomain
	// layerApp is orchestration across modules, which is allowed to import everything.
	layerApp
)

// domainModules are the business modules, each owning one part of the specification.
var domainModules = map[string]struct{}{
	"identity":     {},
	"organization": {},
	"invoice":      {},
	"risk":         {},
	"marketdata":   {},
	"tokenization": {},
	"auction":      {},
	"settlement":   {},
	"payments":     {},
	"redemption":   {},
}

// allowedDomainEdges are the dependencies between domain modules that are deliberate.
//
// Each one is one-directional and stated here rather than discovered by reading imports.
// A risk grade is the risk model's published vocabulary, and an investor bids "at most
// grade C", so the auction has to speak it. The reverse would be the problem: the risk
// model must never know that auctions exist, or pricing could start depending on demand.
var allowedDomainEdges = map[string]map[string]bool{
	"auction": {"risk": true},
}

// TestModuleBoundaries walks every package and checks its imports against its layer.
func TestModuleBoundaries(t *testing.T) {
	t.Parallel()

	packages := collectPackages(t)
	require.NotEmpty(t, packages, "no packages were found to check")

	for pkg, imports := range packages {
		pkgLayer, pkgModule := classify(pkg)

		for _, imported := range imports {
			if !strings.HasPrefix(imported, modulePath+"/internal/") {
				continue // standard library or a third-party dependency
			}
			importLayer, importModule := classify(strings.TrimPrefix(imported, modulePath+"/"))

			switch pkgLayer {
			case layerPlatform:
				// The platform is the bottom of the stack. If it needed a domain type, the
				// dependency is upside down and the type belongs in the platform.
				assert.NotEqualf(t, layerDomain, importLayer,
					"platform package %s imports domain module %s", pkg, importModule)
				assert.NotEqualf(t, layerApp, importLayer,
					"platform package %s imports application package %s", pkg, imported)

			case layerDomain:
				// A domain module may use the platform and itself, nothing else. Crossing to
				// another module is what the application layer is for.
				if importLayer == layerDomain && importModule != pkgModule && !allowedDomainEdges[pkgModule][importModule] {
					t.Errorf("domain module %s imports another domain module %s;"+
						" orchestration across modules belongs in internal/app,"+
						" or the edge belongs in allowedDomainEdges with a reason", pkgModule, importModule)
				}
				assert.NotEqualf(t, layerApp, importLayer,
					"domain module %s imports application package %s", pkgModule, imported)

			case layerApp:
				// The application layer may import anything: composing modules is its job.
			}
		}
	}
}

// TestPlatformHasNoBusinessVocabulary is a cheap check on the same boundary from the other
// side: a platform package naming a domain concept is usually a rule that leaked downwards.
func TestPlatformHasNoBusinessVocabulary(t *testing.T) {
	t.Parallel()

	// apperr and money are shared vocabulary by design; everything else in the platform
	// should be able to describe itself without these words.
	forbidden := []string{"invoice", "auction", "receivable", "debtor"}
	allowed := map[string]bool{
		"internal/platform/money":    true,
		"internal/platform/apperr":   true,
		"internal/platform/pgtest":   true,
		"internal/platform/postgres": true,
	}

	root := filepath.Join("platform")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		pkg := "internal/" + filepath.ToSlash(filepath.Dir(path))
		if allowed[pkg] {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Only declarations are checked: prose in a comment may well mention an invoice
		// while explaining why a rule exists.
		for _, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			lower := strings.ToLower(trimmed)
			for _, word := range forbidden {
				if strings.Contains(lower, word) {
					t.Errorf("%s names the domain concept %q in code: %s", path, word, trimmed)
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
}

// collectPackages maps every package under internal to the packages it imports.
func collectPackages(t *testing.T) map[string][]string {
	t.Helper()

	packages := map[string][]string{}
	fset := token.NewFileSet()

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}

		pkg := "internal/" + filepath.ToSlash(filepath.Dir(path))
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			packages[pkg] = append(packages[pkg], imported)
		}
		return nil
	})
	require.NoError(t, err)

	return packages
}

// classify returns the layer of a package path such as "internal/invoice", along with the
// domain module it belongs to when it has one.
func classify(pkg string) (layer, string) {
	parts := strings.Split(strings.TrimPrefix(pkg, "internal/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return layerApp, ""
	}

	switch parts[0] {
	case "platform":
		return layerPlatform, ""
	case "app":
		return layerApp, ""
	}
	if _, ok := domainModules[parts[0]]; ok {
		return layerDomain, parts[0]
	}
	return layerApp, ""
}

// TestAllowedEdgesStayAcyclic checks the direction of every deliberate dependency: if both
// halves of a pair were allowed, the boundary would be decoration.
func TestAllowedEdgesStayAcyclic(t *testing.T) {
	t.Parallel()

	for from, targets := range allowedDomainEdges {
		for to := range targets {
			assert.Falsef(t, allowedDomainEdges[to][from],
				"%s and %s are allowed to import each other, which is a cycle", from, to)
		}
	}
}
