package memrepo

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Organizations returns the in-memory organization repository.
func (s *Store) Organizations() organization.Repository { return (*organizationRepo)(s) }

// Identity returns the in-memory challenge and session repository.
func (s *Store) Identity() identity.Repository { return (*identityRepo)(s) }

// PutOrganization seeds an organization.
func (s *Store) PutOrganization(org *organization.Organization) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.organizations[org.ID] = *org
}

// Organization reads a stored organization back.
func (s *Store) Organization(id uuid.UUID) (organization.Organization, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.organizations[id]
	return stored, ok
}

// OrganizationCount reports how many organizations exist, which is how a test proves a
// failed registration left nothing behind.
func (s *Store) OrganizationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.organizations)
}

type organizationRepo Store

func (r *organizationRepo) store() *Store { return (*Store)(r) }

func (r *organizationRepo) Create(_ context.Context, _ postgres.Querier, org *organization.Organization) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.organizations[org.ID]; exists {
		return apperr.Conflictf("organization %s already exists", org.ID)
	}
	// The wallet is unique in the schema: one address is one participant, or a second
	// registration could take over an existing organization's identity.
	for _, stored := range s.organizations {
		if stored.Wallet == org.Wallet {
			return apperr.Conflictf("wallet %s already belongs to an organization", org.Wallet)
		}
	}

	s.organizations[org.ID] = *org
	return nil
}

func (r *organizationRepo) Get(_ context.Context, _ postgres.Querier, id uuid.UUID) (*organization.Organization, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.organizations[id]
	if !ok {
		return nil, apperr.NotFoundf("organization %s", id)
	}
	copied := stored
	return &copied, nil
}

func (r *organizationRepo) GetByWallet(_ context.Context, _ postgres.Querier, wallet string) (*organization.Organization, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, stored := range s.organizations {
		if stored.Wallet == wallet {
			copied := stored
			return &copied, nil
		}
	}
	return nil, apperr.NotFoundf("organization for wallet %s", wallet)
}

func (r *organizationRepo) Update(_ context.Context, _ postgres.Querier, org *organization.Organization, expectedVersion int64) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.organizations[org.ID]
	if !ok {
		return apperr.NotFoundf("organization %s", org.ID)
	}
	if stored.Version != expectedVersion {
		return apperr.Conflictf("organization %s was modified concurrently", org.ID)
	}

	s.organizations[org.ID] = *org
	return nil
}

func (r *organizationRepo) List(_ context.Context, _ postgres.Querier, orgType organization.Type, limit int) ([]*organization.Organization, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*organization.Organization, 0, len(s.organizations))
	for _, stored := range s.organizations {
		if orgType != "" && stored.Type != orgType {
			continue
		}
		copied := stored
		out = append(out, &copied)
	}

	// Newest first, matching the SQL ordering a caller pages through.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type identityRepo Store

func (r *identityRepo) store() *Store { return (*Store)(r) }

func (r *identityRepo) SaveChallenge(_ context.Context, _ postgres.Querier, c *identity.Challenge) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.challenges[c.Nonce] = *c
	return nil
}

func (r *identityRepo) GetChallenge(_ context.Context, _ postgres.Querier, nonce string) (*identity.Challenge, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.challenges[nonce]
	if !ok {
		return nil, apperr.NotFoundf("login challenge")
	}
	copied := stored
	return &copied, nil
}

// ConsumeChallenge spends a nonce, and refuses to spend one twice: that one-shot rule is
// the whole of replay protection, so the fake has to enforce it as the SQL does.
func (r *identityRepo) ConsumeChallenge(_ context.Context, _ postgres.Querier, c *identity.Challenge) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.challenges[c.Nonce]
	if !ok {
		return apperr.NotFoundf("login challenge")
	}
	if stored.IsConsumed() {
		return apperr.Conflictf("challenge was already used")
	}

	s.challenges[c.Nonce] = *c
	return nil
}

func (r *identityRepo) DeleteExpiredChallenges(_ context.Context, _ postgres.Querier, before time.Time) (int64, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	var removed int64
	for nonce, stored := range s.challenges {
		if stored.ExpiresAt.Before(before) {
			delete(s.challenges, nonce)
			removed++
		}
	}
	return removed, nil
}

func (r *identityRepo) SaveSession(_ context.Context, _ postgres.Querier, session *identity.Session) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions[session.TokenHash] = *session
	return nil
}

func (r *identityRepo) GetSession(_ context.Context, _ postgres.Querier, tokenHash string) (*identity.Session, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.sessions[tokenHash]
	if !ok {
		return nil, apperr.NotFoundf("session")
	}
	copied := stored
	return &copied, nil
}

func (r *identityRepo) RevokeSession(_ context.Context, _ postgres.Querier, session *identity.Session) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions[session.TokenHash] = *session
	return nil
}

// Audit returns the in-memory audit recorder.
//
// It is part of the store rather than a separate fake so audit rows obey the same
// transaction semantics as everything else: a failed unit of work discards its timeline
// entries too, which is the property the real recorder exists to provide.
func (s *Store) Audit() *AuditRecorder { return (*AuditRecorder)(s) }

// AuditRecorder records into the store.
type AuditRecorder Store

func (r *AuditRecorder) store() *Store { return (*Store)(r) }

// Record appends an event, validating it the way the SQL recorder does.
func (r *AuditRecorder) Record(_ context.Context, _ postgres.Querier, e audit.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}

	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.events = append(s.events, e)
	return nil
}

// Timeline returns one entity's events, newest first.
func (r *AuditRecorder) Timeline(_ context.Context, _ postgres.Querier, entityType, entityID string, limit int) ([]audit.Event, error) {
	events := r.For(entityType, entityID)
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// For returns one entity's events, newest first.
func (r *AuditRecorder) For(entityType, entityID string) []audit.Event {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]audit.Event, 0, len(s.events))
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].EntityType == entityType && s.events[i].EntityID == entityID {
			out = append(out, s.events[i])
		}
	}
	return out
}

// Actions returns every recorded action in the order it was recorded.
func (r *AuditRecorder) Actions() []string {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Action)
	}
	return out
}

// Events returns everything recorded, in order.
func (r *AuditRecorder) Events() []audit.Event {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]audit.Event, len(s.events))
	copy(out, s.events)
	return out
}
