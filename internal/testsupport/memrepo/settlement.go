package memrepo

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

// Settlements returns the in-memory settlement repository.
func (s *Store) Settlements() settlement.Repository { return (*settlementRepo)(s) }

// Settlement reads a stored settlement back.
func (s *Store) Settlement(id uuid.UUID) (settlement.Settlement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.settlements[id]
	return stored, ok
}

// SettlementCount reports how many transfers were planned, which is how a test proves a
// repeated settle command did not plan a second set.
func (s *Store) SettlementCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.settlements)
}

type settlementRepo Store

func (r *settlementRepo) store() *Store { return (*Store)(r) }

// Create refuses a second plan for the same allocation or operation, the way the unique
// indexes do. Without that the fake would let a test pass that the database would reject.
func (r *settlementRepo) Create(_ context.Context, _ postgres.Querier, s *settlement.Settlement) error {
	store := r.store()
	store.mu.Lock()
	defer store.mu.Unlock()

	if _, exists := store.settlements[s.ID]; exists {
		return apperr.Conflictf("settlement %s already exists", s.ID)
	}
	for _, stored := range store.settlements {
		if stored.OperationID == s.OperationID {
			return apperr.Conflictf("settlement for operation %s already exists", s.OperationID)
		}
		if stored.AuctionID == s.AuctionID && stored.LotID == s.LotID && stored.BidID == s.BidID {
			return apperr.Conflictf("allocation %s/%s is already being settled", s.LotID, s.BidID)
		}
	}

	store.settlements[s.ID] = *s
	return nil
}

func (r *settlementRepo) Get(_ context.Context, _ postgres.Querier, id uuid.UUID) (*settlement.Settlement, error) {
	store := r.store()
	store.mu.Lock()
	defer store.mu.Unlock()

	stored, ok := store.settlements[id]
	if !ok {
		return nil, apperr.NotFoundf("settlement %s", id)
	}
	copied := stored
	return &copied, nil
}

func (r *settlementRepo) GetByOperation(_ context.Context, _ postgres.Querier, operationID string) (*settlement.Settlement, error) {
	store := r.store()
	store.mu.Lock()
	defer store.mu.Unlock()

	for _, stored := range store.settlements {
		if stored.OperationID == operationID {
			copied := stored
			return &copied, nil
		}
	}
	return nil, apperr.NotFoundf("settlement for operation %s", operationID)
}

func (r *settlementRepo) Update(_ context.Context, _ postgres.Querier, s *settlement.Settlement, expectedVersion int64) error {
	store := r.store()
	store.mu.Lock()
	defer store.mu.Unlock()

	stored, ok := store.settlements[s.ID]
	if !ok {
		return apperr.NotFoundf("settlement %s", s.ID)
	}
	if stored.Version != expectedVersion {
		return apperr.Conflictf("settlement %s was modified concurrently", s.ID)
	}

	store.settlements[s.ID] = *s
	return nil
}

func (r *settlementRepo) ListByAuction(_ context.Context, _ postgres.Querier, auctionID uuid.UUID) ([]*settlement.Settlement, error) {
	return r.list(func(s settlement.Settlement) bool { return s.AuctionID == auctionID }, 0)
}

func (r *settlementRepo) ListByInvoice(_ context.Context, _ postgres.Querier, invoiceID uuid.UUID) ([]*settlement.Settlement, error) {
	return r.list(func(s settlement.Settlement) bool { return s.InvoiceID == invoiceID }, 0)
}

func (r *settlementRepo) ListByInvestor(_ context.Context, _ postgres.Querier, investorID uuid.UUID, limit int) ([]*settlement.Settlement, error) {
	return r.list(func(s settlement.Settlement) bool { return s.InvestorID == investorID }, limit)
}

func (r *settlementRepo) ListUnfinished(_ context.Context, _ postgres.Querier, limit int) ([]*settlement.Settlement, error) {
	return r.list(func(s settlement.Settlement) bool { return !s.IsFinished() }, limit)
}

func (r *settlementRepo) list(keep func(settlement.Settlement) bool, limit int) ([]*settlement.Settlement, error) {
	store := r.store()
	store.mu.Lock()
	defer store.mu.Unlock()

	out := make([]*settlement.Settlement, 0, len(store.settlements))
	for _, stored := range store.settlements {
		if !keep(stored) {
			continue
		}
		copied := stored
		out = append(out, &copied)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
