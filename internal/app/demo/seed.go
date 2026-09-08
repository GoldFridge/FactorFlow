// Package demo builds the dataset a demonstration starts from.
//
// It drives the real services rather than writing rows: every seeded invoice went through
// the same state machine, the same confidential workflow, the same solver and the same
// settlement saga a live one would. A seed that inserted finished records directly would
// be a fixture of what the platform is supposed to produce, and would keep working after
// the code that produces it broke.
package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/app/marketplace"
	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/clock"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// TxRunner is the transaction boundary the seed needs.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// Seeder builds the demo dataset.
type Seeder struct {
	db            TxRunner
	organizations organization.Repository
	invoices      *invoice.Service
	marketplace   *marketplace.Service
	auctions      *auction.Service
	dispatcher    *outbox.Dispatcher
	clock         *clock.Clock
}

// SeedHistory is how far back the seeded story starts.
//
// Everything a demo shows as finished happened during this week: the receivable was
// uploaded, priced, minted, sold and paid, each step under the rules that apply to it. The
// clock is then returned to the present for the one batch left open, so a visitor bidding
// into it is bidding into a window that has not closed.
const SeedHistory = 7 * 24 * time.Hour

// Config wires the seeder to the same services the server runs.
type Config struct {
	DB            TxRunner
	Organizations organization.Repository
	Invoices      *invoice.Service
	Marketplace   *marketplace.Service
	Auctions      *auction.Service
	// Dispatcher delivers the outbox between steps, so the seeded data passes through the
	// assessment and issuance workers exactly as a real upload would.
	Dispatcher *outbox.Dispatcher
	// Clock must be the same one the services above were wired with, and it must be fixed:
	// the seed moves it to place events in the past, and services reading a different clock
	// would refuse those events as happening at the wrong time.
	Clock *clock.Clock
}

// NewSeeder returns the seeder.
func NewSeeder(cfg Config) *Seeder {
	if cfg.Clock == nil {
		cfg.Clock = clock.At(time.Now().Add(-SeedHistory))
	}
	return &Seeder{
		db:            cfg.DB,
		organizations: cfg.Organizations,
		invoices:      cfg.Invoices,
		marketplace:   cfg.Marketplace,
		auctions:      cfg.Auctions,
		dispatcher:    cfg.Dispatcher,
		clock:         cfg.Clock,
	}
}

// now is the instant the seeded services are currently living at.
func (s *Seeder) now() time.Time { return s.clock.Now() }

// Participants are the organizations a demo signs in as.
//
// Their ids and wallets are fixed rather than generated: a demo script, a seeded frontend
// and a judge's bookmarks all refer to them, and a value that changes on every seed makes
// every one of those wrong.
var (
	OperatorID  = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	IssuerAID   = uuid.MustParse("00000000-0000-4000-8000-000000000002")
	IssuerBID   = uuid.MustParse("00000000-0000-4000-8000-000000000003")
	InvestorAID = uuid.MustParse("00000000-0000-4000-8000-000000000004")
	InvestorBID = uuid.MustParse("00000000-0000-4000-8000-000000000005")
)

// idNamespace anchors the seeded identifiers. It is an arbitrary constant; what matters is
// that it never changes, so a reseeded demo is byte-for-byte the one that was rehearsed.
var idNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

// IDs returns the identifier source the seed path wires into the services.
//
// Seeded identifiers are derived rather than random for a reason beyond tidiness: the
// confidential workflow derives an invoice's features from its identifier, so random ids
// would give every seed a different risk grade — and a demo whose auction clears only on
// some runs is worse than no demo. Deriving them makes the whole dataset, prices included,
// the same one every time.
func IDs() func() uuid.UUID {
	var (
		mu sync.Mutex
		n  int
	)
	return func() uuid.UUID {
		mu.Lock()
		defer mu.Unlock()

		n++
		return uuid.NewSHA1(idNamespace, []byte(strconv.Itoa(n)))
	}
}

// participant is one seeded organization.
type participant struct {
	id     uuid.UUID
	kind   organization.Type
	name   string
	wallet string
}

