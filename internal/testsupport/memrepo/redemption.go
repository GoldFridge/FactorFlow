package memrepo

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/redemption"
)

// Repayments returns the in-memory repayment repository.
func (s *Store) Repayments() redemption.Repository { return (*repaymentRepo)(s) }

// Repayment reads a stored repayment back.
func (s *Store) Repayment(invoiceID uuid.UUID) (redemption.Repayment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.repayments[invoiceID]
	return stored, ok
}

type repaymentRepo Store

func (r *repaymentRepo) store() *Store { return (*Store)(r) }

// Create refuses a second payment against the same receivable, the way the unique index
// does. Without that the fake would let a test pass that the database would reject — and
// this is the constraint that stops every holder being credited twice.
func (r *repaymentRepo) Create(_ context.Context, _ postgres.Querier, rep *redemption.Repayment) error {
	store := r.store()
	store.mu.Lock()
	defer store.mu.Unlock()

	if _, exists := store.repayments[rep.InvoiceID]; exists {
		return apperr.Conflictf("invoice %s has already been repaid", rep.InvoiceID)
	}

	copied := *rep
	copied.Shares = append([]redemption.Share(nil), rep.Shares...)
	store.repayments[rep.InvoiceID] = copied
	return nil
}

func (r *repaymentRepo) GetByInvoice(_ context.Context, _ postgres.Querier, invoiceID uuid.UUID) (*redemption.Repayment, error) {
	store := r.store()
	store.mu.Lock()
	defer store.mu.Unlock()

	stored, ok := store.repayments[invoiceID]
	if !ok {
		return nil, apperr.NotFoundf("repayment of invoice %s", invoiceID)
	}
	copied := stored
	copied.Shares = append([]redemption.Share(nil), stored.Shares...)
	return &copied, nil
}

func (r *repaymentRepo) ListForParty(_ context.Context, _ postgres.Querier, partyID uuid.UUID, limit int) ([]*redemption.Repayment, error) {
	store := r.store()
	store.mu.Lock()
	defer store.mu.Unlock()

	var out []*redemption.Repayment
	for _, stored := range store.repayments {
		if !stored.Holds(partyID) {
			continue
		}
		copied := stored
		copied.Shares = append([]redemption.Share(nil), stored.Shares...)
		out = append(out, &copied)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ReceivedAt.After(out[j].ReceivedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
