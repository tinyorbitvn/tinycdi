// Package broker owns launch tickets, connection leases and session-target
// resolution for interactive workspace sessions (design §6).
package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ErrNotImplemented marks behaviour not yet implemented.
var ErrNotImplemented = errors.New("broker: not implemented")

// Domain errors the contract tests discriminate on. Implementations must
// return these (wrapped is fine) rather than ad-hoc strings.
var (
	// ErrTicketInvalid — ticket unknown, already consumed, wrong audience or
	// otherwise unredeemable.
	ErrTicketInvalid = errors.New("broker: ticket invalid or already used")
	// ErrTicketExpired — ticket past its 60 s TTL; the attempt still consumes it.
	ErrTicketExpired = errors.New("broker: ticket expired")
	// ErrDenied — authenticated but not permitted (wrong tenant, workspace,
	// gateway identity or lease ownership).
	ErrDenied = errors.New("broker: denied")
	// ErrStaleBinding — the fence presented by the gateway no longer matches
	// the current incarnation (stale runtimeGeneration or runtimeUID).
	ErrStaleBinding = errors.New("broker: stale runtime binding")
	// ErrConnectionInUse — a live interactive lease exists and the ticket was
	// not issued with takeover.
	ErrConnectionInUse = errors.New("broker: workspace already has an active lease")
	// ErrLeaseInvalid — lease unknown, expired, revoked or superseded.
	ErrLeaseInvalid = errors.New("broker: lease invalid or expired")
	// ErrRevoked — ticket or lease explicitly revoked.
	ErrRevoked = errors.New("broker: revoked")
	// ErrFreshness — the binding source's observed state is older than the
	// 15 s freshness budget; issuing/renewing must stop until fresh again.
	ErrFreshness = errors.New("broker: observed runtime state too stale")
	// ErrNotReady — the workspace is not in a connectable phase.
	ErrNotReady = errors.New("broker: workspace not ready")
	// ErrNotFound — workspace unknown or not visible to the principal's tenant.
	ErrNotFound = errors.New("broker: not found")
)

// Contract constants from design §6.
const (
	// TicketTTL is the launch-ticket lifetime; tickets are single-use.
	TicketTTL = 60 * time.Second
	// LeaseTTL is the sliding lease lifetime; the gateway renews well inside it.
	LeaseTTL = 30 * time.Second
	// LeaseRenewInterval is the expected gateway renew cadence.
	LeaseRenewInterval = 10 * time.Second
	// MaxBindingAge is the freshness budget for operator-observed state;
	// beyond it the broker must stop issuing and renewing (fail closed).
	MaxBindingAge = 15 * time.Second

	// DefaultGatewayAudience is the audience tickets bind to when no
	// -gateway-audience/session origin is configured; deployments override it.
	DefaultGatewayAudience = "session.example.dev"

	// PhaseReady is the Workspace phase that permits issuing tickets
	// (mirrors api/v1alpha1.WorkspacePhaseReady — kept as a string so the
	// broker does not depend on the CRD types for its core contract).
	PhaseReady = "Ready"
)

// Clock lets tests drive expiry deterministically.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Option configures a Broker.
type Option func(*Broker)

// WithClock injects a clock (fake in tests).
func WithClock(c Clock) Option { return func(b *Broker) { b.clock = c } }

// WithTicketTTL overrides TicketTTL.
func WithTicketTTL(d time.Duration) Option { return func(b *Broker) { b.ticketTTL = d } }

// WithLeaseTTL overrides LeaseTTL.
func WithLeaseTTL(d time.Duration) Option { return func(b *Broker) { b.leaseTTL = d } }

// WithMaxBindingAge overrides MaxBindingAge.
func WithMaxBindingAge(d time.Duration) Option { return func(b *Broker) { b.maxBindingAge = d } }

// WithGatewayAudience sets the session-gateway audience tickets are bound to.
// The gateway's mTLS identity maps to this audience at the internal API.
func WithGatewayAudience(aud string) Option {
	return func(b *Broker) { b.audience = aud }
}

// WithCredentialSource sets the resolver for per-workspace runtime
// credentials (production: the K8s Secret reader). When unset, the broker
// derives deterministic per-workspace material that fails closed upstream —
// it exists so the unit contract runs without Kubernetes, never to serve
// real sessions.
func WithCredentialSource(src CredentialSource) Option {
	return func(b *Broker) { b.creds = src }
}

// PlatformID is the platform workspace identity ("ws_<hex>") the broker
// and the DB route on — aliased because the broker speaks it natively.
// The Workspace CR's metadata.uid (RuntimeBinding.CRUID / linux.CRUID) is
// a different identity used only for Kubernetes object names; mixing the
// two is a compile error.
type PlatformID = provisioning.PlatformID