func participants() []participant {
	return []participant{
		{OperatorID, organization.TypeOperator, "FactorFlow Operations", "0x0000000000000000000000000000000000000f01"},
		{IssuerAID, organization.TypeIssuer, "Northwind Trading GmbH", "0x0000000000000000000000000000000000000a01"},
		{IssuerBID, organization.TypeIssuer, "Baltic Freight OÜ", "0x0000000000000000000000000000000000000a02"},
		{InvestorAID, organization.TypeInvestor, "Alpine Treasury AG", "0x0000000000000000000000000000000000000b01"},
		{InvestorBID, organization.TypeInvestor, "Meridian Credit Fund", "0x0000000000000000000000000000000000000b02"},
	}
}

// Summary reports what a seed produced, for the operator that ran it.
type Summary struct {
	Organizations int
	Invoices      int
	Auctions      int
	Bids          int
	Settlements   int
	// Skipped reports that the data was already there and nothing was created.
	Skipped bool
}

// Run builds the dataset, or reports that it already exists.
//
// Seeding twice is a no-op rather than an error: a demo server that restarts must not fail
// to start, and must not end up with two of everything.
func (s *Seeder) Run(ctx context.Context) (Summary, error) {
	existing, err := s.organizations.Get(ctx, s.db.Querier(), OperatorID)
	if err != nil && !apperr.IsNotFound(err) {
		return Summary{}, err
	}
	if existing != nil {
		return Summary{Skipped: true}, nil
	}

	if !s.clock.IsFixed() {
		// Without a clock it can move, the seed cannot produce a finished batch: clearing
		// refuses to run before the bidding window ends, and waiting out a real window is
		// not something a demo can do.
		return Summary{}, apperr.Invalid("clock", "the seed needs a fixed clock it can advance")
	}
	// The story starts a week ago and catches up to the present, so the finished batch is
	// genuinely older than the open one rather than sharing a timestamp with it.
	s.clock.Set(time.Now().Add(-SeedHistory))

	summary := Summary{}
	if err := s.seedParticipants(ctx); err != nil {
		return summary, fmt.Errorf("seeding participants: %w", err)
	}
	summary.Organizations = len(participants())

	// A receivable at every stage a screen has to render: one still a draft, one waiting
	// for its issuer to approve the extracted facts, one minted and ready to list, and one
	// that has been through an auction.
	if _, err := s.draft(ctx, IssuerAID, "INV-2026-0001", "12500.00", 45); err != nil {
		return summary, fmt.Errorf("seeding the draft invoice: %w", err)
	}
	summary.Invoices++

	if _, err := s.assessed(ctx, IssuerAID, "INV-2026-0002", "8400.00", 30); err != nil {
		return summary, fmt.Errorf("seeding the assessed invoice: %w", err)
	}
	summary.Invoices++

	// One receivable carried all the way through clearing and settlement, so the finished
	// state is visible without waiting a day for a window to close.
	financed, err := s.tokenized(ctx, IssuerAID, "INV-2026-0004", "15000.00", 60)
	if err != nil {
		return summary, fmt.Errorf("seeding the financed invoice: %w", err)
	}
	summary.Invoices++

	settled, err := s.settledAuction(ctx, IssuerAID, financed.ID)
	if err != nil {
		return summary, fmt.Errorf("settling the demo auction: %w", err)
	}
	summary.Auctions++
	summary.Bids += settled.bids
	summary.Settlements += settled.settlements

	// History is over: the rest of the dataset is created now, so the open batch closes a
	// day from when the demo is actually being watched.
	s.clock.Set(time.Now())

	ready, err := s.tokenized(ctx, IssuerBID, "INV-2026-0003", "21000.00", 90)
	if err != nil {
		return summary, fmt.Errorf("seeding the tokenized invoice: %w", err)
	}
	summary.Invoices++

	// And one batch left open, so a judge can bid into it.
	open, err := s.openAuction(ctx, IssuerBID, ready.ID)
	if err != nil {
		return summary, fmt.Errorf("opening the demo auction: %w", err)
	}
	summary.Auctions++

	bids, err := s.bid(ctx, open)
	if err != nil {
		return summary, fmt.Errorf("bidding into the demo auction: %w", err)
	}
	summary.Bids += bids

	return summary, nil
}

