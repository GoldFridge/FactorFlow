// Command factorflow runs the FactorFlow modular monolith: the HTTP API and the background
// workers, in one process against one database.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/app/assessment"
	"github.com/GoldFridge/factorflow/internal/app/issuance"
	"github.com/GoldFridge/factorflow/internal/app/marketplace"
	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/config"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/idempotency"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when configuration fails, so this one failure path
		// writes plainly to stderr.
		fmt.Fprintf(os.Stderr, "factorflow: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))
	slog.Info("starting factorflow", slog.String("version", version), slog.String("config", cfg.Summary()))

	// The process stops on the first signal; a second one is left to the runtime, so an
	// operator can always force an exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Connect(ctx, postgres.DefaultConfig(cfg.DatabaseURL))
	if err != nil {
		return err
	}
	defer db.Close()

	if err := postgres.Migrate(ctx, db); err != nil {
		return err
	}
	schemaVersion, err := postgres.MigrationVersion(ctx, db)
	if err != nil {
		return err
	}
	slog.Info("schema ready", slog.Int64("version", schemaVersion))

	app := wire(cfg, db)

	// The dispatcher runs beside the server rather than in its own process: one deployable
	// is the specification's choice, and a worker that dies with its API is easier to
	// reason about on a single demo server than one that outlives it.
	workers := make(chan error, 1)
	go func() {
		workers <- app.dispatcher.Run(ctx, cfg.OutboxInterval)
	}()

	server := httpserver.NewServer(cfg.HTTPAddr, app.router)
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("listening", slog.String("addr", cfg.HTTPAddr))
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case err := <-workers:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), httpserver.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down http server: %w", err)
	}
	slog.Info("stopped")
	return nil
}

// application holds what run needs after wiring.
type application struct {
	router     http.Handler
	dispatcher *outbox.Dispatcher
}

// wire builds the object graph.
//
// Every external system is chosen here and nowhere else: with credentials the live adapter
// is used, without them the in-process one. That is what lets the whole path be exercised
// offline without any module knowing which it got.
func wire(cfg config.Config, db *postgres.DB) *application {
	now := time.Now

	invoices := invoice.NewPostgresRepository()
	assessments := risk.NewPostgresRepository()
	snapshots := marketdata.NewPostgresRepository()
	assets := tokenization.NewPostgresRepository()
	auctions := auction.NewPostgresRepository()
	organizations := organization.NewPostgresRepository()

	market := marketdata.NewService(marketProvider(cfg), marketdata.NewNormalizer(), now)

	invoiceService := invoice.NewService(db, invoices, now, uuid.New)
	auctionService := auction.NewService(db, auctions, auction.NewSolver(), now, uuid.New)
	marketplaceService := marketplace.NewService(marketplace.Config{
		DB:          db,
		Invoices:    invoices,
		Assessments: assessments,
		Assets:      assets,
		Auctions:    auctions,
		Solver:      auction.NewSolver(),
		Now:         now,
		IDs:         uuid.New,
	})

	assessmentWorker := assessment.NewAssessmentWorker(assessment.WorkerConfig{
		DB:          db,
		Invoices:    invoices,
		Assessments: assessments,
		Snapshots:   snapshots,
		Market:      market,
		Workflow:    confidentialWorkflow(cfg),
		Model:       risk.ModelV1(),
		Query:       marketQuery(cfg),
		Now:         now,
		IDs:         uuid.New,
	})
	issuanceWorker := issuance.NewWorker(issuance.Config{
		DB:          db,
		Invoices:    invoices,
		Assessments: assessments,
		Assets:      assets,
		Wallets:     organizationWallets{repo: organizations},
		Issuer:      assetIssuer(cfg),
		Now:         now,
		IDs:         uuid.New,
	})

	dispatcher := outbox.NewDispatcher(db, outbox.DefaultDispatcherConfig(), now)
	dispatcher.Register(invoice.TopicAssess, assessmentWorker.Handle)
	dispatcher.Register(invoice.TopicTokenize, issuanceWorker.Handle)

	idempotent := idempotency.NewMiddleware(db, now)
	invoiceHandler := invoice.NewHandler(invoiceService)
	auctionHandler := auction.NewHandler(auctionService)
	marketplaceHandler := marketplace.NewHandler(marketplaceService)

	router := httpserver.NewRouter(httpserver.Dependencies{
		Version: version,
		Ready:   db.Ping,
		Routes: func(r chi.Router) {
			r.Use(httpserver.Authenticate(resolver(cfg, organizations, db)))

			r.Group(func(protected chi.Router) {
				protected.Use(httpserver.RequireActor)
				protected.Use(idempotent.Handler)

				invoiceHandler.Routes(protected)
				auctionHandler.Routes(protected)
				marketplaceHandler.Routes(protected)
			})
		},
	})

	return &application{router: router, dispatcher: dispatcher}
}

