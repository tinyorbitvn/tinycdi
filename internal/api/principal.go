package api

import "context"

// Principal is the verified caller identity for the public API. Every field is
// populated exclusively from a validated OIDC ID token (carried by the
// server-side session) — never from request bodies, headers, or URL
// parameters. Handlers MUST derive ownership (owner_subject, tenant) from the
// Principal in the request context; an "owner" field in a request body is
// always ignored.
type Principal struct {
	// Issuer is the verified iss claim of the ID token.
	Issuer string
	// Subject is the verified sub claim of the ID token.
	Subject string
	// TenantID is the verified tenant/membership claim (TenantClaim).
	TenantID string
	// Groups is the verified group-membership claim list (GroupsClaim).
	Groups []string
}

// Owner returns the canonical, immutable owner reference for resources
// created by this principal: the OIDC issuer + subject pair (design:
// ownerSubject is "OIDC issuer + sub bất biến").
func (p Principal) Owner() string { return p.Issuer + "|" + p.Subject }

// InGroup reports whether the principal carries the given verified group
// membership.
func (p Principal) InGroup(group string) bool {
	for _, g := range p.Groups {
		if g == group {
			return true
		}
	}
	return false
}

type ctxKey int

const (
	ctxKeyPrincipal ctxKey = iota
	ctxKeyRequestID
	ctxKeySession
	ctxKeyAuditCollector
)

// WithPrincipal stores a verified principal in the context. Only
// authentication code may call this.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// PrincipalFromContext returns the verified principal attached by the authn
// middleware. The second return value is false when the request was not
// authenticated — handlers must treat that as a bug (the middleware should
// have rejected the request) and must not fabricate an owner.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKeyPrincipal).(Principal)
	return p, ok
}

// SessionFromContext returns the server-side session attached by the authn
// middleware.
func SessionFromContext(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(ctxKeySession).(*Session)
	return s, ok
}
