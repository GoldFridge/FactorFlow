// Package config loads the application's settings from the environment.
//
// Every setting has one name, one default and one place it is validated. The rule that
// matters most is the last one: a deployment that is not development refuses to start on
// development defaults, so a demo server cannot quietly run with the local database URL and
// an open authentication shortcut.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// Environment names a deployment kind.
type Environment string

// The environments the application knows.
const (
	// Development is a laptop or a CI job: fakes are allowed, and so is the demo
	// authentication shortcut.
	Development Environment = "development"
	// Staging and Production are shared deployments; both refuse development shortcuts.
	Staging    Environment = "staging"
	Production Environment = "production"
)

// IsProductionLike reports whether the environment forbids development shortcuts.
func (e Environment) IsProductionLike() bool { return e == Staging || e == Production }

// String returns the wire representation.
func (e Environment) String() string { return string(e) }

// Config is the whole configuration of the process.
type Config struct {
	Env      Environment
	HTTPAddr string
	LogLevel slog.Level

	DatabaseURL string

	// WebDir is the built single-page application this process serves. Empty means the
	// process is an API and nothing else, which is what a split deployment wants.
	WebDir string

	// AutoApprove admits a wallet the moment it registers.
	//
	// It defaults to development only, because eligibility is a decision somebody is meant
	// to take. A public demo is the honest exception: a judge who registers at midnight has
	// nobody to admit them, and a venue nobody can enter demonstrates nothing.
	AutoApprove bool

	// OutboxInterval is how often the worker polls for due events.
	OutboxInterval time.Duration

	// Providers are the external systems. An empty value selects the in-process
	// implementation, which is what makes the offline demo work without pretending to be
	// live.
	Providers Providers

	// Paid is what the machine endpoints charge and where the money goes.
	Paid PaidAPI
}

// PaidAPI configures the x402 endpoints.
type PaidAPI struct {
	// Price is what one paid answer costs, as a decimal string with Currency beside it. A
	// price is never a float: it is quoted to a client's wallet exactly as written.
	Price    string
	Currency string
	// Recipient is the address payments must reach. Without one the endpoints still work
	// in development against the in-process ledger, which pays a placeholder address.
	Recipient string
	// Network and Asset are what the 402 tells a client to pay in.
	Network string
	Asset   string
	// FacilitatorURL is the external verifier. Empty selects the in-process facilitator.
	FacilitatorURL string
}

// IsLive reports whether a real facilitator is configured.
func (p PaidAPI) IsLive() bool { return p.FacilitatorURL != "" }

// Providers holds the credentials and endpoints of the external systems.
type Providers struct {
	HederaAccountID  string
	HederaPrivateKey string
	// HederaNetwork is testnet unless something says otherwise: a demo that mints on
	// mainnet by forgetting a variable is not a mistake worth leaving available.
	HederaNetwork   string
	GraphAPIKey     string
	GraphGatewayURL string
	// CREToken authorizes a confidential workflow to collect work and return results.
	//
	// It is a token rather than an endpoint because the traffic goes the other way: the
	// workflow runs on Chainlink's infrastructure, wakes on its own schedule and calls in.
	// There is nothing here for this platform to dial.
	CREToken string
	// LLM is the model that puts a published score into words. It never decides anything:
	// the assessment exists before it is asked, and what it writes is checked against the
	// numbers that assessment published.
	LLMAPIKey  string
	LLMBaseURL string
	LLMModel   string
	// GraphNetwork and GraphAsset name the question the benchmark answers: which chain's
	// lending markets, denominated in what.
	GraphNetwork string
	GraphAsset   string
}

// GraphIsLive reports whether a live Graph gateway is configured.
func (p Providers) GraphIsLive() bool { return p.GraphAPIKey != "" && p.GraphGatewayURL != "" }

// LLMIsLive reports whether a model is configured to narrate assessments.
func (p Providers) LLMIsLive() bool { return p.LLMAPIKey != "" && p.LLMModel != "" }

// CREIsLive reports whether a confidential workflow collects work from this deployment.
func (p Providers) CREIsLive() bool { return p.CREToken != "" }

// HederaIsLive reports whether Hedera credentials are configured.
func (p Providers) HederaIsLive() bool { return p.HederaAccountID != "" && p.HederaPrivateKey != "" }

// developmentPaymentRecipient is the placeholder the in-process facilitator pays to. A
// deployment that charges real money must set FF_PAID_RECIPIENT, and validate refuses to
// start production-like without one.
const developmentPaymentRecipient = "0x0000000000000000000000000000000000000402"

// developmentDatabaseURL is the URL the local compose file serves. A production-like
// deployment that still carries it has not been configured.
const developmentDatabaseURL = "postgres://factorflow:factorflow@localhost:5432/factorflow?sslmode=disable"

