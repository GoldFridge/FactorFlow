/*
Package deploy_test checks the deployment description against itself.

Nothing here runs the deployment. It checks the two things that are wrong in a way no unit
test would ever notice, because they are only wrong on a server: instructions that do not
work when followed, and a production service that quietly carries a development setting.
*/
package deploy_test

import (
	"os"
	"strings"
	"testing"
)

func read(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

/*
TestTheDocumentedCommandsCanBeRun.

The compose file lives in deploy/, so Compose interpolates deploy/.env, while .env.example
puts DOMAIN and POSTGRES_PASSWORD in the repository root. A command copied out of the header
without --env-file therefore fails on a server that was set up exactly as documented, with
an error about a variable the operator can see they have already set.
*/
func TestTheDocumentedCommandsCanBeRun(t *testing.T) {
	t.Parallel()

	for _, line := range strings.Split(read(t, "docker-compose.prod.yml"), "\n") {
		if !strings.Contains(line, "docker compose") {
			continue
		}
		if !strings.Contains(line, "--env-file .env") {
			t.Errorf("a documented command reads the wrong env file:\n\t%s", strings.TrimSpace(line))
		}
	}
}

// TestTheDeploymentVariablesAreDocumented: the two settings compose cannot start without are
// the two an operator has no way to guess.
func TestTheDeploymentVariablesAreDocumented(t *testing.T) {
	t.Parallel()

	example := read(t, "../.env.example")
	for _, name := range []string{"DOMAIN=", "POSTGRES_PASSWORD="} {
		if !strings.Contains(example, name) {
			t.Errorf(".env.example does not mention %s, which compose refuses to start without", name)
		}
	}
}

/*
TestProductionOverridesTheDevelopmentEnvironment.

The app service reads the same .env a developer uses, which is what lets a machine be set up
by copying one file. It is only safe because the settings that decide what the deployment is
are set in the compose file, where they win: without these two lines a server would come up
in development mode, against a database on a localhost that does not exist inside a
container, with the header-based authentication shortcut enabled.
*/
func TestProductionOverridesTheDevelopmentEnvironment(t *testing.T) {
	t.Parallel()

	compose := read(t, "docker-compose.prod.yml")

	if !strings.Contains(compose, "FF_ENV: production") {
		t.Error("the app service does not force FF_ENV, so a development .env would decide")
	}
	if !strings.Contains(compose, "FF_DATABASE_URL: postgres://factorflow:${POSTGRES_PASSWORD}@postgres:5432") {
		t.Error("the app service does not force FF_DATABASE_URL at the postgres service")
	}
	if !strings.Contains(compose, "env_file:\n      - ../.env") {
		t.Error("the app service no longer reads the repository root .env")
	}
}

// TestTheDatabaseIsNotPublished. A demo server is a machine on the public internet with a
// password somebody chose in a hurry; the database belongs on the compose network only.
func TestTheDatabaseIsNotPublished(t *testing.T) {
	t.Parallel()

	compose := read(t, "docker-compose.prod.yml")
	postgres := compose[strings.Index(compose, "  postgres:"):strings.Index(compose, "  app:")]

	if strings.Contains(postgres, "ports:") {
		t.Error("the database publishes a port to the host")
	}
}

/*
TestTLSIsTerminatedForTheCookie. The session cookie is marked Secure outside development, so
a deployment reachable over plain HTTP is one where nobody can stay signed in. Caddy holding
443 is what makes the rest of the system usable, not a hardening detail.
*/
func TestTLSIsTerminatedForTheCookie(t *testing.T) {
	t.Parallel()

	compose := read(t, "docker-compose.prod.yml")
	if !strings.Contains(compose, `"443:443"`) {
		t.Error("nothing terminates TLS, and the session cookie is Secure outside development")
	}

	caddy := read(t, "Caddyfile")
	if !strings.Contains(caddy, "reverse_proxy app:8080") {
		t.Error("the proxy does not reach the application on the port the image exposes")
	}
	if !strings.Contains(caddy, "{$DOMAIN}") {
		t.Error("the site is not bound to DOMAIN, so Caddy has no name to get a certificate for")
	}

	// The second name people type is answered by a redirect rather than a second copy:
	// a session cookie belongs to one host, so serving both would sign somebody out for
	// following a link between them.
	if !strings.Contains(caddy, "redir https://{$DOMAIN}{uri} permanent") {
		t.Error("the alias serves its own copy instead of redirecting to the canonical name")
	}

	// Caddy requests a certificate for every https site it is given, and a name whose DNS
	// does not point at this server cannot pass the challenge. An unconfigured alias must
	// therefore default to a plain-http address, or every deployment that has only one name
	// spends its Let's Encrypt attempts failing on a second one nobody asked for.
	if !strings.Contains(caddy, "{$DOMAIN_ALIAS:http://") {
		t.Error("an unset alias would be a name Caddy tries, and fails, to get a certificate for")
	}
}
