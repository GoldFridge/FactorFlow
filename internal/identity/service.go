package identity

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// TxRunner is the transaction boundary the service needs.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// Account is what the identity layer needs to know about the organization behind a wallet.
//
// It is a port rather than an import of the organization module: this module authenticates,
// it does not own who an organization is.
type Account struct {
	OrganizationID uuid.UUID
	Wallet         string
	Eligible       bool
	Operator       bool
}

// Accounts resolves the organization a wallet acts for.
type Accounts interface {
	ByWallet(ctx context.Context, q postgres.Querier, wallet string) (Account, error)
}

// Service issues login challenges and exchanges signatures for sessions.
type Service struct {
	db       TxRunner
	repo     Repository
	accounts Accounts
	now      func() time.Time
}

// NewService wires the service.
func NewService(db TxRunner, repo Repository, accounts Accounts, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, repo: repo, accounts: accounts, now: now}
}

// ChallengeResult is what a wallet must sign.
type ChallengeResult struct {
	Nonce     string
	Message   string
	ExpiresAt time.Time
}

// Challenge mints a one-time message for a wallet to sign.
//
// It does not check that the wallet belongs to a known organization. Telling an anonymous
// caller whether an address is registered would turn this endpoint into a directory of
// participants; the check happens after the signature proves who is asking.
func (s *Service) Challenge(ctx context.Context, wallet string) (ChallengeResult, error) {
	challenge, err := NewChallenge(wallet, s.now())
	if err != nil {
		return ChallengeResult{}, err
	}

	if err := s.db.InTx(ctx, func(q postgres.Querier) error {
		return s.repo.SaveChallenge(ctx, q, challenge)
	}); err != nil {
		return ChallengeResult{}, err
	}

	return ChallengeResult{
		Nonce:     challenge.Nonce,
		Message:   challenge.Message(),
		ExpiresAt: challenge.ExpiresAt,
	}, nil
}

// SessionResult is a minted session.
type SessionResult struct {
	Token          string
	OrganizationID uuid.UUID
	Wallet         string
	ExpiresAt      time.Time
}

// Verify exchanges a signed challenge for a session.
//
// Everything happens in one transaction, and the challenge is consumed before the session is
// created: two requests racing with the same captured signature can produce at most one
// session, because the database decides which one consumes the nonce.
func (s *Service) Verify(ctx context.Context, nonce, signature string) (SessionResult, error) {
	if nonce == "" {
		return SessionResult{}, apperr.Invalid("nonce", "must not be empty")
	}

	var result SessionResult
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		challenge, err := s.repo.GetChallenge(ctx, q, nonce)
		if err != nil {
			// An unknown nonce and an expired one are the same answer on purpose: a caller
			// guessing nonces learns nothing from the difference.
			return errUnauthorized()
		}
		if challenge.IsConsumed() || challenge.IsExpired(s.now()) {
			return errUnauthorized()
		}

		if err := VerifySignature(challenge.Wallet, challenge.Message(), signature); err != nil {
			return err
		}

		account, err := s.accounts.ByWallet(ctx, q, challenge.Wallet)
		if err != nil {
			if apperr.IsNotFound(err) {
				return apperr.Forbiddenf("wallet %s is not registered to an organization", challenge.Wallet)
			}
			return err
		}

		if err := challenge.Consume(s.now()); err != nil {
			return err
		}
		if err := s.repo.ConsumeChallenge(ctx, q, challenge); err != nil {
			return err
		}

		session, token, err := NewSession(account.OrganizationID, challenge.Wallet, s.now())
		if err != nil {
			return err
		}
		if err := s.repo.SaveSession(ctx, q, session); err != nil {
			return err
		}

		result = SessionResult{
			Token:          token,
			OrganizationID: session.OrganizationID,
			Wallet:         session.Wallet,
			ExpiresAt:      session.ExpiresAt,
		}
		return nil
	})
	if err != nil {
		return SessionResult{}, err
	}
	return result, nil
}

// Resolve turns a session token into the caller it authenticates.
//
// The organization is read on every request rather than copied into the session: an
// eligibility decision or a revoked account has to take effect immediately, not when the
// session happens to expire.
func (s *Service) Resolve(ctx context.Context, token string) (httpserver.Actor, error) {
	if token == "" {
		return httpserver.Actor{}, errUnauthorized()
	}

	session, err := s.repo.GetSession(ctx, s.db.Querier(), HashToken(token))
	if err != nil {
		return httpserver.Actor{}, errUnauthorized()
	}
	if !session.IsActive(s.now()) {
		return httpserver.Actor{}, errUnauthorized()
	}

	account, err := s.accounts.ByWallet(ctx, s.db.Querier(), session.Wallet)
	if err != nil {
		return httpserver.Actor{}, errUnauthorized()
	}
	if account.OrganizationID != session.OrganizationID {
		// The wallet now acts for a different organization than when it signed in.
		return httpserver.Actor{}, errUnauthorized()
	}

	return httpserver.Actor{
		OrganizationID: account.OrganizationID,
		Wallet:         session.Wallet,
		Role:           httpserver.RoleOwner,
		Eligible:       account.Eligible,
		Operator:       account.Operator,
	}, nil
}

// Logout revokes a session. Revoking an unknown or already revoked token is not an error:
// the caller asked to be logged out, and afterwards they are.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}

	return s.db.InTx(ctx, func(q postgres.Querier) error {
		session, err := s.repo.GetSession(ctx, q, HashToken(token))
		if err != nil {
			if apperr.IsNotFound(err) {
				return nil
			}
			return err
		}

		session.Revoke(s.now())
		return s.repo.RevokeSession(ctx, q, session)
	})
}

// PurgeExpiredChallenges removes challenges past their window. It is safe to run on a timer
// and returns how many rows went.
func (s *Service) PurgeExpiredChallenges(ctx context.Context) (int64, error) {
	var removed int64
	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		removed, err = s.repo.DeleteExpiredChallenges(ctx, q, s.now())
		return err
	})
	return removed, err
}

// errUnauthorized is the single answer to every failed login attempt, so a caller cannot
// tell an unknown nonce from an expired one or a revoked session from a forged token.
func errUnauthorized() error { return httpserver.ErrUnauthorized }

// Resolver adapts the service to the transport's session resolver.
type Resolver struct {
	service *Service
	// CookieName is the cookie a browser session is carried in.
	CookieName string
}

// NewResolver returns a resolver that reads the session from a cookie or a bearer token.
func NewResolver(service *Service, cookieName string) *Resolver {
	return &Resolver{service: service, CookieName: cookieName}
}

// Resolve implements httpserver.Resolver.
//
// A bearer token is checked first: it is what a machine client sends and, unlike a cookie,
// a browser will never attach it to a cross-site request on its own.
func (r *Resolver) Resolve(request *http.Request) (httpserver.Actor, error) {
	if token := bearerToken(request); token != "" {
		return r.service.Resolve(request.Context(), token)
	}
	if cookie, err := request.Cookie(r.CookieName); err == nil {
		return r.service.Resolve(request.Context(), cookie.Value)
	}
	return httpserver.Actor{}, nil
}
