package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// writeError is the package-local convenience wrapper around WriteError
// (errors.go) that pulls the request ID from context.
func writeError(w http.ResponseWriter, r *http.Request, code ErrorCode, msg string) {
	WriteError(w, RequestIDFromContext(r.Context()), NewError(code, msg))
}

// ---------------------------------------------------------------------------
// Request ID
// ---------------------------------------------------------------------------

// RequestIDHeader is propagated inbound and always set on the response.
const RequestIDHeader = "X-Request-Id"

// RequestID ensures every request carries an ID: an inbound X-Request-Id that
// is short and printable is propagated, otherwise a fresh `req-` ID is
// generated. The ID is stored in the context and set on the response before
// the handler runs, so error responses carry it too.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = "req-" + mustRandToken(12)
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := contextWithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func contextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestIDFromContext returns the request ID assigned by RequestID.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// RequireAuth rejects requests without a valid server-side session (opaque
// host-only cookie) and attaches the verified Principal and Session to the
// request context. Handlers must derive owner/tenant from that principal.
// Each authenticated request slides the session's idle deadline (Get).
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return a.requireAuth(next, true)
}

// RequireAuthPassive authenticates exactly like RequireAuth but does not
// slide the idle timer (Peek): passive endpoints the portal polls on a timer
// must not keep an unattended session alive forever (P4, D18).
func (a *Authenticator) RequireAuthPassive(next http.Handler) http.Handler {
	return a.requireAuth(next, false)
}