// BindingSource supplies the operator-reported incarnation of a workspace.
// The production implementation projects Workspace CR status (bindings.go);
// tests inject a fake.
type BindingSource interface {
	CurrentBinding(ctx context.Context, workspaceUID PlatformID) (RuntimeBinding, error)
}

// RuntimeBinding is the current runtime incarnation as observed by the
// operator. Tickets and leases bind the triple (WorkspaceUID,
// RuntimeGeneration, RuntimeUID): a recreated runtime changes RuntimeUID and
// auto-fences access granted to the old incarnation.
type RuntimeBinding struct {
	// WorkspaceUID is the platform id (ws_…) — the identity tickets and
	// leases are bound to. It never names a Kubernetes object.
	WorkspaceUID PlatformID
	// CRUID is the Workspace CR's metadata.uid — the identity the operator
	// names the runtime children (ws-<cruid>, ws-<cruid>-rt, …) from. It is
	// the ONLY input credential/target resolution may derive Kubernetes
	// object names from.
	CRUID             types.UID
	TenantID          string
	OwnerSubject      string // iss|sub of the workspace owner
	Phase             string // "Ready" required to issue
	RuntimeGeneration uint64
	RuntimeUID        string
	ObservedAt        time.Time // must be within MaxBindingAge of Now()

	// Namespace/ServiceName/ServicePort project metadata.namespace and
	// status.serviceRef for target resolution; empty when unknown.
	Namespace   string
	ServiceName string
	ServicePort int32
}

// GatewayIdentity is the authenticated session-gateway caller (mTLS-derived).
type GatewayIdentity struct {
	ID       string // unique gateway instance identity
	Audience string // audience the ticket/lease is bound to
}

// Ticket is the launch ticket returned to the caller by IssueTicket. Token is
// opaque (~256 bits); it is delivered in the response body only, never in a
// URL query string, and only its SHA-256 hash is persisted.
type Ticket struct {
	WorkspaceID PlatformID
	Token       string
	ExpiresAt   time.Time
}

// Broker issues tickets and manages connection leases over the control-plane
// store, bound to the current runtime incarnation reported by BindingSource.
type Broker struct {
	db            *store.DB
	src           BindingSource
	clock         Clock
	ticketTTL     time.Duration
	leaseTTL      time.Duration
	maxBindingAge time.Duration
	audience      string
	creds         CredentialSource

	synthOnce  sync.Once
	synthCreds *synthesizedCredentials
	synthErr   error
}

