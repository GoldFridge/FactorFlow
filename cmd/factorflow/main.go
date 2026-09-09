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

	"github.com/GoldFridge/factorflow/internal/app/agents"
	"github.com/GoldFridge/factorflow/internal/app/assessment"
	"github.com/GoldFridge/factorflow/internal/app/collections"
	"github.com/GoldFridge/factorflow/internal/app/demo"
	"github.com/GoldFridge/factorflow/internal/app/documents"
	"github.com/GoldFridge/factorflow/internal/app/issuance"
	"github.com/GoldFridge/factorflow/internal/app/marketplace"
	"github.com/GoldFridge/factorflow/internal/app/onboarding"
	"github.com/GoldFridge/factorflow/internal/app/reporting"
	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/payments"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/clock"
	"github.com/GoldFridge/factorflow/internal/platform/config"
	"github.com/GoldFridge/factorflow/internal/platform/hedera"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/idempotency"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/objects"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/redemption"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/settlement"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		// The logger may not exist yet when configuration fails, so this one failure path
		// writes plainly to stderr.
		fmt.Fprintf(os.Stderr, "factorflow: %v\n", err)
		os.Exit(1)
	}
}

// dispatch picks what this invocation does. The seed shares the server's object graph
// rather than reimplementing it, so seeded data is produced by the code being demonstrated.
func dispatch(args []string) error {
	if len(args) > 0 && args[0] == "seed" {
		return seed()
	}
	return run()
}

// seed builds the demo dataset and exits.
func seed() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

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

	// The seed needs a clock it can move, and the services have to read the same one, so
	// it is chosen here and handed to the whole graph.
	app := wire(cfg, db, clock.At(time.Now().Add(-demo.SeedHistory)), demo.IDs())
	summary, err := app.seeder.Run(ctx)
	if err != nil {
		return err
	}
	if summary.Skipped {
		slog.Info("demo data already present; nothing was created")
		return nil
	}

	slog.Info("demo data ready",
		slog.Int("organizations", summary.Organizations),
		slog.Int("invoices", summary.Invoices),
		slog.Int("auctions", summary.Auctions),
		slog.Int("bids", summary.Bids),
		slog.Int("settlements", summary.Settlements),
		slog.Int("repayments", summary.Repayments))
	return nil
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

	app := wire(cfg, db, clock.Live(), uuid.New)

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
	seeder     *demo.Seeder
}

