// Package auth is the demo stand-in for authentication: every request names
// one of three roles, and handlers declare which roles may call them.
//
// There are no credentials. The caller picks a role with the X-Role header
// (or, for browser sessions, the mavi_role cookie) and optionally identifies
// themselves with X-Actor: the candidate id for talent, a company name for
// employers, an email for ops. The actor is what scopes talent to their own
// records and what the review audit trail records.
package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/colehanke/mavi-demo/api/internal/contract"
)

// Role is one of the three demo personas: the API contract's Persona enum
// (api/openapi.yaml), so the header values are defined once.
type Role = contract.Persona

const (
	Talent   = contract.PersonaTalent   // a candidate managing their own record
	Employer = contract.PersonaEmployer // a hiring company; only ever sees released matches
	Ops      = contract.PersonaOps      // internal reviewer; sees and edits everything
)

// All lists every valid role.
var All = []Role{Talent, Employer, Ops}

const (
	RoleHeader  = "X-Role"
	ActorHeader = "X-Actor"
	RoleCookie  = "mavi_role"
	ActorCookie = "mavi_actor"
)

// Identity is what a request carries: a role and an optional actor id.
type Identity struct {
	Role  Role
	Actor string
}

// Parse returns the role named by s, or false if it is not one of All.
func Parse(s string) (Role, bool) {
	r := Role(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range All {
		if r == known {
			return r, true
		}
	}
	return "", false
}

type ctxKey struct{}

// FromContext returns the identity the middleware attached, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// WithIdentity attaches an identity to ctx; tests use it to bypass the
// middleware.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromRequest reads the identity from the header, falling back to the
// session cookie. ok is false when no role is present or it is unknown.
func FromRequest(r *http.Request) (Identity, bool) {
	raw := r.Header.Get(RoleHeader)
	actor := r.Header.Get(ActorHeader)
	if raw == "" {
		if c, err := r.Cookie(RoleCookie); err == nil {
			raw = c.Value
		}
		if actor == "" {
			if c, err := r.Cookie(ActorCookie); err == nil {
				actor = c.Value
			}
		}
	}
	role, ok := Parse(raw)
	if !ok {
		return Identity{}, false
	}
	return Identity{Role: role, Actor: strings.TrimSpace(actor)}, true
}

// Rejecter writes the error response for a rejected request, so the
// middleware can use the server's JSON error format.
type Rejecter func(w http.ResponseWriter, code int, msg string)

// Require wraps next so it only runs for one of the given roles. A request
// with no role or an unknown role gets 401; a known role that is not listed
// gets 403. The identity is attached to the request context for the handler.
func Require(reject Rejecter, next http.Handler, roles ...Role) http.Handler {
	allowed := map[Role]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromRequest(r)
		if !ok {
			reject(w, http.StatusUnauthorized, "missing or unknown role: set "+RoleHeader+" to one of talent, employer, ops")
			return
		}
		if !allowed[id.Role] {
			reject(w, http.StatusForbidden, "role "+string(id.Role)+" may not call this endpoint")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}
