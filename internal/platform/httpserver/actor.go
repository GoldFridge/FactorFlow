package httpserver

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// actorKey is the context key for the authenticated caller.
const actorKey contextKey = 1

// Actor is the authenticated caller of a request.
//
// Authorization decisions are made against this, never against a field in the request
// body: a client that could name its own organization could act as any organization.
type Actor struct {
	OrganizationID uuid.UUID
	Wallet         string
	// Role scopes what the actor may do inside its organization.
	Role string
	// Operator marks a platform operator, who may run reconciliation and replay outbox
	// work but has no access to document plaintext.
	Operator bool
}

// Roles an actor can hold.
const (
	RoleOwner    = "OWNER"
	RoleMember   = "MEMBER"
	RoleOperator = "OPERATOR"
)

// IsZero reports whether no actor is present.
func (a Actor) IsZero() bool { return a.OrganizationID == uuid.Nil }

// Resolver turns a request into an actor.
//
// The live implementation reads the wallet session cookie; tests and the seeded demo use a
// static resolver. Keeping it an interface is what lets the transport layer be finished and
// tested before the wallet signature flow lands.
type Resolver interface {
	Resolve(r *http.Request) (Actor, error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(r *http.Request) (Actor, error)

// Resolve implements Resolver.
func (f ResolverFunc) Resolve(r *http.Request) (Actor, error) { return f(r) }

// Authenticate attaches the actor to the request context when one can be resolved, and
// leaves the request anonymous otherwise. Rejecting is RequireActor's job, so a route can
// be public without bypassing session handling.
func Authenticate(resolver Resolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if resolver == nil {
				next.ServeHTTP(w, r)
				return
			}

			actor, err := resolver.Resolve(r)
			if err != nil || actor.IsZero() {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithActor(r.Context(), actor)))
		})
	}
}

// RequireActor rejects a request that carries no authenticated actor.
func RequireActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ActorFrom(r.Context()).IsZero() {
			WriteProblem(w, r, ErrUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireOperator rejects a request from anyone but a platform operator.
func RequireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := ActorFrom(r.Context())
		if actor.IsZero() {
			WriteProblem(w, r, ErrUnauthorized)
			return
		}
		if !actor.Operator {
			WriteProblem(w, r, errForbiddenOperator)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ContextWithActor attaches an actor, so a worker can act with the same scope as the
// request that queued its work.
func ContextWithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey, actor)
}

// ActorFrom returns the actor on a context, or the zero actor.
func ActorFrom(ctx context.Context) Actor {
	actor, _ := ctx.Value(actorKey).(Actor)
	return actor
}