func (a *Authenticator) requireAuth(next http.Handler, slide bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(a.cfg.SessionCookieName)
		if err != nil || c.Value == "" {
			writeError(w, r, CodeUnauthenticated, "authentication required")
			return
		}
		var sess *Session
		if slide {
			sess, err = a.sessions.Get(r.Context(), c.Value)
		} else {
			sess, err = a.sessions.Peek(r.Context(), c.Value)
		}
		if err != nil {
			switch {
			case errors.Is(err, ErrSessionNotFound):
				writeError(w, r, CodeUnauthenticated, "session missing or expired")
			case store.IsTransient(err):
				// The session store could not answer: not "no session".
				// 503 so the portal retries instead of re-logging in
				// (fail closed, same shape as the session listener).
				writeError(w, r, CodeUnavailable, "session store unavailable")
			default:
				writeError(w, r, CodeInternal, "internal error")
			}
			return
		}
		// Fill the outer audit collector (set by AuditWithSink) — context
		// values flow downward only, so the verified principal reaches the
		// request-audit event through this shared pointer.
		if col, ok := r.Context().Value(ctxKeyAuditCollector).(*auditCollector); ok {
			col.principal = sess.Principal
			col.hasPrincipal = true
		}
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, sess.Principal)
		ctx = context.WithValue(ctx, ctxKeySession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---------------------------------------------------------------------------
// CSRF (synchronizer token)
// ---------------------------------------------------------------------------

var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// RequireCSRF enforces the synchronizer-token check on state-changing
// methods: the request must carry the CSRFHeader value matching the token
// derived from the session ID (csrfTokenFor, P1 — the token the portal
// read from GET /v1/me). Safe methods pass through. Use after RequireAuth
// or RequireAuthPassive so the session is in context; a request that
// reaches here without a session is rejected rather than trusted.
func (a *Authenticator) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethods[r.Method] {
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := SessionFromContext(r.Context())
		if !ok {
			writeError(w, r, CodeUnauthenticated, "authentication required")
			return
		}
		got := r.Header.Get(a.cfg.CSRFHeader)
		if got == "" ||
			subtle.ConstantTimeCompare([]byte(got), []byte(csrfTokenFor(sess.ID))) != 1 {
			writeError(w, r, CodeCSRFFailed, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Origin / Fetch-Metadata defence in depth
// ---------------------------------------------------------------------------

// RequireTrustedOrigin guards cookie-authenticated state-changing requests
// against forged browser provenance (defence in depth alongside the
// synchronizer CSRF token and SameSite=Lax cookie, which alone do not stop a
// caller that holds a valid token from claiming a foreign Origin):
//
//   - safe methods pass through untouched;
//   - requests without the session cookie pass through — they are not
//     cookie-authenticated (RequireAuth rejects them downstream as 401);
//   - an Origin header, when present, must appear exactly once and equal one
//     of allowedOrigins byte-for-byte ("null" and foreign origins are
//     rejected);
//   - with no Origin, a Sec-Fetch-Site header must be "same-origin" — the
//     portal terminates TLS and reverse-proxies /v1 to this API, so portal
//     and API share one origin. A split-origin-but-same-site deployment
//     would need "same-site" here; the Helm chart never produces one;
//   - requests carrying neither header are non-browser clients and pass —
//     RequireCSRF's token check remains their only gate.
//
// Rejections are 403 CSRF_FAILED via the shared error model. Header values
// are never logged. Wire outermost-but-inside-RequestID:
// RequestID(Audit(RequireTrustedOrigin(cookie, origins)(mux))).
func RequireTrustedOrigin(sessionCookieName string, allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o = strings.TrimSuffix(strings.TrimSpace(o), "/"); o != "" {
			allowed[o] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethods[r.Method] {
				next.ServeHTTP(w, r)
				return
			}
			if c, err := r.Cookie(sessionCookieName); err != nil || c.Value == "" {
				next.ServeHTTP(w, r)
				return
			}
			origins := r.Header.Values("Origin")
			switch {
			case len(origins) > 1:
				// Ambiguous provenance — browsers never send two.
				writeError(w, r, CodeCSRFFailed, "cross-origin request rejected")
				return
			case len(origins) == 1 && origins[0] != "":
				if _, ok := allowed[origins[0]]; !ok {
					writeError(w, r, CodeCSRFFailed, "cross-origin request rejected")
					return
				}
			default:
				if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
					writeError(w, r, CodeCSRFFailed, "cross-origin request rejected")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// Rate limiting (E7)
// ---------------------------------------------------------------------------

// RateLimit throttles a route per client key: requests inside the bucket
// pass; over the limit the caller gets 429 RATE_LIMITED with a Retry-After
// in whole seconds and the refusal is counted in
// tinycdi_rate_limited_total under the matched route template (E8). The
// key is the socket peer, or the right-most untrusted X-Forwarded-For
// entry when the peer sits inside trusted — the same derivation the
// session gateway applies (S18). A nil limiter disables the check
// entirely; a nil Metrics skips the count.
func RateLimit(l *ratelimit.Limiter, trusted []netip.Prefix, m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l != nil {
				if ok, retry := l.Allow(ratelimit.ClientKey(r, trusted)); !ok {
					if m != nil {
						m.IncRateLimited(strings.TrimPrefix(r.Pattern, r.Method+" "))
					}
					w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(retry)))
					writeError(w, r, CodeRateLimited, "rate limit exceeded")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// Audit logging
// ---------------------------------------------------------------------------

// statusRecorder captures the response status for audit logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// auditCollector is a request-scoped record placed in the context by
// AuditWithSink; inner middleware (RequireAuth) fills the verified principal
// so the audit event can name the actor without ever touching headers.
type auditCollector struct {
	principal    Principal
	hasPrincipal bool
}

func statusOrOK(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}

func codeClass(status int) string {
	switch {
	case status >= 100 && status < 600:
		return string(rune('0'+status/100)) + "xx"
	default:
		return "unknown"
	}
}

func outcomeFor(status int) observability.AuditOutcome {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return observability.OutcomeDenied
	case status >= 400:
		return observability.OutcomeFailure
	default:
		return observability.OutcomeSuccess
	}
}

// InstrumentHTTP records per-request Prometheus metrics (count + duration)
// labeled by listener, route template, method and code class (E8). A nil
// Metrics passes requests through unobserved. It must wrap the mux
// innermost: it relies on ServeMux setting r.Pattern, which is only readable
// when this middleware passes the same *http.Request down unwrapped.
func InstrumentHTTP(m *observability.Metrics, listener string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(rec, r)
			route := strings.TrimPrefix(r.Pattern, r.Method+" ")
			if route == "" {
				route = "unmatched"
			}
			m.ObserveHTTP(listener, route, r.Method, codeClass(statusOrOK(rec.status)), time.Since(start))
		})
	}
}

// Audit emits one structured audit record per request via slog, plus one
// AuditEvent to sink when non-nil. REDACTION RULES:
//   - never log request/response headers — Cookie, Set-Cookie and
//     Authorization carry session IDs and tokens;
//   - never log the raw query string — the OIDC callback carries code/state;
//   - never log request bodies — they may carry credentials;
//   - the audit actor is the pseudonymous observability.ActorRef
//     (hash of issuer|subject), never the raw subject;
//   - Detail keys are redacted via observability.RedactDetails.
func AuditWithSink(log *slog.Logger, sink observability.AuditSink) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			col := &auditCollector{}
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyAuditCollector, col))
			rec := &statusRecorder{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(rec, r)
			status := statusOrOK(rec.status)
			attrs := []interface{}{
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
			}
			if col.hasPrincipal {
				attrs = append(attrs,
					"actor", observability.ActorRef(col.principal.Issuer, col.principal.Subject),
					"tenant", col.principal.TenantID)
			}
			log.Info("http_request", attrs...)
			if sink != nil {
				var actor, tenant string
				if col.hasPrincipal {
					actor = observability.ActorRef(col.principal.Issuer, col.principal.Subject)
					tenant = col.principal.TenantID
				}
				_ = sink.WriteAudit(r.Context(), observability.AuditEvent{
					Actor:     actorOrAnonymous(actor),
					Action:    "http.request",
					Tenant:    tenant,
					RequestID: RequestIDFromContext(r.Context()),
					Outcome:   outcomeFor(status),
					Details: observability.RedactDetails(map[string]string{
						"method": r.Method,
						"path":   r.URL.Path,
					}),
				})
			}
		})
	}
}

func actorOrAnonymous(actor string) string {
	if actor == "" {
		return "anonymous"
	}
	return actor
}

// Audit is AuditWithSink without an AuditSink — kept for callers that only
// need the slog request record.
func Audit(log *slog.Logger) func(http.Handler) http.Handler {
	return AuditWithSink(log, nil)
}
