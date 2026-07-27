package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Role is the permission level assigned to a user account or API token.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// ParseRole validates and normalizes a role string.
func ParseRole(s string) (Role, error) {
	switch r := Role(strings.ToLower(strings.TrimSpace(s))); r {
	case RoleAdmin, RoleOperator, RoleViewer:
		return r, nil
	default:
		return "", fmt.Errorf("invalid role %q: must be one of admin, operator, viewer", s)
	}
}

// rank orders roles from least to most privileged: viewer < operator < admin.
func (r Role) rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// Allows reports whether r has at least the privilege level of min.
func (r Role) Allows(min Role) bool { return r.rank() >= min.rank() }

// Principal identifies the authenticated caller of a request, whether via
// a browser session, an API token, or the implicitly trusted Unix socket.
type Principal struct {
	Subject string // username, or "token:<name>"
	Role    Role
	Via     string // "session", "token" or "trusted"
}

type principalContextKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

func principalFrom(r *http.Request) (Principal, bool) {
	p, ok := r.Context().Value(principalContextKey{}).(Principal)
	return p, ok
}

// requireRole wraps next so it only runs if the request's principal has at
// least the given role; otherwise it responds 401 (no principal) or 403
// (insufficient role).
func requireRole(min Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, fmt.Errorf("authentication required"))
			return
		}
		if !p.Role.Allows(min) {
			writeError(w, http.StatusForbidden, fmt.Errorf("role %q does not have %q access", p.Role, min))
			return
		}
		next(w, r)
	}
}
