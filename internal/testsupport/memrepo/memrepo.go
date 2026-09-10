// Package memrepo holds in-memory implementations of the repository ports, for testing the
// layer above storage.
//
// It is not a replacement for the repository tests: those run against real PostgreSQL,
// because SQL, constraints and transactions are where a repository's behaviour actually
// lives. What these are for is the application layer, where the rules under test are
// authorization, composition and which writes happen together, and where a database would
// only make the test slower and the failure harder to read.
//
// The store gives transactions real semantics: writes made inside a failed unit of work are
// discarded, so a test can prove that nothing was left behind.
package memrepo

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/objects"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/redemption"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/settlement"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

// Querier satisfies the repository signatures without touching a database. Statements are
// recorded so a test can see that, say, an outbox event was published in the same unit of
// work.
type Querier struct {
	mu    sync.Mutex
	execs []string
}

// Exec records a statement.
func (q *Querier) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.execs = append(q.execs, sql)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

// Query is not used by the application layer.
func (q *Querier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("memrepo: Query is not supported")
}

// QueryRow is not used by the application layer.
func (q *Querier) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("memrepo: QueryRow is not supported")
}

// Executed reports whether a statement mentioning table was run.
func (q *Querier) Executed(table string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, sql := range q.execs {
		if contains(sql, table) {
			return true
		}
	}
	return false
}

// Store holds every in-memory repository and gives them shared transaction semantics.
type Store struct {
	mu sync.Mutex

	organizations map[uuid.UUID]organization.Organization
	challenges    map[string]identity.Challenge
	sessions      map[string]identity.Session
	invoices      map[uuid.UUID]invoice.Invoice
	documents     map[uuid.UUID]invoice.Document
	assessments   map[uuid.UUID]risk.Assessment
	explanations  map[uuid.UUID]risk.Explanation
	snapshots     map[string]marketdata.Snapshot
	assets        map[uuid.UUID]tokenization.Asset
	auctions      map[uuid.UUID]auction.Auction
	bids          map[uuid.UUID]auction.Bid
	solutions     map[uuid.UUID]auction.Solution
	settlements   map[uuid.UUID]settlement.Settlement
	repayments    map[uuid.UUID]redemption.Repayment
	blobs         map[string]objects.Object
	events        []audit.Event

	snapshot *state
	querier  *Querier

	// FailCommit makes the next transaction fail after its body ran, standing in for a
	// commit that never lands.
	FailCommit error
}

type state struct {
	organizations map[uuid.UUID]organization.Organization
	challenges    map[string]identity.Challenge
	sessions      map[string]identity.Session

	invoices     map[uuid.UUID]invoice.Invoice
	documents    map[uuid.UUID]invoice.Document
	assessments  map[uuid.UUID]risk.Assessment
	explanations map[uuid.UUID]risk.Explanation
	snapshots    map[string]marketdata.Snapshot
	assets       map[uuid.UUID]tokenization.Asset
	auctions     map[uuid.UUID]auction.Auction
	bids         map[uuid.UUID]auction.Bid
	solutions    map[uuid.UUID]auction.Solution
	settlements  map[uuid.UUID]settlement.Settlement
	repayments   map[uuid.UUID]redemption.Repayment
	blobs        map[string]objects.Object
	events       []audit.Event
}

// New returns an empty store.
func New() *Store {
	return &Store{
		organizations: map[uuid.UUID]organization.Organization{},
		challenges:    map[string]identity.Challenge{},
		sessions:      map[string]identity.Session{},

		invoices:     map[uuid.UUID]invoice.Invoice{},
		documents:    map[uuid.UUID]invoice.Document{},
		assessments:  map[uuid.UUID]risk.Assessment{},
		explanations: map[uuid.UUID]risk.Explanation{},
		snapshots:    map[string]marketdata.Snapshot{},
		assets:       map[uuid.UUID]tokenization.Asset{},
		auctions:     map[uuid.UUID]auction.Auction{},
		bids:         map[uuid.UUID]auction.Bid{},
		solutions:    map[uuid.UUID]auction.Solution{},
		settlements:  map[uuid.UUID]settlement.Settlement{},
		repayments:   map[uuid.UUID]redemption.Repayment{},
		blobs:        map[string]objects.Object{},
		querier:      &Querier{},
	}
}

