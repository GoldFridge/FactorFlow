package config_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/config"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, config.Development, cfg.Env)
	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
	assert.Equal(t, time.Second, cfg.OutboxInterval)
	assert.True(t, cfg.DemoAuthEnabled(), "development may use the demo header")
	assert.True(t, cfg.AutoApprove, "development admits a wallet as it registers")
	assert.Empty(t, cfg.WebDir, "the process is an API unless it is told to serve the app")
	assert.False(t, cfg.Providers.GraphIsLive(), "no credentials means the in-process provider")
	assert.False(t, cfg.Providers.CREIsLive())
	assert.False(t, cfg.Providers.HederaIsLive())
	assert.False(t, cfg.PaidIsLive(), "and nothing to charge a machine customer with")
}

func TestLoadFromEnvironment(t *testing.T) {
	t.Setenv("FF_ENV", "production")
	t.Setenv("FF_HTTP_ADDR", ":9090")
	t.Setenv("FF_LOG_LEVEL", "debug")
	t.Setenv("FF_DATABASE_URL", "postgres://app:secret@db.internal:5432/factorflow")
	t.Setenv("FF_OUTBOX_INTERVAL", "250ms")
	t.Setenv("FF_GRAPH_API_KEY", "key")
	t.Setenv("FF_GRAPH_GATEWAY_URL", "https://gateway.example")
	t.Setenv("FF_CONFIDENTIAL_TOKEN", "a-token-the-vault-releases")
	t.Setenv("FF_HEDERA_ACCOUNT_ID", "0.0.1234")
	t.Setenv("FF_HEDERA_PRIVATE_KEY", "302e...")
	t.Setenv("FF_PAID_RECIPIENT", "0.0.4402")
	t.Setenv("FF_PAID_PRICE", "0.50")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, config.Production, cfg.Env)
	assert.Equal(t, ":9090", cfg.HTTPAddr)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
	assert.Equal(t, 250*time.Millisecond, cfg.OutboxInterval)
	assert.True(t, cfg.Providers.GraphIsLive())
	// The token is what makes the confidential path live: the workflow calls in, so there is
	// no endpoint for this platform to hold.
	assert.True(t, cfg.Providers.CREIsLive())
	assert.True(t, cfg.Providers.HederaIsLive())
	assert.False(t, cfg.DemoAuthEnabled(), "a production deployment never trusts the demo header")
	assert.True(t, cfg.PaidIsLive(), "an account to be paid into, and the keys to reach it")
	assert.Equal(t, "0.50", cfg.Paid.Price, "a price is carried as a decimal string, never a float")
	assert.Equal(t, "0.0.4402", cfg.Paid.Recipient)
}

// TestProductionNeedsSomewhereToBePaid keeps a deployment from charging real money into
// the placeholder address the in-process facilitator uses.
/*
 * TestEligibilityFollowsTheEnvironmentUnlessItIsTold. Admitting a participant is a decision
 * somebody is meant to take, so it is off outside development — and a public demo is the
 * honest exception, because a judge who registers at midnight has nobody to admit them.
 */
func TestEligibilityFollowsTheEnvironmentUnlessItIsTold(t *testing.T) {
	t.Setenv("FF_ENV", "production")
	t.Setenv("FF_DATABASE_URL", "postgres://app:secret@db.internal:5432/factorflow")
	t.Setenv("FF_PAID_RECIPIENT", "0.0.4402")

	strict, err := config.Load()
	require.NoError(t, err)
	assert.False(t, strict.AutoApprove, "production waits for an operator")
	assert.False(t, strict.DemoAuthEnabled(), "and never trusts a header")

	t.Setenv("FF_AUTO_APPROVE", "true")
	open, err := config.Load()
	require.NoError(t, err)
	assert.True(t, open.AutoApprove)
	assert.False(t, open.DemoAuthEnabled(),
		"opening registration is not the same as trusting a header, and must not enable one")

	t.Setenv("FF_AUTO_APPROVE", "perhaps")
	_, err = config.Load()
	require.Error(t, err)
}

func TestProductionNeedsSomewhereToBePaid(t *testing.T) {
	t.Setenv("FF_ENV", "production")
	t.Setenv("FF_DATABASE_URL", "postgres://app:secret@db.internal:5432/factorflow")

	_, err := config.Load()
	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.Contains(t, err.Error(), "FF_PAID_RECIPIENT")
}