// wire builds the object graph.
//
// Every external system is chosen here and nowhere else: with credentials the live adapter
// is used, without them the in-process one. That is what lets the whole path be exercised
// offline without any module knowing which it got.
func wire(cfg config.Config, db *postgres.DB, clk *clock.Clock, ids func() uuid.UUID) *application {
	now := clk.Now
	chain := hederaClient(cfg)

	invoices := invoice.NewPostgresRepository()
	assessments := risk.NewPostgresRepository()
	snapshots := marketdata.NewPostgresRepository()
	assets := tokenization.NewPostgresRepository()
	auctions := auction.NewPostgresRepository()
	settlements := settlement.NewPostgresRepository()
	organizations := organization.NewPostgresRepository()
	// One recorder is shared by every module: an audit trail split across several writers
	// is several timelines that can disagree.
	trail := audit.NewPostgresRecorder()

	market := marketdata.NewService(marketProvider(cfg), marketdata.NewNormalizer(), now)

	invoiceService := invoice.NewService(db, invoices, trail, now, ids)
	auctionService := auction.NewService(db, auctions, auction.NewSolver(), trail, now, ids)
	marketplaceService := marketplace.NewService(marketplace.Config{
		DB:          db,
		Invoices:    invoices,
		Assessments: assessments,
		Assets:      assets,
		Auctions:    auctions,
		Settlements: settlements,
		Wallets:     organizationWallets{repo: organizations},
		Solver:      auction.NewSolver(),
		Audit:       trail,
		Now:         now,
		IDs:         ids,
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
		Audit:       trail,
		Now:         now,
		IDs:         ids,
	})
	issuanceWorker := issuance.NewWorker(issuance.Config{
		DB:          db,
		Invoices:    invoices,
		Assessments: assessments,
		Assets:      assets,
		Wallets:     organizationWallets{repo: organizations},
		Issuer:      assetIssuer(cfg, chain),
		Audit:       trail,
		Now:         now,
		IDs:         ids,
	})

	// The settlement saga runs off the outbox like the other workers: a transfer that
	// stopped half-way is retried by delivery rather than by anyone remembering to.
	settlementWorker := marketplace.NewSettlementWorker(marketplaceService, transferExecutor(cfg, now), assets)

	dispatcher := outbox.NewDispatcher(db, outbox.DefaultDispatcherConfig(), now)
	dispatcher.Register(invoice.TopicAssess, assessmentWorker.Handle)
	dispatcher.Register(invoice.TopicTokenize, issuanceWorker.Handle)
	dispatcher.Register(marketplace.TopicSettle, settlementWorker.Handle)

	identityService := identity.NewService(db, identity.NewPostgresRepository(),
		organizationAccounts{repo: organizations}, now)
	identityHandler := identity.NewHandler(identityService, cfg.Env.IsProductionLike())

	onboardingService := onboarding.NewService(onboarding.Config{
		DB:            db,
		Organizations: organizations,
		Challenges:    identity.NewPostgresRepository(),
		Sessions:      identityService,
		Audit:         trail,
		// A demo has nobody to approve the first participant, so development grants
		// eligibility on registration. Anywhere else it is an operator's decision.
		AutoApprove: cfg.DemoAuthEnabled(),
		Now:         now,
		IDs:         ids,
	})
	// The machine-facing endpoints. They read the same published model and stored market as
	// the rest of the platform, so an agent's quote and an issuer's price cannot disagree.
	agentService := agents.NewService(agents.Config{
		DB:        db,
		Snapshots: snapshots,
		Auctions:  auctions,
		Model:     risk.ModelV1(),
		Market:    marketQuery(cfg),
		Now:       now,
	})
	paidService := payments.NewService(payments.Config{
		DB:          db,
		Repo:        payments.NewPostgresRepository(),
		Facilitator: paymentFacilitator(cfg, now),
		Network:     cfg.Paid.Network,
		Recipient:   cfg.Paid.Recipient,
		Asset:       cfg.Paid.Asset,
		Now:         now,
		IDs:         ids,
	})

	documentService := documents.NewService(documents.Config{
		DB:       db,
		Invoices: invoices,
		Objects:  objects.NewPostgresStore(),
		Audit:    trail,
		Now:      now,
	})
	collectionService := collections.NewService(collections.Config{
		DB:          db,
		Invoices:    invoices,
		Settlements: settlements,
		Repayments:  redemption.NewPostgresRepository(),
		Audit:       trail,
		IDs:         ids,
		Now:         now,
	})
	reportingService := reporting.NewService(reporting.Config{
		DB:          db,
		Invoices:    invoices,
		Assessments: assessments,
		Snapshots:   snapshots,
		Timeline:    trail,
		Listings:    auctions,
		Market:      marketQuery(cfg),
	})

	idempotent := idempotency.NewMiddleware(db, now)
	invoiceHandler := invoice.NewHandler(invoiceService)
	auctionHandler := auction.NewHandler(auctionService)
	marketplaceHandler := marketplace.NewHandler(marketplaceService)
	onboardingHandler := onboarding.NewHandler(onboardingService)
	reportingHandler := reporting.NewHandler(reportingService, now)
	documentHandler := documents.NewHandler(documentService)
	collectionHandler := collections.NewHandler(collectionService)
	paidHandler := payments.NewHandler(paidService)
	paidPrice := paidPrice(cfg)

	router := httpserver.NewRouter(httpserver.Dependencies{
		Version: version,
		Ready:   db.Ping,
		// The paid endpoints sit at the root because their path is part of the x402
		// contract an agent was given, not part of this platform's own versioning.
		RootRoutes: func(r chi.Router) {
			r.Post(agents.RouteRiskQuote, paidHandler.Paid(agents.RouteRiskQuote, paidPrice, agentService.RiskQuote))
			r.Post(agents.RouteRecommendation,
				paidHandler.Paid(agents.RouteRecommendation, paidPrice, agentService.AuctionRecommendation))
		},
		Routes: func(r chi.Router) {
			r.Use(httpserver.Authenticate(resolver(cfg, identityService, organizations, db)))

			// Logging in cannot require being logged in, and neither can registering:
			// a wallet with no organization has no session to present, so it proves
			// itself with a signature instead.
			identityHandler.Routes(r)
			onboardingHandler.PublicRoutes(r)

			r.Group(func(protected chi.Router) {
				protected.Use(httpserver.RequireActor)
				protected.Use(idempotent.Handler)

				invoiceHandler.Routes(protected)
				auctionHandler.Routes(protected)
				marketplaceHandler.Routes(protected)
				onboardingHandler.Routes(protected)
				reportingHandler.Routes(protected)
				documentHandler.Routes(protected)
				collectionHandler.Routes(protected)
			})
		},
	})

	seeder := demo.NewSeeder(demo.Config{
		DB:            db,
		Organizations: organizations,
		Invoices:      invoiceService,
		Marketplace:   marketplaceService,
		Collections:   collectionService,
		Auctions:      auctionService,
		Dispatcher:    dispatcher,
		Clock:         clk,
	})

	return &application{router: router, dispatcher: dispatcher, seeder: seeder}
}