// Load reads the configuration from the environment.
func Load() (Config, error) {
	// A local .env fills in what the environment has not set, never the other way round.
	loadDotEnv(DotEnvFile)

	cfg := Config{
		Env:            Environment(strings.ToLower(envOr("FF_ENV", string(Development)))),
		HTTPAddr:       envOr("FF_HTTP_ADDR", ":8080"),
		DatabaseURL:    envOr("FF_DATABASE_URL", developmentDatabaseURL),
		WebDir:         os.Getenv("FF_WEB_DIR"),
		OutboxInterval: time.Second,
		Providers: Providers{
			HederaAccountID:  os.Getenv("FF_HEDERA_ACCOUNT_ID"),
			HederaPrivateKey: os.Getenv("FF_HEDERA_PRIVATE_KEY"),
			HederaNetwork:    envOr("FF_HEDERA_NETWORK", "testnet"),
			GraphAPIKey:      os.Getenv("FF_GRAPH_API_KEY"),
			GraphGatewayURL:  os.Getenv("FF_GRAPH_GATEWAY_URL"),
			GraphNetwork:     envOr("FF_GRAPH_NETWORK", "ethereum"),
			GraphAsset:       envOr("FF_GRAPH_ASSET", "USDC"),
			CREToken:         os.Getenv("FF_CONFIDENTIAL_TOKEN"),
			LLMAPIKey:        os.Getenv("FF_LLM_API_KEY"),
			LLMBaseURL:       envOr("FF_LLM_BASE_URL", "https://api.deepseek.com"),
			LLMModel:         envOr("FF_LLM_MODEL", "deepseek-chat"),
		},
		Paid: PaidAPI{
			Price:          envOr("FF_PAID_PRICE", "0.25"),
			Currency:       envOr("FF_PAID_CURRENCY", "USD"),
			Recipient:      envOr("FF_PAID_RECIPIENT", developmentPaymentRecipient),
			Network:        envOr("FF_PAID_NETWORK", "local"),
			Asset:          envOr("FF_PAID_ASSET", "USDC"),
			FacilitatorURL: os.Getenv("FF_PAID_FACILITATOR_URL"),
		},
	}

	cfg.AutoApprove = cfg.Env == Development
	if raw := os.Getenv("FF_AUTO_APPROVE"); raw != "" {
		approve, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("FF_AUTO_APPROVE must be true or false: %w", err)
		}
		cfg.AutoApprove = approve
	}

	level, err := parseLevel(envOr("FF_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	if raw := os.Getenv("FF_OUTBOX_INTERVAL"); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval <= 0 {
			return Config{}, apperr.Invalid("FF_OUTBOX_INTERVAL", "must be a positive duration such as 1s")
		}
		cfg.OutboxInterval = interval
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	var violations []error

	switch c.Env {
	case Development, Staging, Production:
	default:
		violations = append(violations, apperr.Invalid("FF_ENV",
			"must be development, staging or production, got %q", c.Env))
	}

	if strings.TrimSpace(c.HTTPAddr) == "" {
		violations = append(violations, apperr.Invalid("FF_HTTP_ADDR", "must not be empty"))
	}
	if strings.TrimSpace(c.DatabaseURL) == "" {
		violations = append(violations, apperr.Invalid("FF_DATABASE_URL", "must not be empty"))
	}

	// A shared deployment on development defaults is the failure this check exists for: it
	// would run against a database nobody expects, with the demo authentication shortcut
	// enabled, and look perfectly healthy while doing it.
	if c.Env.IsProductionLike() && c.DatabaseURL == developmentDatabaseURL {
		violations = append(violations, apperr.Invalid("FF_DATABASE_URL",
			"a %s deployment must not use the development default", c.Env))
	}

	// Charging real money into a placeholder address would take payments nobody can spend.
	if c.Env.IsProductionLike() && c.Paid.Recipient == developmentPaymentRecipient {
		violations = append(violations, apperr.Invalid("FF_PAID_RECIPIENT",
			"a %s deployment must name the address that receives payments", c.Env))
	}

	return errors.Join(violations...)
}

// DemoAuthEnabled reports whether the header-based demo authentication may be used.
//
// It is development-only, and deliberately so: it accepts an organization id from a header,
// which is exactly what real authentication must never do.
func (c Config) DemoAuthEnabled() bool { return c.Env == Development }

// Summary renders the configuration for a startup log line, with every secret redacted.
func (c Config) Summary() string {
	return fmt.Sprintf(
		"env=%s addr=%s log=%s database=%s web=%s auto-approve=%t graph=%s cre=%s hedera=%s llm=%s paid=%s",
		c.Env, c.HTTPAddr, c.LogLevel,
		redactURL(c.DatabaseURL),
		servedOrNot(c.WebDir),
		c.AutoApprove,
		liveOrFake(c.Providers.GraphIsLive()),
		liveOrFake(c.Providers.CREIsLive()),
		liveOrFake(c.Providers.HederaIsLive()),
		liveOrFake(c.Providers.LLMIsLive()),
		liveOrFake(c.Paid.IsLive()),
	)
}

// servedOrNot says whether this process also serves the interface.
func servedOrNot(dir string) string {
	if dir == "" {
		return "api-only"
	}
	return dir
}

func liveOrFake(live bool) string {
	if live {
		return "live"
	}
	return "in-process"
}

// redactURL keeps the host and database name and drops the credentials, so a startup line
// is useful in a log without being a leak.
func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	scheme := strings.Index(raw, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return raw
	}
	return raw[:scheme+3] + "***@" + raw[at+1:]
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		if n, err := strconv.Atoi(raw); err == nil {
			return slog.Level(n), nil
		}
		return 0, apperr.Invalid("FF_LOG_LEVEL", "must be debug, info, warn or error, got %q", raw)
	}
}