func (s *Seeder) seedParticipants(ctx context.Context) error {
	return s.db.InTx(ctx, func(q postgres.Querier) error {
		for _, p := range participants() {
			org, err := organization.New(organization.NewParams{
				ID: p.id, Type: p.kind, Name: p.name, Wallet: p.wallet,
			}, s.now())
			if err != nil {
				return err
			}
			// Seeded participants are eligible: a demo cannot start with everyone waiting
			// for an approval nobody is there to give.
			if err := org.Approve(s.now()); err != nil {
				return err
			}
			if err := s.organizations.Create(ctx, q, org); err != nil {
				return err
			}
		}
		return nil
	})
}

// draft creates an invoice and stops there.
func (s *Seeder) draft(ctx context.Context, issuerID uuid.UUID, number, face string, tenorDays int) (*invoice.Invoice, error) {
	amount, err := money.Parse(face, money.USD)
	if err != nil {
		return nil, err
	}

	issued := s.now().Add(-7 * 24 * time.Hour)
	return s.invoices.Create(ctx, invoice.Actor{OrganizationID: issuerID}, invoice.CreateParams{
		DebtorRef: debtorFor(number),
		Number:    number,
		Face:      amount,
		IssuedAt:  issued,
		DueAt:     issued.Add(time.Duration(tenorDays) * 24 * time.Hour),
	})
}

// assessed carries an invoice as far as a price: uploaded, then scored by the real worker.
func (s *Seeder) assessed(ctx context.Context, issuerID uuid.UUID, number, face string, tenorDays int) (*invoice.Invoice, error) {
	inv, err := s.draft(ctx, issuerID, number, face, tenorDays)
	if err != nil {
		return nil, err
	}

	actor := invoice.Actor{OrganizationID: issuerID}
	if _, err := s.invoices.AttachDocument(ctx, actor, inv.ID, documentFor(inv)); err != nil {
		return nil, err
	}
	if _, err := s.invoices.RequestAssessment(ctx, actor, inv.ID, "seed"); err != nil {
		return nil, err
	}
	if err := s.drain(ctx); err != nil {
		return nil, err
	}
	return s.invoices.Get(ctx, actor, inv.ID)
}

// tokenized carries an invoice to a minted asset, ready to be auctioned.
func (s *Seeder) tokenized(ctx context.Context, issuerID uuid.UUID, number, face string, tenorDays int) (*invoice.Invoice, error) {
	inv, err := s.assessed(ctx, issuerID, number, face, tenorDays)
	if err != nil {
		return nil, err
	}

	actor := invoice.Actor{OrganizationID: issuerID}
	if _, err := s.invoices.Approve(ctx, actor, inv.ID); err != nil {
		return nil, err
	}
	if _, err := s.invoices.RequestTokenization(ctx, actor, inv.ID, "seed"); err != nil {
		return nil, err
	}
	if err := s.drain(ctx); err != nil {
		return nil, err
	}
	return s.invoices.Get(ctx, actor, inv.ID)
}

func (s *Seeder) openAuction(ctx context.Context, issuerID uuid.UUID, invoiceIDs ...uuid.UUID) (*auction.Auction, error) {
	return s.marketplace.OpenAuction(ctx, marketplace.Actor{OrganizationID: issuerID}, marketplace.OpenParams{
		InvoiceIDs: invoiceIDs,
		OpensAt:    s.now(),
		ClosesAt:   s.now().Add(24 * time.Hour),
	})
}

// bid places two constrained bids, one of which is deliberately too demanding: an auction
// where everything clears shows nothing about why anything cleared.
func (s *Seeder) bid(ctx context.Context, a *auction.Auction) (int, error) {
	mandates := []struct {
		investor uuid.UUID
		minYield string
		maxGrade string
		budget   string
	}{
		// The first mandate is deliberately broad, so the batch demonstrates clearing
		// rather than the seed's luck with a grade. The second is too demanding to fill,
		// which is what makes the rejection reason worth showing.
		{InvestorAID, "0.06", "E", "40000.00"},
		{InvestorBID, "0.45", "A", "25000.00"},
	}

	placed := 0
	for _, mandate := range mandates {
		budget, err := money.Parse(mandate.budget, money.USD)
		if err != nil {
			return placed, err
		}
		minYield, err := money.ParseRate(mandate.minYield)
		if err != nil {
			return placed, err
		}
		maxGrade, err := parseGrade(mandate.maxGrade)
		if err != nil {
			return placed, err
		}

		if _, err := s.auctions.PlaceBid(ctx,
			auction.Actor{OrganizationID: mandate.investor, Eligible: true}, a.ID, auction.BidParams{
				Budget:       budget,
				MinYield:     minYield,
				MaxGrade:     maxGrade,
				MaxTenorDays: 120,
				MinimumLot:   money.Zero(money.USD),
			}); err != nil {
			return placed, err
		}
		placed++
	}
	return placed, nil
}

