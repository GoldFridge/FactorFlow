// Package onboarding registers an organization behind a wallet.
//
// It spans two modules on purpose. Registration has to prove that the caller controls the
// wallet before an organization is created for it, so it runs identity's challenge check and
// then the organization module's own rules; neither module may reach into the other.
package onboarding

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/identity"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// EntityType names organizations in the audit timeline.
const EntityType = "organization"

// Audit actions this service records. Who was let into the market, and on whose say-so, is
// exactly the question an eligibility review asks afterwards.
const (
	ActionRegistered = "organization.registered"
	ActionApproved   = "organization.approved"
	ActionRejected   = "organization.rejected"
)

// TxRunner is the transaction boundary the service needs.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// Actor is the authenticated caller, for the operator-only actions.
type Actor struct {
	OrganizationID uuid.UUID
	Operator       bool
}

// Service registers organizations and records eligibility decisions.
type Service struct {
	db            TxRunner
	organizations organization.Repository
	challenges    identity.Repository
	sessions      *identity.Service
	audit         audit.Recorder
	now           func() time.Time
	ids           func() uuid.UUID

	// autoApprove grants demo eligibility on registration. It is a development
	// convenience: a demo would otherwise need a second participant to approve the first,
	// and there is no real check behind the decision anyway.
	autoApprove bool
}

// Config wires the service.
type Config struct {
	DB            TxRunner
	Organizations organization.Repository
	Challenges    identity.Repository
	Sessions      *identity.Service
	Audit         audit.Recorder
	AutoApprove   bool
	Now           func() time.Time
	IDs           func() uuid.UUID
}

// NewService returns the service.
func NewService(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IDs == nil {
		cfg.IDs = uuid.New
	}
	if cfg.Audit == nil {
		cfg.Audit = audit.Discard{}
	}
	return &Service{
		db:            cfg.DB,
		organizations: cfg.Organizations,
		challenges:    cfg.Challenges,
		sessions:      cfg.Sessions,
		audit:         cfg.Audit,
		now:           cfg.Now,
		ids:           cfg.IDs,
		autoApprove:   cfg.AutoApprove,
	}
}

// RegisterParams carries a registration and the proof behind it.
type RegisterParams struct {
	// Nonce and Signature are the challenge this wallet signed, which is what proves the
	// caller controls the address they are registering.
	Nonce     string
	Signature string
	Type      organization.Type
	Name      string
}

// Register creates an organization for the wallet that signed the challenge.
//
// The signature is checked before anything is written: without it, anyone could register an
// organization against someone else's address and then be unable to sign in as it, leaving
// the real owner locked out of their own wallet.
func (s *Service) Register(ctx context.Context, p RegisterParams) (*organization.Organization, error) {
	if p.Nonce == "" {
		return nil, apperr.Invalid("nonce", "must not be empty")
	}
	if p.Type == organization.TypeOperator {
		// Registration is open to anyone with a wallet, so it must not be a way to become
		// the party that approves everyone else, reads every receivable and records what
		// debtors paid. An operator is created by an operator, or by the seed.
		return nil, apperr.Forbiddenf("an operator is not created by registering")
	}

	var created *organization.Organization
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		challenge, err := s.challenges.GetChallenge(ctx, q, p.Nonce)
		if err != nil {
			return apperr.Forbiddenf("the signing challenge is unknown or expired")
		}
		if challenge.IsConsumed() || challenge.IsExpired(s.now()) {
			return apperr.Forbiddenf("the signing challenge is unknown or expired")
		}
		if err := identity.VerifySignature(challenge.Wallet, challenge.Message(), p.Signature); err != nil {
			return err
		}

		org, err := organization.New(organization.NewParams{
			ID:     s.ids(),
			Type:   p.Type,
			Name:   p.Name,
			Wallet: challenge.Wallet,
		}, s.now())
		if err != nil {
			return err
		}
		if s.autoApprove {
			if err := org.Approve(s.now()); err != nil {
				return err
			}
		}

		if err := challenge.Consume(s.now()); err != nil {
			return err
		}
		if err := s.challenges.ConsumeChallenge(ctx, q, challenge); err != nil {
			return err
		}
		if err := s.organizations.Create(ctx, q, org); err != nil {
			return err
		}
		// The wallet registered itself, so it is its own actor: nobody else vouched for it.
		if err := s.audit.Record(ctx, q,
			audit.Of(ctx, org.ID.String(), ActionRegistered, EntityType, org.ID.String(), s.now()).
				Between(nil, auditState(org)).
				With("type", org.Type.String()).
				With("wallet", org.Wallet).
				With("auto_approved", s.autoApprove)); err != nil {
			return err
		}

		created = org
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Get returns an organization the caller may see: their own, or any of them for an operator.
func (s *Service) Get(ctx context.Context, actor Actor, id uuid.UUID) (*organization.Organization, error) {
	if !actor.Operator && actor.OrganizationID != id {
		return nil, apperr.NotFoundf("organization %s", id)
	}
	return s.organizations.Get(ctx, s.db.Querier(), id)
}

// List returns organizations of one type. It backs the demo's participant view and is
// operator-only: a directory of everyone's wallets is not something a participant needs.
func (s *Service) List(ctx context.Context, actor Actor, orgType organization.Type, limit int) ([]*organization.Organization, error) {
	if !actor.Operator {
		return nil, apperr.Forbiddenf("listing organizations requires a platform operator")
	}
	if orgType != "" && !orgType.IsValid() {
		return nil, apperr.Invalid("type", "must be one of ISSUER, INVESTOR, OPERATOR")
	}
	return s.organizations.List(ctx, s.db.Querier(), orgType, limit)
}

// auditState is what an eligibility entry's hashes are taken over.
func auditState(org *organization.Organization) map[string]any {
	return map[string]any{"eligibility": org.Eligibility.String(), "version": org.Version}
}

// Approve records a passed demo eligibility check.
func (s *Service) Approve(ctx context.Context, actor Actor, id uuid.UUID) (*organization.Organization, error) {
	return s.decide(ctx, actor, id, ActionApproved, func(org *organization.Organization) error {
		return org.Approve(s.now())
	})
}

// Reject records a failed demo eligibility check, which also takes an already eligible
// organization out of the market.
func (s *Service) Reject(ctx context.Context, actor Actor, id uuid.UUID, reason string) (*organization.Organization, error) {
	return s.decide(ctx, actor, id, ActionRejected, func(org *organization.Organization) error {
		return org.Reject(reason, s.now())
	})
}

func (s *Service) decide(ctx context.Context, actor Actor, id uuid.UUID, action string, apply func(*organization.Organization) error) (*organization.Organization, error) {
	if !actor.Operator {
		return nil, apperr.Forbiddenf("an eligibility decision requires a platform operator")
	}

	var updated *organization.Organization
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		org, err := s.organizations.Get(ctx, q, id)
		if err != nil {
			return err
		}

		before := auditState(org)
		expectedVersion := org.Version
		if err := apply(org); err != nil {
			return err
		}
		if err := s.organizations.Update(ctx, q, org, expectedVersion); err != nil {
			return err
		}
		// The operator who decided is the actor: an eligibility decision with nobody
		// behind it is not a decision anyone can review.
		if err := s.audit.Record(ctx, q,
			audit.Of(ctx, actor.OrganizationID.String(), action, EntityType, org.ID.String(), s.now()).
				Between(before, auditState(org)).
				With("eligibility", org.Eligibility.String()).
				With("reason", org.Reason)); err != nil {
			return err
		}

		updated = org
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}
