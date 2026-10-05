// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// post-restore one-shot mode (`backend post-restore`): the deterministic
// cleanup that docs/runbooks/disaster-recovery.md §"Postgres-only restore
// onto a live cluster" requires after loading an older pg_dump while the
// cluster kept running. It exists because the procedure is several steps
// and none of them may be skipped:
//
//   - rotate platform_meta.session_epoch — every restored session row is
//     dead on next read, including sessions revoked after the dump (S17);
//   - revoke every 'active' connection_lease — restored leases resurrect
//     with live session_digest bindings and no portal-session barrier;
//   - deny every unconsumed launch_ticket;
//   - align workspaces rows that trail their live CR
//     (desired_state/runtime_generation/intent_revision) so the first
//     post-restore intent is not silently dropped by the operator's
//     applied-intent fence.
//
// The default is a dry-run that prints the plan and the affected row
// counts; -apply executes. -apply requires -i-have-scaled-down, then takes
// the leader advisory lock on a dedicated connection and holds it for the
// whole run: pg_try_advisory_lock is atomic (no probe-then-act window —
// a serving replica or a second post-restore run already holding it means
// refusal with zero writes), and while the tool holds it no replica scaled
// up mid-apply can elect a leader. It also refuses while backend
// connections remain in pg_stat_activity (non-leader replicas can still
// create sessions and renew leases) unless -i-know-backends-are-running
// overrides. The acknowledgement flag is the operator's statement that the
// platform is frozen — a Job cannot distinguish scaled-to-zero from
// serving replicas — and the held lock is the hard backstop because any
// serving replica set elects a leader within leaderRetryInterval.
//
// The CR-alignment phase needs Kubernetes read access (a kubeconfig, or an
// in-cluster ServiceAccount token — a Job may run with
// automountServiceAccountToken=false and still perform every DB step).
// Without it the rows cannot be compared to live CRs, so the tool prints
// the per-workspace UPDATE statement with the kubectl jsonpath that fills
// each value, and exits non-zero: automation must notice the incomplete
// run.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// postRestoreExitKubeUnavailable is the exit code when the CR-alignment
// phase cannot read Workspace CRs. The runbook's SQL is printed instead.
const postRestoreExitKubeUnavailable = 3

const postRestoreUsage = `usage: backend post-restore [-apply] [-i-have-scaled-down] [flags]

Runs the deterministic post-restore cleanup for a Postgres dump loaded
onto a live cluster (docs/runbooks/disaster-recovery.md, "Postgres-only
restore onto a live cluster"). The default is a dry-run: it prints the
plan and the affected row counts and writes nothing.

  -apply               execute the steps (default: dry-run)
  -dry-run             explicit dry-run; mutually exclusive with -apply
  -i-have-scaled-down  required with -apply: acknowledges backend and
                       operator are scaled to 0
  -i-know-backends-are-running
                       UNSUPPORTED, dangerous: with -apply, proceed even
                       though backend connections are still present in
                       pg_stat_activity — serving replicas can create
                       sessions and renew leases mid-apply
  -database-url        PostgreSQL DSN (env TCDI_DATABASE_URL)
  -dev-insecure-db     allow a non-verifying database sslmode (env
                       TCDI_DEV_INSECURE_DB; local development only)
  -kubeconfig          kubeconfig path for the CR read (env KUBECONFIG;
                       default: in-cluster)
  -tenant-namespaces   tenant=namespace pairs, comma-separated — the
                       namespaces Workspace CRs are listed in (env
                       TCDI_TENANT_NAMESPACES; empty lists cluster-wide)

exit codes: 0 done/plan printed, 1 refused or failed, 2 usage/config,
3 Kubernetes read unavailable — per-workspace SQL printed instead.
`

type postRestoreConfig struct {
	databaseURL      string
	devInsecureDB    bool
	kubeconfig       string
	tenantNamespaces string
	apply            bool
	dryRun           bool
	scaledDownAck    bool
	backendsRunning  bool
}