type settledResult struct {
	bids        int
	settlements int
}

// settledAuction runs one batch all the way to settled transfers.
func (s *Seeder) settledAuction(ctx context.Context, issuerID uuid.UUID, invoiceID uuid.UUID) (settledResult, error) {
	var result settledResult

	a, err := s.openAuction(ctx, issuerID, invoiceID)
	if err != nil {
		return result, err
	}

	bids, err := s.bid(ctx, a)
	result.bids = bids
	if err != nil {
		return result, err
	}

	// The batch is cleared the way a real one is: after its bidding window has ended. The
	// clock moves rather than the rule bending, so this auction was closed to new bids at
	// the moment it cleared, exactly like a live one.
	s.clock.Advance(a.ClosesAt.Sub(s.now()) + time.Hour)

	actor := marketplace.Actor{OrganizationID: issuerID}
	if _, err := s.marketplace.ClearAuction(ctx, actor, a.ID); err != nil {
		return result, err
	}

	planned, err := s.marketplace.SettleAuction(ctx, actor, a.ID, "seed")
	if err != nil {
		return result, err
	}
	result.settlements = len(planned)

	// The transfers run through the same outbox the server drives, so what a demo shows is
	// the saga's own output rather than a shortcut.
	if err := s.drain(ctx); err != nil {
		return result, err
	}
	return result, nil
}

// drain delivers the outbox until it is empty.
//
// The bound is a safety net rather than a limit anyone expects to reach: a handler that
// keeps re-queueing work would otherwise seed forever.
func (s *Seeder) drain(ctx context.Context) error {
	const maxRounds = 50

	for round := 0; round < maxRounds; round++ {
		result, err := s.dispatcher.RunOnce(ctx)
		if err != nil {
			return err
		}
		if result.Claimed == 0 {
			return nil
		}
		if result.Failed > 0 || result.Parked > 0 {
			slog.Warn("seed: an outbox event did not deliver",
				slog.Int("failed", result.Failed), slog.Int("parked", result.Parked))
		}
	}
	return fmt.Errorf("the outbox did not drain after %d rounds", maxRounds)
}

// documentFor is the encrypted-upload metadata a browser would have produced. The demo has
// no document behind it, which is the point: the platform never sees one.
func documentFor(inv *invoice.Invoice) invoice.NewDocumentParams {
	return invoice.NewDocumentParams{
		ObjectKey:  fmt.Sprintf("invoices/demo/%s.enc", inv.ID),
		CipherHash: cipherHashFor(inv.ID),
		KeyRef:     "cre-secret://data-key/demo",
		MIME:       "application/pdf",
		SizeBytes:  184_320,
	}
}

func debtorFor(number string) string {
	switch number {
	case "INV-2026-0001", "INV-2026-0004":
		return "ACME Logistics GmbH"
	case "INV-2026-0002":
		return "Helios Manufacturing SpA"
	default:
		return "Nordic Retail Group AS"
	}
}

// cipherHashFor derives the ciphertext digest a real upload would have carried. It is
// deterministic so a reseeded demo produces the same audit trail.
func cipherHashFor(invoiceID uuid.UUID) string {
	sum := sha256.Sum256([]byte("factorflow.demo.document|" + invoiceID.String()))
	return hex.EncodeToString(sum[:])
}

// parseGrade is the risk module's own parser, reached through the auction module's
// vocabulary so this package does not have to decide what a grade means.
func parseGrade(s string) (risk.Grade, error) { return risk.ParseGrade(s) }