// New returns a Broker. src is required for issue/renew/resolve; db is
// required for every operation.
func New(db *store.DB, src BindingSource, opts ...Option) *Broker {
	b := &Broker{
		db:            db,
		src:           src,
		clock:         systemClock{},
		ticketTTL:     TicketTTL,
		leaseTTL:      LeaseTTL,
		maxBindingAge: MaxBindingAge,
		audience:      DefaultGatewayAudience,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// now is the single clock read for an operation.
func (b *Broker) now() time.Time { return b.clock.Now() }

// fresh reports whether the binding's observation is inside the freshness
// budget. Future-dated observations are tolerated (clock skew), zero never is.
func (b *Broker) fresh(binding RuntimeBinding, now time.Time) error {
	if binding.ObservedAt.IsZero() || now.Sub(binding.ObservedAt) > b.maxBindingAge {
		return ErrFreshness
	}
	return nil
}

// currentBinding resolves and freshness-checks the binding for wsUID.
func (b *Broker) currentBinding(ctx context.Context, wsUID PlatformID, now time.Time) (RuntimeBinding, error) {
	if b.src == nil {
		return RuntimeBinding{}, ErrFreshness
	}
	binding, err := b.src.CurrentBinding(ctx, wsUID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RuntimeBinding{}, ErrNotFound
		}
		if errors.Is(err, ErrFreshness) {
			return RuntimeBinding{}, ErrFreshness
		}
		return RuntimeBinding{}, fmt.Errorf("broker: binding lookup: %w", err)
	}
	if err := b.fresh(binding, now); err != nil {
		return RuntimeBinding{}, err
	}
	return binding, nil
}

func randToken() (string, error) {
	return randID("tkt_")
}

func randID(prefix string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func ticketHash(opaque string) []byte {
	sum := sha256.Sum256([]byte(opaque))
	return sum[:]
}

// requestID pulls the request-scoped correlation ID when the caller went
// through the API middleware, else mints one for internal callers.
func requestID(ctx context.Context) string {
	if id := api.RequestIDFromContext(ctx); id != "" {
		return id
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "req-unknown"
	}
	return "req-" + base64.RawURLEncoding.EncodeToString(raw[:])
}

// IssueTicket mints a 60 s, single-use opaque ticket for workspaceUID bound to
// (workspaceUID, runtimeGeneration, runtimeUID, gateway audience) of the
// current incarnation. Fails with ErrNotFound/ErrDenied when the principal may
// not connect, ErrNotReady when the workspace is not Ready, ErrFreshness when
// observed state is stale, and ErrConnectionInUse when a live lease exists and
// takeover is false. With takeover the ticket may supersede the live lease at
// redemption, fencing the old socket before the new session works.
func (b *Broker) IssueTicket(ctx context.Context, p api.Principal, workspaceUID PlatformID, takeover bool) (Ticket, error) {
	now := b.now()

	// Ownership check against the authoritative workspace row: cross-tenant
	// and deleted workspaces are indistinguishable from unknown ones.
	var tenantID, owner, state string
	err := b.db.Pool().QueryRow(ctx,
		`SELECT tenant_id, owner_subject, state FROM workspaces WHERE id = $1`,
		workspaceUID).Scan(&tenantID, &owner, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Ticket{}, ErrNotFound
	case err != nil:
		return Ticket{}, fmt.Errorf("broker: workspace lookup: %w", err)
	}
	if state != "active" || tenantID != p.TenantID {
		return Ticket{}, ErrNotFound
	}
	if owner != p.Owner() {
		return Ticket{}, ErrDenied
	}

	binding, err := b.currentBinding(ctx, workspaceUID, now)
	if err != nil {
		return Ticket{}, err
	}
	if binding.Phase != PhaseReady {
		return Ticket{}, ErrNotReady
	}
	if binding.TenantID != "" && binding.TenantID != tenantID {
		return Ticket{}, ErrDenied
	}

	// Operator revocation (design §5 teardown): a workspace_revocation row
	// covering this generation blocks new tickets — connects stay blocked
	// while teardown proceeds, but a newer runtime generation reconnects.
	revoked, err := b.revokedGeneration(ctx, workspaceUID, binding.RuntimeGeneration)
	if err != nil {
		return Ticket{}, err
	}
	if revoked {
		return Ticket{}, ErrDenied
	}

	// Advisory live-lease gate: a live lease blocks issue unless the caller
	// asked for takeover. The atomic claim happens at redeem; this check is
	// what maps to 409 CONNECTION_IN_USE for the public API.
	var live string
	err = b.db.Pool().QueryRow(ctx,
		`SELECT id FROM connection_lease
		 WHERE workspace_id = $1 AND state = 'active' AND expires_at > $2`,
		workspaceUID, now).Scan(&live)
	switch {
	case err == nil:
		if !takeover {
			return Ticket{}, ErrConnectionInUse
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return Ticket{}, fmt.Errorf("broker: lease check: %w", err)
	}

	token, err := randToken()
	if err != nil {
		return Ticket{}, fmt.Errorf("broker: mint ticket: %w", err)
	}
	expires := now.Add(b.ticketTTL)
	if _, err := b.db.Pool().Exec(ctx,
		`INSERT INTO launch_ticket
			(ticket_hash, workspace_id, tenant_id, principal_subject,
			 runtime_generation, runtime_uid, audience, takeover,
			 request_id, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		ticketHash(token), workspaceUID, tenantID, owner,
		int64(binding.RuntimeGeneration), binding.RuntimeUID,
		b.audience, takeover, requestID(ctx), expires); err != nil {
		return Ticket{}, fmt.Errorf("broker: insert ticket: %w", err)
	}
	return Ticket{WorkspaceID: workspaceUID, Token: token, ExpiresAt: expires}, nil
}

// RedeemTicket atomically consumes the ticket — even when expired — and
// claims the workspace's single active lease in one transaction, bound to the
// incarnation recorded at issuance. A ticket never redeems twice, and a
// revoked ticket never redeems. On success it returns the new Lease.
func (b *Broker) RedeemTicket(ctx context.Context, gw GatewayIdentity, opaque string) (Lease, error) {
	now := b.now()
	hash := ticketHash(opaque)
	var lease Lease
	err := b.db.WithTx(ctx, func(tx store.Tx) error {
		var (
			wsUID, tenantID, subject, runtimeUID, audience string
			gen                                            uint64
			takeover                                       bool
			expiresAt                                      time.Time
			consumedAt, revokedAt                          *time.Time
		)
		err := tx.QueryRow(ctx,
			`SELECT workspace_id, tenant_id, principal_subject, runtime_generation,
				runtime_uid, audience, takeover, expires_at, consumed_at, revoked_at
			 FROM launch_ticket WHERE ticket_hash = $1 FOR UPDATE`, hash).
			Scan(&wsUID, &tenantID, &subject, &gen, &runtimeUID, &audience,
				&takeover, &expiresAt, &consumedAt, &revokedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrTicketInvalid
		case err != nil:
			return fmt.Errorf("broker: ticket lookup: %w", err)
		}
		switch {
		case revokedAt != nil:
			return ErrRevoked
		case consumedAt != nil:
			return ErrTicketInvalid
		case audience != gw.Audience:
			// Validation failure: the attempt does not consume the ticket —
			// the correctly-audienced gateway may still redeem it.
			return ErrDenied
		}
		// Consume on first redemption attempt, even when already expired.
		if _, err := tx.Exec(ctx,
			`UPDATE launch_ticket SET consumed_at = $2 WHERE ticket_hash = $1`,
			hash, now); err != nil {
			return fmt.Errorf("broker: consume ticket: %w", err)
		}
		if !now.Before(expiresAt) {
			return ErrTicketExpired
		}

		// A revocation covering the ticket's generation blocks redemption:
		// the ticket is consumed (single-use still holds) but no lease
		// issues for a generation under teardown.
		var revoked bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM workspace_revocation
				WHERE workspace_id = $1 AND runtime_generation >= $2)`,
			wsUID, int64(gen)).Scan(&revoked); err != nil {
			return fmt.Errorf("broker: revocation check: %w", err)
		}
		if revoked {
			return ErrRevoked
		}

		// Claim the single active lease for this workspace. The FOR UPDATE
		// read plus the partial unique index serialize concurrent claims;
		// a takeover fences the old lease BEFORE the new lease exists.
		var liveID string
		var liveExp time.Time
		err = tx.QueryRow(ctx,
			`SELECT id, expires_at FROM connection_lease
			 WHERE workspace_id = $1 AND state = 'active' FOR UPDATE`, wsUID).
			Scan(&liveID, &liveExp)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// no live lease
		case err != nil:
			return fmt.Errorf("broker: lease lookup: %w", err)
		default:
			next := `UPDATE connection_lease SET state = 'expired', closed_at = $2 WHERE id = $1`
			switch {
			case !liveExp.After(now):
			case takeover:
				next = `UPDATE connection_lease SET state = 'superseded', closed_at = $2 WHERE id = $1`
			default:
				return ErrConnectionInUse
			}
			if _, err := tx.Exec(ctx, next, liveID, now); err != nil {
				return fmt.Errorf("broker: fence old lease: %w", err)
			}
		}

		var fencing uint64
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(fencing_version), 0) + 1
			 FROM connection_lease WHERE workspace_id = $1`, wsUID).Scan(&fencing); err != nil {
			return fmt.Errorf("broker: fencing version: %w", err)
		}
		id, err := randID("lease-")
		if err != nil {
			return err
		}
		lease = Lease{
			ID:                id,
			WorkspaceUID:      wsUID,
			TenantID:          tenantID,
			PrincipalSubject:  subject,
			RuntimeGeneration: gen,
			RuntimeUID:        runtimeUID,
			FencingVersion:    fencing,
			GatewayID:         gw.ID,
			ExpiresAt:         now.Add(b.leaseTTL),
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO connection_lease
				(id, workspace_id, tenant_id, principal_subject,
				 runtime_generation, runtime_uid, fencing_version, gateway_id,
				 state, created_at, expires_at, last_renewed_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', $9, $10, $9)`,
			lease.ID, wsUID, tenantID, subject, int64(gen), runtimeUID,
			int64(fencing), gw.ID, now, lease.ExpiresAt); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrConnectionInUse
			}
			return fmt.Errorf("broker: insert lease: %w", err)
		}
		return nil
	})
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// RevokeTicket permanently denies a ticket that has not been consumed; a
// revoked ticket can never redeem. Revoking a ticket that did not produce the
// current lease must not affect the live session.
func (b *Broker) RevokeTicket(ctx context.Context, opaque string) error {
	_, err := b.db.Pool().Exec(ctx,
		`UPDATE launch_ticket SET revoked_at = $2
		 WHERE ticket_hash = $1 AND consumed_at IS NULL`,
		ticketHash(opaque), b.now())
	if err != nil {
		return fmt.Errorf("broker: revoke ticket: %w", err)
	}
	return nil
}