// organizationWallets answers the one question issuance has about an organization: which
// wallet receives the supply. It is an adapter rather than an import so the issuance worker
// does not depend on the whole organization module for a single string.
type organizationWallets struct {
	repo organization.Repository
}

func (o organizationWallets) WalletOf(ctx context.Context, q postgres.Querier, organizationID uuid.UUID) (string, error) {
	org, err := o.repo.Get(ctx, q, organizationID)
	if err != nil {
		return "", err
	}
	return org.Wallet, nil
}

// assetIssuer picks the live tokenization studio when Hedera credentials are configured.
func assetIssuer(cfg config.Config) tokenization.Issuer {
	if cfg.Providers.HederaIsLive() {
		slog.Warn("hedera credentials are set but the ATS adapter is not implemented; using the local issuer")
	}
	return tokenization.NewLocalIssuer()
}

// marketProvider picks the live gateway when one is configured, and the deterministic demo
// market set otherwise.
func marketProvider(cfg config.Config) marketdata.Provider {
	if cfg.Providers.GraphIsLive() {
		// The live Graph gateway adapter is not implemented yet; falling back keeps the
		// process honest about what it is running rather than failing at the first price.
		slog.Warn("graph credentials are set but the live adapter is not implemented; using the demo market set")
	}
	return marketdata.NewStaticProvider(marketdata.DemoMarkets()...)
}

func marketQuery(cfg config.Config) marketdata.Query {
	query := marketdata.DemoQuery()
	if cfg.Providers.GraphIsLive() {
		query.Provider = "thegraph-gateway"
	}
	return query
}

// confidentialWorkflow picks the live CRE workflow when one is configured.
func confidentialWorkflow(cfg config.Config) risk.Workflow {
	if cfg.Providers.CREIsLive() {
		slog.Warn("a CRE endpoint is set but the live workflow client is not implemented; using the local workflow")
	}
	return risk.NewDeterministicWorkflow()
}

// resolver picks how a caller is identified.
//
// Wallet authentication is not implemented yet. In development a header names the acting
// organization so the API can be exercised end to end; anywhere else there is no resolver
// at all, and every protected route answers 401. An unimplemented login must fail closed,
// not fall back to trusting a header.
func resolver(cfg config.Config, organizations organization.Repository, db *postgres.DB) httpserver.Resolver {
	if !cfg.DemoAuthEnabled() {
		slog.Warn("wallet authentication is not implemented; protected routes will answer 401")
		return nil
	}

	slog.Warn("development demo authentication is enabled: the X-Demo-Organization header names the caller")
	return httpserver.ResolverFunc(func(r *http.Request) (httpserver.Actor, error) {
		raw := r.Header.Get("X-Demo-Organization")
		if raw == "" {
			return httpserver.Actor{}, nil
		}
		orgID, err := uuid.Parse(raw)
		if err != nil {
			return httpserver.Actor{}, err
		}

		// Only the identity is taken from the header. Everything a rule depends on comes
		// from the stored organization, so the demo shortcut cannot grant eligibility that
		// the record does not have.
		org, err := organizations.Get(r.Context(), db.Querier(), orgID)
		if err != nil {
			return httpserver.Actor{}, err
		}

		return httpserver.Actor{
			OrganizationID: org.ID,
			Wallet:         org.Wallet,
			Role:           httpserver.RoleOwner,
			Eligible:       org.IsEligible(),
			Operator:       org.Type == organization.TypeOperator,
		}, nil
	})
}