// InTx runs fn as a transaction, discarding its writes when it fails.
func (s *Store) InTx(_ context.Context, fn func(postgres.Querier) error) error {
	s.begin()

	err := fn(s.querier)
	if err == nil {
		err = s.FailCommit
	}
	if err != nil {
		s.rollback()
		return err
	}
	s.commit()
	return nil
}

// Querier returns the recording querier.
func (s *Store) Querier() postgres.Querier { return s.querier }

// Statements exposes the querier for assertions about what was executed directly.
func (s *Store) Statements() *Querier { return s.querier }

func (s *Store) begin() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshot = &state{
		organizations: copyMap(s.organizations),
		challenges:    copyMap(s.challenges),
		sessions:      copyMap(s.sessions),

		invoices:    copyMap(s.invoices),
		documents:   copyMap(s.documents),
		assessments: copyMap(s.assessments),
		snapshots:   copyMap(s.snapshots),
		assets:      copyMap(s.assets),
		auctions:    copyMap(s.auctions),
		bids:        copyMap(s.bids),
		solutions:   copyMap(s.solutions),
		settlements: copyMap(s.settlements),
		repayments:  copyMap(s.repayments),
		blobs:       copyMap(s.blobs),
		events:      append([]audit.Event(nil), s.events...),
	}
}

func (s *Store) rollback() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.snapshot == nil {
		return
	}
	s.organizations = s.snapshot.organizations
	s.challenges = s.snapshot.challenges
	s.sessions = s.snapshot.sessions
	s.invoices = s.snapshot.invoices
	s.documents = s.snapshot.documents
	s.assessments = s.snapshot.assessments
	s.explanations = s.snapshot.explanations
	s.snapshots = s.snapshot.snapshots
	s.assets = s.snapshot.assets
	s.auctions = s.snapshot.auctions
	s.bids = s.snapshot.bids
	s.solutions = s.snapshot.solutions
	s.settlements = s.snapshot.settlements
	s.repayments = s.snapshot.repayments
	s.blobs = s.snapshot.blobs
	s.events = s.snapshot.events
	s.snapshot = nil
}

func (s *Store) commit() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshot = nil
}

// Invoices returns the in-memory invoice repository.
func (s *Store) Invoices() invoice.Repository { return (*invoiceRepo)(s) }

// Assessments returns the in-memory assessment repository.
func (s *Store) Assessments() risk.Repository { return (*assessmentRepo)(s) }

// Snapshots returns the in-memory market snapshot repository.
func (s *Store) Snapshots() marketdata.Repository { return (*snapshotRepo)(s) }

// Assets returns the in-memory tokenized asset repository.
func (s *Store) Assets() tokenization.Repository { return (*assetRepo)(s) }

// Auctions returns the in-memory auction repository.
func (s *Store) Auctions() auction.Repository { return (*auctionRepo)(s) }

// PutInvoice seeds an invoice.
func (s *Store) PutInvoice(inv *invoice.Invoice) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.invoices[inv.ID] = *inv
}

// Invoice reads a seeded invoice back.
func (s *Store) Invoice(id uuid.UUID) (invoice.Invoice, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.invoices[id]
	return stored, ok
}

// PutAssessment seeds an assessment.
func (s *Store) PutAssessment(a *risk.Assessment) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.assessments[a.ID] = *a
}

// PutAsset seeds a tokenized asset.
func (s *Store) PutAsset(a *tokenization.Asset) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.assets[a.ID] = *a
}

// Asset reads a stored asset back.
func (s *Store) Asset(id uuid.UUID) (tokenization.Asset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.assets[id]
	return stored, ok
}

// AssetCount reports how many assets exist, which is how a test proves that a redelivered
// command did not mint a second one.
func (s *Store) AssetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.assets)
}

// Auction reads a stored auction back.
func (s *Store) Auction(id uuid.UUID) (auction.Auction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.auctions[id]
	return stored, ok
}

// AuctionCount reports how many auctions exist.
func (s *Store) AuctionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.auctions)
}

type invoiceRepo Store

func (r *invoiceRepo) store() *Store { return (*Store)(r) }