func parsePostRestoreFlags(args []string, getenv func(string) string) (postRestoreConfig, error) {
	var c postRestoreConfig
	fs := flag.NewFlagSet("post-restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.databaseURL, "database-url", envOr(getenv, "TCDI_DATABASE_URL", ""), "PostgreSQL DSN")
	fs.BoolVar(&c.devInsecureDB, "dev-insecure-db", envOr(getenv, "TCDI_DEV_INSECURE_DB", "") == "true", "allow non-verifying PostgreSQL sslmode")
	fs.StringVar(&c.kubeconfig, "kubeconfig", envOr(getenv, "KUBECONFIG", ""), "kubeconfig path (default: in-cluster)")
	fs.StringVar(&c.tenantNamespaces, "tenant-namespaces", envOr(getenv, "TCDI_TENANT_NAMESPACES", ""), "tenant=namespace pairs")
	fs.BoolVar(&c.apply, "apply", false, "execute")
	fs.BoolVar(&c.dryRun, "dry-run", false, "plan only (default)")
	fs.BoolVar(&c.scaledDownAck, "i-have-scaled-down", false, "freeze acknowledgement")
	fs.BoolVar(&c.backendsRunning, "i-know-backends-are-running", false, "UNSUPPORTED: apply despite live backend connections")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected positional args: %s", strings.Join(fs.Args(), " "))
	}
	switch {
	case c.apply && c.dryRun:
		return c, errors.New("-apply and -dry-run are mutually exclusive")
	case c.apply && !c.scaledDownAck:
		return c, errors.New("-apply requires -i-have-scaled-down: post-restore runs only while backend and operator are scaled to 0")
	case !c.apply && c.scaledDownAck:
		return c, errors.New("-i-have-scaled-down only applies to -apply")
	case !c.apply && c.backendsRunning:
		return c, errors.New("-i-know-backends-are-running only applies to -apply")
	}
	if c.databaseURL == "" {
		return c, errors.New("required: -database-url (env TCDI_DATABASE_URL)")
	}
	return c, nil
}

// PostRestoreMain is the `backend post-restore` entry point. It returns
// the process exit code; the plan and the fallback SQL go to out, errors
// to errOut.
func PostRestoreMain(ctx context.Context, args []string, getenv func(string) string, out, errOut io.Writer, log *slog.Logger) int {
	cfg, err := parsePostRestoreFlags(args, getenv)
	if err != nil {
		fmt.Fprintf(errOut, "post-restore: %v\n\n%s", err, postRestoreUsage)
		return 2
	}
	mode := "dry-run"
	if cfg.apply {
		mode = "apply"
	}
	if _, err := checkDatabaseTLS(cfg.databaseURL, cfg.devInsecureDB); err != nil {
		fmt.Fprintf(errOut, "post-restore: %v\n", err)
		return 2
	}
	// The tool's own connections carry a distinct application_name so the
	// backend-connection guard never counts this run.
	db, err := store.OpenWithAppName(ctx, cfg.databaseURL, postRestoreAppName)
	if err != nil {
		fmt.Fprintf(errOut, "post-restore: %v\n", err)
		return 1
	}
	defer db.Close()

	tenants, err := parseTenants(cfg.tenantNamespaces)
	if err != nil {
		fmt.Fprintf(errOut, "post-restore: -tenant-namespaces: %v\n", err)
		return 2
	}
	src, srcErr := newLiveCRSource(cfg.kubeconfig, tenants)
	return postRestore(ctx, db, src, srcErr, tenants, cfg.apply, cfg.backendsRunning, out, errOut, log.With("mode", mode))
}

// workspaceRow is the restored DB side of a workspace.
type workspaceRow struct {
	id, tenantID      string
	desiredState      string
	runtimeGeneration int64
	intentRevision    int64
}

// liveCR is the live-cluster side of a workspace: the spec fields the row
// must describe after the restore. spec.intentRevision is the alignment
// target — it is the newest submitted intent and always >= the recorded
// applied-intent revision, so aligning to it also clears the operator's
// drop fence and covers an intent still pending at freeze time.
type liveCR struct {
	uid               string // platform workspace id (ws_<hex>)
	namespace, name   string
	desiredState      string
	runtimeGeneration int64
	intentRevision    int64
}

// liveCRSource enumerates the Workspace CRs the restore must agree with.
type liveCRSource interface {
	List(ctx context.Context) ([]liveCR, error)
}

// kubeCRSource lists Workspace CRs through the API server — per tenant
// namespace when namespaces is non-empty (the backend ServiceAccount's
// RoleBindings reach exactly those), cluster-wide otherwise.
type kubeCRSource struct {
	c          client.Client
	namespaces []string
}

