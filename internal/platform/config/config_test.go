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
	assert.False(t, cfg.Providers.GraphIsLive(), "no credentials means the in-process provider")
	assert.False(t, cfg.Providers.CREIsLive())
	assert.False(t, cfg.Providers.HederaIsLive())
}

func TestLoadFromEnvironment(t *testing.T) {
	t.Setenv("FF_ENV", "production")
	t.Setenv("FF_HTTP_ADDR", ":9090")
	t.Setenv("FF_LOG_LEVEL", "debug")
	t.Setenv("FF_DATABASE_URL", "postgres://app:secret@db.internal:5432/factorflow")
	t.Setenv("FF_OUTBOX_INTERVAL", "250ms")
	t.Setenv("FF_GRAPH_API_KEY", "key")
	t.Setenv("FF_GRAPH_GATEWAY_URL", "https://gateway.example")
	t.Setenv("FF_CRE_ENDPOINT", "https://cre.example")
	t.Setenv("FF_HEDERA_ACCOUNT_ID", "0.0.1234")
	t.Setenv("FF_HEDERA_PRIVATE_KEY", "302e...")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, config.Production, cfg.Env)
	assert.Equal(t, ":9090", cfg.HTTPAddr)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
	assert.Equal(t, 250*time.Millisecond, cfg.OutboxInterval)
	assert.True(t, cfg.Providers.GraphIsLive())
	assert.True(t, cfg.Providers.CREIsLive())
	assert.True(t, cfg.Providers.HederaIsLive())
	assert.False(t, cfg.DemoAuthEnabled(), "a production deployment never trusts the demo header")
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