func (r *invoiceRepo) Create(_ context.Context, _ postgres.Querier, inv *invoice.Invoice) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.invoices[inv.ID]; exists {
		return apperr.Conflictf("invoice %s already exists", inv.ID)
	}
	// The database allows one live receivable per set of terms, across the venue. Without
	// the same rule here, an application test would pass a double-financing the database
	// refuses — which is the one place this rule must not be discovered late.
	for _, stored := range s.invoices {
		if stored.Status.IsTerminal() {
			continue
		}
		if stored.Fingerprint() == inv.Fingerprint() {
			return apperr.Conflictf("a receivable with these terms is already being financed")
		}
	}

	s.invoices[inv.ID] = *inv
	return nil
}

func (r *invoiceRepo) Get(_ context.Context, _ postgres.Querier, id uuid.UUID) (*invoice.Invoice, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.invoices[id]
	if !ok {
		return nil, apperr.NotFoundf("invoice %s", id)
	}
	copied := stored
	return &copied, nil
}

func (r *invoiceRepo) Update(_ context.Context, _ postgres.Querier, inv *invoice.Invoice, expected int64) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.invoices[inv.ID]
	if !ok {
		return apperr.NotFoundf("invoice %s", inv.ID)
	}
	if stored.Version != expected {
		return apperr.Conflictf("invoice %s was modified by another writer", inv.ID)
	}
	s.invoices[inv.ID] = *inv
	return nil
}

func (r *invoiceRepo) ListByIssuer(_ context.Context, _ postgres.Querier, issuerID uuid.UUID, limit int) ([]*invoice.Invoice, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*invoice.Invoice
	for _, stored := range s.invoices {
		if stored.IssuerID != issuerID {
			continue
		}
		copied := stored
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return truncate(out, limit), nil
}

func (r *invoiceRepo) ListByStatus(_ context.Context, _ postgres.Querier, status invoice.Status, limit int) ([]*invoice.Invoice, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*invoice.Invoice
	for _, stored := range s.invoices {
		if stored.Status != status {
			continue
		}
		copied := stored
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return truncate(out, limit), nil
}

func (r *invoiceRepo) SaveDocument(_ context.Context, _ postgres.Querier, doc *invoice.Document) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.documents[doc.InvoiceID] = *doc
	return nil
}

func (r *invoiceRepo) GetDocument(_ context.Context, _ postgres.Querier, invoiceID uuid.UUID) (*invoice.Document, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.documents[invoiceID]
	if !ok {
		return nil, apperr.NotFoundf("document for invoice %s", invoiceID)
	}
	copied := stored
	return &copied, nil
}

type assessmentRepo Store

func (r *assessmentRepo) store() *Store { return (*Store)(r) }

func (r *assessmentRepo) Save(_ context.Context, _ postgres.Querier, a *risk.Assessment) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.assessments[a.ID] = *a
	return nil
}

func (r *assessmentRepo) Get(_ context.Context, _ postgres.Querier, id uuid.UUID) (*risk.Assessment, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.assessments[id]
	if !ok {
		return nil, apperr.NotFoundf("risk assessment %s", id)
	}
	copied := stored
	return &copied, nil
}

func (r *assessmentRepo) SaveExplanation(_ context.Context, _ postgres.Querier, e *risk.Explanation) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := *e
	copied.Bullets = append([]string(nil), e.Bullets...)
	s.explanations[e.AssessmentID] = copied
	return nil
}

func (r *assessmentRepo) GetExplanation(_ context.Context, _ postgres.Querier, assessmentID uuid.UUID) (*risk.Explanation, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.explanations[assessmentID]
	if !ok {
		return nil, apperr.NotFoundf("explanation of assessment %s", assessmentID)
	}
	copied := stored
	copied.Bullets = append([]string(nil), stored.Bullets...)
	return &copied, nil
}

func (r *assessmentRepo) Latest(_ context.Context, _ postgres.Querier, invoiceID uuid.UUID) (*risk.Assessment, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var latest *risk.Assessment
	for _, stored := range s.assessments {
		if stored.InvoiceID != invoiceID {
			continue
		}
		copied := stored
		if latest == nil || copied.CreatedAt.After(latest.CreatedAt) {
			latest = &copied
		}
	}
	if latest == nil {
		return nil, apperr.NotFoundf("risk assessment for invoice %s", invoiceID)
	}
	return latest, nil
}

