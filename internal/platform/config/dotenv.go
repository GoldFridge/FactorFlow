package config

import (
	"bufio"
	"os"
	"strings"
)

// DotEnvFile is the local configuration a developer keeps beside the repository. It is
// deliberately not committed: it is where credentials live on one machine.
const DotEnvFile = ".env"

/*
loadDotEnv fills in variables the environment has not already set.

The precedence matters more than the parsing. A real environment variable always wins, so a
deployment that sets FF_DATABASE_URL cannot be quietly overridden by a file someone left in
the working directory, and running one command with a variable in front of it does what it
looks like it does.

A missing file is not an error. Most environments have no .env at all, and demanding one
would make the deployment case carry the development case's baggage.
*/
func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseDotEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(key); !set {
			_ = os.Setenv(key, value)
		}
	}
}

// parseDotEnvLine reads one KEY=value line, ignoring comments and blank lines.
//
// It understands quotes because a database URL with a # in its password is otherwise
// truncated at what looks like a comment, and that failure is very hard to see.
func parseDotEnvLine(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")

	name, rest, found := strings.Cut(trimmed, "=")
	if !found {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false
	}

	rest = strings.TrimSpace(rest)
	switch {
	case len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"',
		len(rest) >= 2 && rest[0] == '\'' && rest[len(rest)-1] == '\'':
		rest = rest[1 : len(rest)-1]
	default:
		// An unquoted value ends at a comment, which is how these files are usually written.
		if hash := strings.Index(rest, " #"); hash >= 0 {
			rest = strings.TrimSpace(rest[:hash])
		}
	}

	return name, rest, true
}
