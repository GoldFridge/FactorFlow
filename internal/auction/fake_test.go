package auction_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// The fakes below cover the layer above SQL: authorization, which writes happen together,
// and what the service does when the solver refuses. The repository itself is tested
// against real PostgreSQL, where its SQL and constraints actually live.

type fakeQuerier struct{}

func (fakeQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeQuerier: Query is not used by the service layer")
}
func (fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("fakeQuerier: QueryRow is not used by the service layer")
}

// fakeDB gives the in-memory repository transaction semantics, so a test can check that a
// failed unit of work leaves nothing behind.
type fakeDB struct {
	repo       *memRepository
	failCommit error
}

func (f *fakeDB) InTx(_ context.Context, fn func(postgres.Querier) error) error {
	f.repo.begin()

	err := fn(fakeQuerier{})
	if err == nil {
		err = f.failCommit
	}
	if err != nil {
		f.repo.rollback()
		return err
	}
	f.repo.commit()
	return nil
}

func (f *fakeDB) Querier() postgres.Querier { return fakeQuerier{} }

// memRepository is an in-memory auction repository with the same version semantics as the
// PostgreSQL one.
type memRepository struct {
	mu        sync.Mutex
	auctions  map[uuid.UUID]auction.Auction
	bids      map[uuid.UUID]auction.Bid
	solutions map[uuid.UUID]auction.Solution

	snapshot *memSnapshot
}

type memSnapshot struct {
	auctions  map[uuid.UUID]auction.Auction
	bids      map[uuid.UUID]auction.Bid
	solutions map[uuid.UUID]auction.Solution
}

func newMemRepository() *memRepository {
	return &memRepository{
		auctions:  map[uuid.UUID]auction.Auction{},
		bids:      map[uuid.UUID]auction.Bid{},
		solutions: map[uuid.UUID]auction.Solution{},
	}
}

func (m *memRepository) begin() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.snapshot = &memSnapshot{
		auctions:  copyMap(m.auctions),
		bids:      copyMap(m.bids),
		solutions: copyMap(m.solutions),
	}
}

func (m *memRepository) rollback() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.snapshot != nil {
		m.auctions, m.bids, m.solutions = m.snapshot.auctions, m.snapshot.bids, m.snapshot.solutions
	}
	m.snapshot = nil
}

func (m *memRepository) commit() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.snapshot = nil
}

func (m *memRepository) CreateAuction(_ context.Context, _ postgres.Querier, a *auction.Auction) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.auctions[a.ID]; exists {
		return apperr.Conflictf("auction %s already exists", a.ID)
	}
	m.auctions[a.ID] = *a
	return nil
}

func (m *memRepository) GetAuction(_ context.Context, _ postgres.Querier, id uuid.UUID) (*auction.Auction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.auctions[id]
	if !ok {
		return nil, apperr.NotFoundf("auction %s", id)
	}
	copied := stored
	copied.Lots = append([]auction.Lot(nil), stored.Lots...)
	return &copied, nil
}

func (m *memRepository) UpdateAuction(_ context.Context, _ postgres.Querier, a *auction.Auction, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.auctions[a.ID]
	if !ok {
		return apperr.NotFoundf("auction %s", a.ID)
	}
	if stored.Version != expected {
		return apperr.Conflictf("auction %s was modified by another writer", a.ID)
	}
	m.auctions[a.ID] = *a
	return nil
}

func (m *memRepository) ListAuctions(_ context.Context, _ postgres.Querier, status auction.Status, limit int) ([]*auction.Auction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []*auction.Auction
	for _, stored := range m.auctions {
		if status != "" && stored.Status != status {
			continue
		}
		copied := stored
		out = append(out, &copied)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memRepository) CreateBid(_ context.Context, _ postgres.Querier, b *auction.Bid) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.bids[b.ID] = *b
	return nil
}

func (m *memRepository) GetBid(_ context.Context, _ postgres.Querier, id uuid.UUID) (*auction.Bid, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.bids[id]
	if !ok {
		return nil, apperr.NotFoundf("bid %s", id)
	}
	copied := stored
	return &copied, nil
}

func (m *memRepository) UpdateBid(_ context.Context, _ postgres.Querier, b *auction.Bid, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.bids[b.ID]
	if !ok {
		return apperr.NotFoundf("bid %s", b.ID)
	}
	if stored.Version != expected {
		return apperr.Conflictf("bid %s was modified by another writer", b.ID)
	}
	m.bids[b.ID] = *b
	return nil
}

func (m *memRepository) ListBids(_ context.Context, _ postgres.Querier, auctionID uuid.UUID) ([]*auction.Bid, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []*auction.Bid
	for _, stored := range m.bids {
		if stored.AuctionID != auctionID {
			continue
		}
		copied := stored
		out = append(out, &copied)
	}
	// The real repository returns the solver's canonical order; the fake must too, or the
	// service would look deterministic only because a map happened to iterate one way.
	sortBids(out)
	return out, nil
}

func (m *memRepository) SaveSolution(_ context.Context, _ postgres.Querier, s *auction.Solution, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.solutions[s.AuctionID]; exists {
		return apperr.Conflictf("auction %s is already cleared", s.AuctionID)
	}
	m.solutions[s.AuctionID] = *s
	return nil
}

func (m *memRepository) GetSolution(_ context.Context, _ postgres.Querier, auctionID uuid.UUID) (*auction.Solution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.solutions[auctionID]
	if !ok {
		return nil, apperr.NotFoundf("clearing for auction %s", auctionID)
	}
	copied := stored
	return &copied, nil
}

func sortBids(bids []*auction.Bid) {
	for i := 1; i < len(bids); i++ {
		for j := i; j > 0; j-- {
			left, right := bids[j-1], bids[j]
			if left.CreatedAt.Before(right.CreatedAt) {
				break
			}
			if left.CreatedAt.Equal(right.CreatedAt) && left.ID.String() < right.ID.String() {
				break
			}
			bids[j-1], bids[j] = right, left
		}
	}
}

func copyMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// compile-time check that the fake satisfies the port the service depends on.
var _ auction.Repository = (*memRepository)(nil)