// TestProductionRefusesDevelopmentDefaults is the check that keeps a demo server from
// running against the local database with the authentication shortcut on, while looking
// perfectly healthy.
func TestProductionRefusesDevelopmentDefaults(t *testing.T) {
	for _, env := range []string{"production", "staging"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("FF_ENV", env)

			_, err := config.Load()
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Contains(t, err.Error(), "FF_DATABASE_URL")
		})
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		field string
	}{
		{name: "unknown environment", env: map[string]string{"FF_ENV": "qa"}, field: "FF_ENV"},
		{name: "empty address", env: map[string]string{"FF_HTTP_ADDR": " "}, field: "FF_HTTP_ADDR"},
		{name: "unknown log level", env: map[string]string{"FF_LOG_LEVEL": "chatty"}, field: "FF_LOG_LEVEL"},
		{name: "zero outbox interval", env: map[string]string{"FF_OUTBOX_INTERVAL": "0s"}, field: "FF_OUTBOX_INTERVAL"},
		{name: "malformed outbox interval", env: map[string]string{"FF_OUTBOX_INTERVAL": "soon"}, field: "FF_OUTBOX_INTERVAL"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			_, err := config.Load()
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Contains(t, err.Error(), tc.field)
		})
	}
}

// TestSummaryRedactsCredentials keeps a startup line useful without making it a leak.
func TestSummaryRedactsCredentials(t *testing.T) {
	t.Setenv("FF_ENV", "production")
	t.Setenv("FF_DATABASE_URL", "postgres://app:hunter2@db.internal:5432/factorflow")
	t.Setenv("FF_GRAPH_API_KEY", "super-secret-key")
	t.Setenv("FF_GRAPH_GATEWAY_URL", "https://gateway.example")
	t.Setenv("FF_PAID_RECIPIENT", "0.0.4402")

	cfg, err := config.Load()
	require.NoError(t, err)

	summary := cfg.Summary()
	assert.NotContains(t, summary, "hunter2")
	assert.NotContains(t, summary, "super-secret-key")
	assert.Contains(t, summary, "db.internal:5432/factorflow")
	assert.Contains(t, summary, "graph=live")
	assert.Contains(t, summary, "cre=in-process")
}

func TestSummaryHandlesAURLWithoutCredentials(t *testing.T) {
	t.Setenv("FF_DATABASE_URL", "postgres://localhost:5432/factorflow")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Contains(t, cfg.Summary(), "postgres://localhost:5432/factorflow")
}

/*
TestTheStartupLineDoesNotLieAboutPayments.

The line a server prints on start is how an operator learns which integrations are live, and
it used to read the wrong thing: paid was decided by a facilitator URL nobody sets, so a
process charging real HBAR announced itself as in-process on the line above the one saying
it was charging on Hedera. The two now come from the same question.
*/
func TestTheStartupLineDoesNotLieAboutPayments(t *testing.T) {
	t.Setenv("FF_DATABASE_URL", "postgres://app:secret@db.internal:5432/factorflow")
	t.Setenv("FF_HEDERA_ACCOUNT_ID", "0.0.1234")
	t.Setenv("FF_HEDERA_PRIVATE_KEY", "302e...")
	t.Setenv("FF_PAID_RECIPIENT", "0.0.4402")

	live, err := config.Load()
	require.NoError(t, err)
	assert.True(t, live.PaidIsLive())
	assert.Contains(t, live.Summary(), "paid=live")

	// An EVM address is not somewhere Hedera can transfer anything, so it selects the
	// in-process facilitator however real the credentials beside it are.
	t.Setenv("FF_PAID_RECIPIENT", "0x0000000000000000000000000000000000000402")
	evm, err := config.Load()
	require.NoError(t, err)
	assert.False(t, evm.PaidIsLive())
	assert.Contains(t, evm.Summary(), "paid=in-process")

	// And keys without an account to pay into are keys for something else.
	t.Setenv("FF_PAID_RECIPIENT", "0.0.4402")
	t.Setenv("FF_HEDERA_PRIVATE_KEY", "")
	keyless, err := config.Load()
	require.NoError(t, err)
	assert.False(t, keyless.PaidIsLive())
}