// organizationAccounts answers identity's one question about an organization: which one a
// wallet acts for, and what it is allowed to do. It is an adapter rather than an import so
// the identity module does not depend on the whole organization module.
type organizationAccounts struct {
	repo organization.Repository
}

func (o organizationAccounts) ByWallet(ctx context.Context, q postgres.Querier, wallet string) (identity.Account, error) {
	org, err := o.repo.GetByWallet(ctx, q, wallet)
	if err != nil {
		return identity.Account{}, err
	}
	return identity.Account{
		OrganizationID: org.ID,
		Wallet:         org.Wallet,
		Eligible:       org.IsEligible(),
		Operator:       org.Type == organization.TypeOperator,
	}, nil
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

// paidPrice is what one machine answer costs.
//
// A price that will not parse is a configuration mistake, not a runtime condition: the
// process refuses to charge an amount nobody wrote down, and falls back to the published
// default rather than to zero.
func paidPrice(cfg config.Config) money.Amount {
	currency, err := money.ParseCurrency(cfg.Paid.Currency)
	if err != nil {
		slog.Error("FF_PAID_CURRENCY is not a supported currency; using USD", slog.String("value", cfg.Paid.Currency))
		currency = money.USD
	}

	price, err := money.Parse(cfg.Paid.Price, currency)
	if err != nil || !price.IsPositive() {
		slog.Error("FF_PAID_PRICE is not a positive amount; using 0.25", slog.String("value", cfg.Paid.Price))
		return money.MustParse("0.25", currency)
	}
	return price
}

// paymentFacilitator picks the live x402 verifier when one is configured.
func paymentFacilitator(cfg config.Config, now func() time.Time) payments.Facilitator {
	if cfg.Paid.IsLive() {
		slog.Warn("an x402 facilitator is configured but the live client is not implemented; using the local one")
	}
	return payments.NewLocalFacilitator(cfg.Paid.Recipient, now)
}

// transferExecutor picks the live chain when Hedera credentials are configured, and the
// in-process ledger otherwise. The saga is the same either way: what changes is only who
// answers Submit and Lookup.
func transferExecutor(cfg config.Config, now func() time.Time) settlement.Executor {
	if cfg.Providers.HederaIsLive() {
		slog.Warn("hedera credentials are set but the transfer executor is not implemented; using the local ledger")
	}
	return settlement.NewLocalExecutor(now)
}

// assetIssuer mints on Hedera when credentials are configured, and in process otherwise.
//
// A broken chain client is not a reason to refuse to start: the local issuer keeps the
// service usable, and the warning says plainly that nothing is reaching a network. What must
// never happen is the opposite — quietly reporting a local asset as though it were on chain,
// which is why the local issuer calls its network "local" rather than naming a real one.
func assetIssuer(cfg config.Config, chain *hedera.Client) tokenization.Issuer {
	if chain == nil {
		if cfg.Providers.HederaIsLive() {
			slog.Warn("hedera credentials are set but the client could not be built; using the local issuer")
		}
		return tokenization.NewLocalIssuer()
	}

	slog.Info("minting receivables on hedera",
		slog.String("network", chain.Network()), slog.String("treasury", chain.Operator()))
	return tokenization.NewHederaIssuer(chain)
}

// hederaClient connects when credentials are present, and reports why it could not rather
// than failing the process: every path it serves has an in-process alternative.
func hederaClient(cfg config.Config) *hedera.Client {
	if !cfg.Providers.HederaIsLive() {
		return nil
	}

	client, err := hedera.Connect(hedera.Config{
		Network:    cfg.Providers.HederaNetwork,
		AccountID:  cfg.Providers.HederaAccountID,
		PrivateKey: cfg.Providers.HederaPrivateKey,
	})
	if err != nil {
		slog.Error("hedera credentials were rejected", slog.String("error", err.Error()))
		return nil
	}
	return client
}

// marketProvider picks the live gateway when one is configured, and the deterministic demo
// market set otherwise.
func marketProvider(cfg config.Config) marketdata.Provider {
	if !cfg.Providers.GraphIsLive() {
		return marketdata.NewStaticProvider(marketdata.DemoMarkets()...)
	}

	provider, err := marketdata.NewGraphProvider(marketdata.GraphConfig{
		APIKey:     cfg.Providers.GraphAPIKey,
		GatewayURL: cfg.Providers.GraphGatewayURL,
	})
	if err != nil {
		// A rejected key is a configuration mistake, not a reason to stop serving. What it
		// must not do is go unsaid: every price computed after this line comes from the
		// demo market set, and the snapshot behind it says so in its provider field.
		slog.Error("graph credentials were rejected; using the demo market set",
			slog.String("error", err.Error()))
		return marketdata.NewStaticProvider(marketdata.DemoMarkets()...)
	}

	slog.Info("pricing against live lending markets",
		slog.String("network", cfg.Providers.GraphNetwork),
		slog.String("asset", cfg.Providers.GraphAsset),
		slog.Any("subgraphs", provider.Subgraphs()))
	return provider
}

func marketQuery(cfg config.Config) marketdata.Query {
	if !cfg.Providers.GraphIsLive() {
		return marketdata.DemoQuery()
	}
	return marketdata.GraphQuery(cfg.Providers.GraphNetwork, cfg.Providers.GraphAsset)
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
// A wallet session is the real mechanism and works in every environment. In development a
// header may also name an organization, so the API can be driven by curl and by the seeded
// demo without a browser wallet; outside development that fallback does not exist, so an
// unauthenticated request is simply unauthenticated.
func resolver(cfg config.Config, identityService *identity.Service, organizations organization.Repository, db *postgres.DB) httpserver.Resolver {
	sessions := identity.NewResolver(identityService, identity.SessionCookie)
	if !cfg.DemoAuthEnabled() {
		return sessions
	}

	slog.Warn("development demo authentication is enabled: the X-Demo-Organization header also names a caller")
	return httpserver.ResolverFunc(func(r *http.Request) (httpserver.Actor, error) {
		if actor, err := sessions.Resolve(r); err == nil && !actor.IsZero() {
			return actor, nil
		}

		raw := r.Header.Get("X-Demo-Organization")
		if raw == "" {
			return httpserver.Actor{}, nil
		}
		orgID, err := uuid.Parse(raw)
		if err != nil {
			return httpserver.Actor{}, err
		}

		// Only the identity comes from the header. Everything a rule depends on is read
		// from the stored organization, so the shortcut cannot grant eligibility the
		// record does not have.
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