type snapshotRepo Store

func (r *snapshotRepo) store() *Store { return (*Store)(r) }

func (r *snapshotRepo) Save(_ context.Context, _ postgres.Querier, snapshot *marketdata.Snapshot) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshots[snapshot.PayloadHash] = *snapshot
	return nil
}

func (r *snapshotRepo) Get(_ context.Context, _ postgres.Querier, payloadHash string) (*marketdata.Snapshot, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.snapshots[payloadHash]
	if !ok {
		return nil, apperr.NotFoundf("market snapshot %s", payloadHash)
	}
	copied := stored
	return &copied, nil
}

func (r *snapshotRepo) Latest(_ context.Context, _ postgres.Querier, network, asset string) (*marketdata.Snapshot, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var latest *marketdata.Snapshot
	for _, stored := range s.snapshots {
		if stored.Network != network || stored.Asset != asset {
			continue
		}
		copied := stored
		if latest == nil || copied.ObservedAt.After(latest.ObservedAt) {
			latest = &copied
		}
	}
	if latest == nil {
		return nil, apperr.NotFoundf("market snapshot for %s/%s", network, asset)
	}
	return latest, nil
}

type assetRepo Store

func (r *assetRepo) store() *Store { return (*Store)(r) }

func (r *assetRepo) Create(_ context.Context, _ postgres.Querier, asset *tokenization.Asset) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, stored := range s.assets {
		if stored.InvoiceID == asset.InvoiceID {
			return apperr.Conflictf("invoice %s already has an asset", asset.InvoiceID)
		}
	}
	s.assets[asset.ID] = *asset
	return nil
}

func (r *assetRepo) Get(_ context.Context, _ postgres.Querier, id uuid.UUID) (*tokenization.Asset, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.assets[id]
	if !ok {
		return nil, apperr.NotFoundf("tokenized asset %s", id)
	}
	copied := stored
	return &copied, nil
}

func (r *assetRepo) GetByInvoice(_ context.Context, _ postgres.Querier, invoiceID uuid.UUID) (*tokenization.Asset, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, stored := range s.assets {
		if stored.InvoiceID == invoiceID {
			copied := stored
			return &copied, nil
		}
	}
	return nil, apperr.NotFoundf("tokenized asset for invoice %s", invoiceID)
}

func (r *assetRepo) Update(_ context.Context, _ postgres.Querier, asset *tokenization.Asset) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.assets[asset.ID]; !ok {
		return apperr.NotFoundf("tokenized asset %s", asset.ID)
	}
	s.assets[asset.ID] = *asset
	return nil
}

func (r *assetRepo) ListByIssuer(_ context.Context, _ postgres.Querier, issuerID uuid.UUID, limit int) ([]*tokenization.Asset, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*tokenization.Asset
	for _, stored := range s.assets {
		if stored.IssuerID != issuerID {
			continue
		}
		copied := stored
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return truncate(out, limit), nil
}

type auctionRepo Store

func (r *auctionRepo) store() *Store { return (*Store)(r) }

func (r *auctionRepo) CreateAuction(_ context.Context, _ postgres.Querier, a *auction.Auction) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.auctions[a.ID]; exists {
		return apperr.Conflictf("auction %s already exists", a.ID)
	}
	// The database keeps one asset in one auction; the fake enforces it too, or a test
	// would pass here and fail against PostgreSQL.
	for _, lot := range a.Lots {
		for _, stored := range s.auctions {
			for _, existing := range stored.Lots {
				if existing.AssetID == lot.AssetID {
					return apperr.Conflictf("asset %s is already in an auction", lot.AssetID)
				}
			}
		}
	}
	s.auctions[a.ID] = *a
	return nil
}

func (r *auctionRepo) GetAuction(_ context.Context, _ postgres.Querier, id uuid.UUID) (*auction.Auction, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.auctions[id]
	if !ok {
		return nil, apperr.NotFoundf("auction %s", id)
	}
	copied := stored
	copied.Lots = append([]auction.Lot(nil), stored.Lots...)
	return &copied, nil
}

