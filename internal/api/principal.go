package api

import (
	"context"
	"strings"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

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
	// DisplayName and Email are display-only identity from the verified ID
	// token (name / preferred_username / email claims). They carry no
	// authorization meaning and must never be used for ownership.
	DisplayName string
	Email       string
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
	ctxKeyRouteAudit
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

// ---------------------------------------------------------------------------
// Principal directory — display identity for tenant views (migration 012).
// ---------------------------------------------------------------------------

// Owner is the public identity block carried by workspace and retained-data
// views (openapi Owner). Subject is the bare OIDC sub; DisplayName is the
// name the principal directory last saw at login, falling back to Subject.
type Owner struct {
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName"`
}

// Directory persists and resolves display identities for owner references
// (issuer|sub). Display data only — never an authorization input. The
// Postgres implementation is store.PrincipalDirectory.
type Directory interface {
	// Remember upserts the caller's display identity after a login.
	Remember(ctx context.Context, tenantID string, e store.DirectoryEntry) error
	// Lookup returns the known entries for ownerRefs within tenantID.
	Lookup(ctx context.Context, tenantID string, ownerRefs []string) (map[string]store.DirectoryEntry, error)
}

// NewDirectory wraps the Postgres principal directory (migration 012).
func NewDirectory(db *store.DB) Directory { return store.NewPrincipalDirectory(db) }

// ownerFallback is the public Owner for a reference the directory does not
// know: subject used as its own display name.
func ownerFallback(ownerRef string) Owner {
	sub := ownerRefSubject(ownerRef)
	return Owner{Subject: sub, DisplayName: sub}
}

// resolveOwner resolves one owner reference — convenience for single-record
// responses (resolveOwners batches list pages).
func resolveOwner(ctx context.Context, d Directory, tenantID, ownerRef string) Owner {
	return resolveOwners(ctx, d, tenantID, []string{ownerRef})[ownerRef]
}

// ownerRefSubject extracts the bare subject from an issuer|sub owner
// reference; a ref without a separator is returned whole.
func ownerRefSubject(ownerRef string) string {
	if _, sub, ok := strings.Cut(ownerRef, "|"); ok {
		return sub
	}
	return ownerRef
}

// resolveOwners builds the public Owner block per owner reference. The
// directory fills display names; owners the directory does not know fall
// back to their subject. A nil directory or a lookup failure degrades to
// subject-only — display data must never break a read.
func resolveOwners(ctx context.Context, d Directory, tenantID string, ownerRefs []string) map[string]Owner {
	out := make(map[string]Owner, len(ownerRefs))
	for _, ref := range ownerRefs {
		sub := ownerRefSubject(ref)
		out[ref] = Owner{Subject: sub, DisplayName: sub}
	}
	if d == nil || len(ownerRefs) == 0 {
		return out
	}
	entries, err := d.Lookup(ctx, tenantID, ownerRefs)
	if err != nil {
		return out
	}
	for ref, e := range entries {
		o := out[ref]
		if e.Subject != "" {
			o.Subject = e.Subject
		}
		if e.DisplayName != "" {
			o.DisplayName = e.DisplayName
		}
		out[ref] = o
	}
	return out
}