func newLiveCRSource(kubeconfig string, tenants provisioning.TenantNamespaces) (liveCRSource, error) {
	rcfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	kc, err := client.New(rcfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	seen := map[string]bool{}
	var namespaces []string
	for _, ns := range tenants {
		if !seen[ns] {
			seen[ns] = true
			namespaces = append(namespaces, ns)
		}
	}
	sort.Strings(namespaces)
	return &kubeCRSource{c: kc, namespaces: namespaces}, nil
}

func (s *kubeCRSource) List(ctx context.Context) ([]liveCR, error) {
	list := func(ns string) (*workspacev1alpha1.WorkspaceList, error) {
		l := &workspacev1alpha1.WorkspaceList{}
		opts := []client.ListOption{}
		if ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := s.c.List(ctx, l, opts...); err != nil {
			return nil, err
		}
		return l, nil
	}
	var out []liveCR
	if len(s.namespaces) == 0 {
		l, err := list("")
		if err != nil {
			return nil, err
		}
		for _, ws := range l.Items {
			out = append(out, liveCRFromWorkspace(&ws))
		}
		return out, nil
	}
	for _, ns := range s.namespaces {
		l, err := list(ns)
		if err != nil {
			return nil, err
		}
		for _, ws := range l.Items {
			out = append(out, liveCRFromWorkspace(&ws))
		}
	}
	return out, nil
}

// liveCRFromWorkspace projects a Workspace CR. The platform uid comes from
// the workspace-uid label the applier stamps; a CR without it still counts
// as an orphan candidate via its name.
func liveCRFromWorkspace(ws *workspacev1alpha1.Workspace) liveCR {
	uid := ws.Labels[provisioning.LabelWorkspaceUID]
	if uid == "" {
		uid = "ws_" + strings.TrimPrefix(ws.Name, "ws-")
	}
	// The intent fence floor is spec.intentRevision: the applied-intent
	// annotation records the newest ADOPTED intent, which can lag a spec
	// the operator had not yet applied when the freeze hit.
	rev := ws.Spec.IntentRevision
	if raw := ws.Annotations[operator.AnnotationAppliedIntent]; raw != "" {
		var applied operator.AppliedIntent
		if err := json.Unmarshal([]byte(raw), &applied); err == nil && applied.Revision > rev {
			rev = applied.Revision
		}
	}
	return liveCR{
		uid:               uid,
		namespace:         ws.Namespace,
		name:              ws.Name,
		desiredState:      string(ws.Spec.DesiredState),
		runtimeGeneration: ws.Spec.RuntimeGeneration,
		intentRevision:    rev,
	}
}

// postRestore runs the whole mode and returns the exit code.
func postRestore(ctx context.Context, db *store.DB, src liveCRSource, srcErr error,
	tenants provisioning.TenantNamespaces, apply, backendsRunning bool, out, errOut io.Writer, log *slog.Logger) int {

	// Freeze guard. Dry-run only probes (it may run while the platform is
	// still serving). -apply takes the leader lock itself and holds it for
	// the entire run — a try-lock is atomic, so there is no window where a
	// replica scaled up between a probe and the first write can elect a
	// leader, and a second post-restore run cannot start either.
	var held bool
	var backendConns int64
	if apply {
		lockConn, err := takeLeaderLock(ctx, db)
		if errors.Is(err, errLeaderLockHeld) {
			fmt.Fprintln(errOut, "post-restore: refusing: the leader lock is held — a backend "+
				"replica (or another post-restore run) is still active; scale backend and "+
				"operator to 0 before running the post-restore steps")
			return 1
		}
		if err != nil {
			fmt.Fprintf(errOut, "post-restore: %v\n", err)
			return 1
		}
		defer releaseLeaderLock(ctx, lockConn)

		// A replica that has not (yet) elected a leader can still serve
		// logins, create sessions and renew leases — refuse while any of
		// its connections remain.
		backendConns, err = backendConnCount(ctx, db)
		if err != nil {
			fmt.Fprintf(errOut, "post-restore: backend connection probe: %v\n", err)
			return 1
		}
		if backendConns > 0 && !backendsRunning {
			fmt.Fprintf(errOut, "post-restore: refusing: %d backend connection(s) present in "+
				"pg_stat_activity (application_name LIKE %q) — scale backend to 0 first, or pass "+
				"-i-know-backends-are-running (UNSUPPORTED) to override\n", backendConns, backendAppName+"%")
			return 1
		}
		if backendConns > 0 {
			fmt.Fprintf(out, "  WARNING: -i-know-backends-are-running: applying with %d live "+
				"backend connection(s) — they can create sessions and renew leases mid-apply\n",
				backendConns)
		}
	} else {
		var err error
		if held, err = leaderLockHeld(ctx, db); err != nil {
			fmt.Fprintf(errOut, "post-restore: leader lock probe: %v\n", err)
			return 1
		}
		if backendConns, err = backendConnCount(ctx, db); err != nil {
			fmt.Fprintf(errOut, "post-restore: backend connection probe: %v\n", err)
			return 1
		}
	}

	if apply {
		// Idempotent, versioned — fills any schema gap between the dump and
		// this binary before the statements below run.
		if err := db.Migrate(ctx); err != nil {
			fmt.Fprintf(errOut, "post-restore: migrate: %v\n", err)
			return 1
		}
	}

	p, err := gatherPostRestorePlan(ctx, db)
	if err != nil {
		fmt.Fprintf(errOut, "post-restore: %v\n", err)
		return 1
	}

	if apply {
		fmt.Fprintln(out, "post-restore plan (apply):")
	} else {
		fmt.Fprintln(out, "post-restore plan (dry-run — no writes):")
	}
	fmt.Fprintf(out, "  sessions to rotate (session_epoch): %d\n", p.sessions)
	fmt.Fprintf(out, "  active leases to revoke:            %d\n", p.activeLeases)
	fmt.Fprintf(out, "  unconsumed tickets to deny:         %d\n", p.unconsumedTickets)
	fmt.Fprintf(out, "  workspace rows (state='active'):    %d\n", len(p.rows))
	if held {
		fmt.Fprintln(out, "  NOTE: a backend leader lock is currently held — the platform does not look frozen")
	}
	if backendConns > 0 {
		fmt.Fprintf(out, "  NOTE: %d backend connection(s) present in pg_stat_activity — the platform does not look frozen\n",
			backendConns)
	}

	var rotated, revokedLeases, deniedTickets int64
	if apply {
		if err := db.RotateSessionEpoch(ctx); err != nil {
			fmt.Fprintf(errOut, "post-restore: session epoch rotation: %v\n", err)
			return 1
		}
		rotated = p.sessions
		if revokedLeases, err = execCount(ctx, db,
			`UPDATE connection_lease SET state = 'revoked', closed_at = now() WHERE state = 'active'`); err != nil {
			fmt.Fprintf(errOut, "post-restore: revoke leases: %v\n", err)
			return 1
		}
		if deniedTickets, err = execCount(ctx, db,
			`UPDATE launch_ticket SET revoked_at = now() WHERE consumed_at IS NULL AND revoked_at IS NULL`); err != nil {
			fmt.Fprintf(errOut, "post-restore: deny tickets: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "  applied: rotated session_epoch, revoked %d leases, denied %d tickets\n",
			revokedLeases, deniedTickets)
	}

	// CR alignment — the only phase that needs Kubernetes.
	var crs []liveCR
	if srcErr == nil && src != nil {
		crs, srcErr = src.List(ctx)
	}
	if srcErr != nil {
		fmt.Fprintf(out, "post-restore: no Kubernetes read access (%v) — cannot compare workspace "+
			"rows to live CRs.\n", srcErr)
		printAlignmentSQL(out, p.rows, tenants)
		log.Info("post-restore", "sessions_rotated", rotated, "leases_revoked", revokedLeases,
			"tickets_revoked", deniedTickets, "cr_alignment", "skipped-no-kube-access",
			"workspace_rows", len(p.rows))
		return postRestoreExitKubeUnavailable
	}

	recon := reconcilePlan(p.rows, crs, tenants)
	for _, line := range recon.detail {
		fmt.Fprintf(out, "  %s\n", line)
	}
	fmt.Fprintf(out, "  workspaces vs live CRs: %d rows behind, %d ghost rows (no CR), %d orphan CRs (no row)\n",
		len(recon.diverged), len(recon.ghosts), len(recon.orphans))

	var aligned int64
	if apply {
		for _, d := range recon.diverged {
			n, err := execCount(ctx, db, `
				UPDATE workspaces
				   SET desired_state = $2, runtime_generation = $3, intent_revision = $4
				 WHERE id = $1 AND state = 'active' AND intent_revision <= $4
				   AND (intent_revision < $4 OR desired_state <> $2 OR runtime_generation <> $3)`,
				d.row.id, d.cr.desiredState, d.cr.runtimeGeneration, d.cr.intentRevision)
			if err != nil {
				fmt.Fprintf(errOut, "post-restore: align %s: %v\n", d.row.id, err)
				return 1
			}
			aligned += n
		}
		fmt.Fprintf(out, "  applied: aligned %d workspace rows to their live CRs\n", aligned)
	}
	log.Info("post-restore", "sessions_rotated", rotated, "leases_revoked", revokedLeases,
		"tickets_revoked", deniedTickets, "intents_aligned", aligned,
		"rows_behind", len(recon.diverged), "ghost_rows", len(recon.ghosts),
		"orphan_crs", len(recon.orphans))
	return 0
}

// postRestorePlan holds the counts the dry-run prints and the rows the
// CR-alignment phase works on.
type postRestorePlan struct {
	sessions          int64
	activeLeases      int64
	unconsumedTickets int64
	rows              []workspaceRow
}

func gatherPostRestorePlan(ctx context.Context, db *store.DB) (*postRestorePlan, error) {
	var p postRestorePlan
	for dst, q := range map[*int64]string{
		// Every row dies at the new epoch, revoked-after-dump or not.
		&p.sessions:          `SELECT count(*) FROM sessions`,
		&p.activeLeases:      `SELECT count(*) FROM connection_lease WHERE state = 'active'`,
		&p.unconsumedTickets: `SELECT count(*) FROM launch_ticket WHERE consumed_at IS NULL AND revoked_at IS NULL`,
	} {
		if err := db.Pool().QueryRow(ctx, q).Scan(dst); err != nil {
			return nil, fmt.Errorf("count: %w", err)
		}
	}
	rrows, err := db.Pool().Query(ctx, `
		SELECT id, tenant_id, desired_state, runtime_generation, intent_revision
		  FROM workspaces WHERE state = 'active' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var r workspaceRow
		if err := rrows.Scan(&r.id, &r.tenantID, &r.desiredState, &r.runtimeGeneration, &r.intentRevision); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		p.rows = append(p.rows, r)
	}
	if err := rrows.Err(); err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return &p, nil
}

// divergence pairs a row with the live CR it trails.
type divergence struct {
	row workspaceRow
	cr  liveCR
}

// reconcileResult is the workspaces↔CR comparison: diverged rows (the CR
// is ahead — aligned on apply), ghosts (row without CR), orphans (CR
// without row) and anomalies (row ahead of its CR — reported, never
// rewritten downwards).
type reconcileResult struct {
	diverged []divergence
	ghosts   []workspaceRow
	orphans  []liveCR
	detail   []string
}

func reconcilePlan(rows []workspaceRow, crs []liveCR, tenants provisioning.TenantNamespaces) reconcileResult {
	byUID := map[string]liveCR{}
	for _, cr := range crs {
		byUID[cr.uid] = cr
	}
	seen := map[string]bool{}
	var r reconcileResult
	for _, row := range rows {
		// A per-namespace list cannot see a CR whose tenant has no mapping.
		if len(tenants) > 0 {
			if _, ok := tenants.Namespace(row.tenantID); !ok {
				r.detail = append(r.detail, fmt.Sprintf(
					"unchecked %s: tenant %s has no -tenant-namespaces mapping — its CR was not inspected",
					row.id, row.tenantID))
				continue
			}
		}
		cr, ok := byUID[row.id]
		if !ok {
			r.ghosts = append(r.ghosts, row)
			r.detail = append(r.detail, fmt.Sprintf(
				"ghost row %s (tenant %s): no live CR — re-delete via the API or mark state='deleted' (runbook step 5)",
				row.id, row.tenantID))
			continue
		}
		seen[cr.uid] = true
		if row.intentRevision > cr.intentRevision {
			r.detail = append(r.detail, fmt.Sprintf(
				"anomaly %s: row intent_revision %d is AHEAD of CR %s/%s spec.intentRevision %d — inspect by hand, not touched",
				row.id, row.intentRevision, cr.namespace, cr.name, cr.intentRevision))
			continue
		}
		if row.intentRevision < cr.intentRevision ||
			row.desiredState != cr.desiredState ||
			row.runtimeGeneration != cr.runtimeGeneration {
			r.diverged = append(r.diverged, divergence{row: row, cr: cr})
			r.detail = append(r.detail, fmt.Sprintf(
				"behind %s: intent_revision %d -> %d, desired %s -> %s, generation %d -> %d",
				row.id, row.intentRevision, cr.intentRevision,
				row.desiredState, cr.desiredState,
				row.runtimeGeneration, cr.runtimeGeneration))
		}
	}
	for _, cr := range crs {
		if !seen[cr.uid] {
			r.orphans = append(r.orphans, cr)
			r.detail = append(r.detail, fmt.Sprintf(
				"orphan CR %s/%s (uid %s): no workspaces row — created after the dump; delete per runbook step 5",
				cr.namespace, cr.name, cr.uid))
		}
	}
	return r
}

// printAlignmentSQL emits the per-workspace UPDATE the operator runs by
// hand when the Job could not read CRs — each value the CR must supply is
// named by the kubectl jsonpath that produces it.
func printAlignmentSQL(out io.Writer, rows []workspaceRow, tenants provisioning.TenantNamespaces) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "post-restore: no active workspace rows — no CR alignment needed.")
		return
	}
	fmt.Fprintln(out, "post-restore: run this per active workspace row after reading the live CR "+
		"(the intent_revision must reach spec.intentRevision — also recorded in the "+
		"workspaces.cdi.tinyorbit.vn/applied-intent annotation):")
	for _, r := range rows {
		name := provisioning.WorkspaceCRName(provisioning.PlatformID(r.id))
		ns, ok := tenants.Namespace(r.tenantID)
		if !ok {
			ns = "<tenant-namespace>"
		}
		fmt.Fprintf(out, `
-- %s (tenant %s):
--   kubectl -n %s get workspace %s \
--     -o jsonpath='{.spec.desiredState}{"\t"}{.spec.runtimeGeneration}{"\t"}{.spec.intentRevision}'
UPDATE workspaces
   SET desired_state = '<spec.desiredState>',
       runtime_generation = <spec.runtimeGeneration>,
       intent_revision = <spec.intentRevision>
 WHERE id = '%s' AND state = 'active';
`, r.id, r.tenantID, ns, name, r.id)
	}
}

// postRestoreAppName is the application_name the tool stamps on its own
// connections so the backend-connection guard never counts this run.
const postRestoreAppName = "tcdi-post-restore"

// errLeaderLockHeld means the try-lock came back false: another session —
// a serving backend replica or a concurrent post-restore run — holds the
// leader advisory lock.
var errLeaderLockHeld = errors.New("leader lock already held")

// takeLeaderLock grabs the leader advisory lock on its own dedicated
// connection, reusing the backend's keepalive settings. The lock is
// session-scoped, so the caller holds it for the whole run simply by
// keeping the connection open; releaseLeaderLock (or the kernel, if the
// process dies) releases it.
func takeLeaderLock(ctx context.Context, db *store.DB) (*pgx.Conn, error) {
	cfg := leaderConnConfig(db)
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = postRestoreAppName
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("leader lock connect: %w", err)
	}
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLockKey).Scan(&held); err != nil {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("leader lock: %w", err)
	}
	if !held {
		_ = conn.Close(context.Background())
		return nil, errLeaderLockHeld
	}
	return conn, nil
}

// releaseLeaderLock unlocks and closes the dedicated lock connection —
// closing alone would drop the session lock, but the explicit unlock makes
// the release ordered and visible in pg_locks before the socket goes away.
// It runs on a fresh context so an already-cancelled run still releases.
func releaseLeaderLock(ctx context.Context, conn *pgx.Conn) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaderConnCloseTimeout)
	defer cancel()
	_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, leaderLockKey)
	_ = conn.Close(uctx)
}

// backendConnCount reports how many live backend connections this database
// shows — serving replicas are identifiable because the backend stamps
// application_name on every pooled connection (wireMerged). The match is a
// prefix: OpenWithAppName composes a DSN-provided name as
// "tcdi-backend/<dsn-name>", and those connections are serving
// connections too. The tool's own application_name (tcdi-post-restore)
// doesn't share the prefix, and the calling connection is also excluded
// by pid.
func backendConnCount(ctx context.Context, db *store.DB) (int64, error) {
	var n int64
	err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database()
		   AND application_name LIKE $1
		   AND pid <> pg_backend_pid()`, backendAppName+"%").Scan(&n)
	return n, err
}

// leaderLockHeld reports whether any backend replica currently holds the
// leader advisory lock — the signal that the platform is still serving.
func leaderLockHeld(ctx context.Context, db *store.DB) (bool, error) {
	var held bool
	err := db.Pool().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			 WHERE locktype = 'advisory' AND granted AND objsubid = 1
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			   AND ((classid::bigint << 32) | objid::bigint) = $1)`, leaderLockKey).Scan(&held)
	return held, err
}

func execCount(ctx context.Context, db *store.DB, query string, args ...any) (int64, error) {
	tag, err := db.Pool().Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