func (r *auctionRepo) UpdateAuction(_ context.Context, _ postgres.Querier, a *auction.Auction, expected int64) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.auctions[a.ID]
	if !ok {
		return apperr.NotFoundf("auction %s", a.ID)
	}
	if stored.Version != expected {
		return apperr.Conflictf("auction %s was modified by another writer", a.ID)
	}
	s.auctions[a.ID] = *a
	return nil
}

func (r *auctionRepo) ListAuctions(_ context.Context, _ postgres.Querier, status auction.Status, limit int) ([]*auction.Auction, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*auction.Auction
	for _, stored := range s.auctions {
		if status != "" && stored.Status != status {
			continue
		}
		copied := stored
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClosesAt.Before(out[j].ClosesAt) })
	return truncate(out, limit), nil
}

func (r *auctionRepo) ListingOf(_ context.Context, _ postgres.Querier, invoiceID uuid.UUID) (auction.Listing, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var found []auction.Auction
	for _, stored := range s.auctions {
		for _, lot := range stored.Lots {
			if lot.InvoiceID == invoiceID {
				found = append(found, stored)
			}
		}
	}
	if len(found) == 0 {
		return auction.Listing{}, apperr.NotFoundf("listing of invoice %s", invoiceID)
	}

	// Newest first, matching the stored repository: a relisted receivable is described by
	// the batch it stands in now.
	sort.Slice(found, func(i, j int) bool { return found[i].CreatedAt.After(found[j].CreatedAt) })
	newest := found[0]

	listing := auction.Listing{AuctionID: newest.ID, Status: newest.Status}
	for _, lot := range newest.Lots {
		if lot.InvoiceID == invoiceID {
			listing.LotID = lot.ID
			break
		}
	}
	return listing, nil
}

func (r *auctionRepo) CreateBid(_ context.Context, _ postgres.Querier, b *auction.Bid) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.bids[b.ID] = *b
	return nil
}

func (r *auctionRepo) GetBid(_ context.Context, _ postgres.Querier, id uuid.UUID) (*auction.Bid, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.bids[id]
	if !ok {
		return nil, apperr.NotFoundf("bid %s", id)
	}
	copied := stored
	return &copied, nil
}

func (r *auctionRepo) UpdateBid(_ context.Context, _ postgres.Querier, b *auction.Bid, expected int64) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.bids[b.ID]
	if !ok {
		return apperr.NotFoundf("bid %s", b.ID)
	}
	if stored.Version != expected {
		return apperr.Conflictf("bid %s was modified by another writer", b.ID)
	}
	s.bids[b.ID] = *b
	return nil
}

func (r *auctionRepo) ListBids(_ context.Context, _ postgres.Querier, auctionID uuid.UUID) ([]*auction.Bid, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*auction.Bid
	for _, stored := range s.bids {
		if stored.AuctionID != auctionID {
			continue
		}
		copied := stored
		out = append(out, &copied)
	}
	// The canonical order the solver relies on: creation time, then id.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func (r *auctionRepo) SaveSolution(_ context.Context, _ postgres.Querier, sol *auction.Solution, _ time.Time) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.solutions[sol.AuctionID]; exists {
		return apperr.Conflictf("auction %s is already cleared", sol.AuctionID)
	}
	s.solutions[sol.AuctionID] = *sol
	return nil
}

func (r *auctionRepo) GetSolution(_ context.Context, _ postgres.Querier, auctionID uuid.UUID) (*auction.Solution, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.solutions[auctionID]
	if !ok {
		return nil, apperr.NotFoundf("clearing for auction %s", auctionID)
	}
	copied := stored
	return &copied, nil
}

func truncate[T any](items []T, limit int) []T {
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

func copyMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func contains(haystack, needle string) bool {
	return needle != "" && strings.Contains(haystack, needle)
}

// Compile-time checks that the fakes satisfy the ports they stand in for.
var (
	_ invoice.Repository      = (*invoiceRepo)(nil)
	_ risk.Repository         = (*assessmentRepo)(nil)
	_ marketdata.Repository   = (*snapshotRepo)(nil)
	_ tokenization.Repository = (*assetRepo)(nil)
	_ auction.Repository      = (*auctionRepo)(nil)
)
