#!/bin/sh
# TinyCDI node-profile installer — one shot per node.
#
# Runs as the privileged initContainer of the node-profiles DaemonSet
# (dedicated PSS-privileged namespace, hostPID; the long-running verifier
# container is uid 0 with every capability dropped — the least privilege
# that can still read the host's AppArmor loaded list, CHTR-1).
# NODE_PROFILES_ONESHOT=1 makes it exit after
# the install — required for initContainer semantics. For each node it:
#   * refuses (clear log, pod stays NotReady) when the host lacks AppArmor
#     (no loaded-profiles file) or apparmor_parser;
#   * resolves the AppArmor template's `signal (receive) peer=<daemon>`
#     placeholder to the node containerd's own AppArmor label (read from
#     /proc/<containerd>/attr/current — "unconfined" when unconfined),
#     exactly as the proven trial did;
#   * compile-checks the rendered profile with the HOST's apparmor_parser
#     (nsenter into PID 1's mount namespace, `apparmor_parser -Q -K`),
#     installs it atomically into host /etc/apparmor.d/ (temp + rename) and
#     loads it with `apparmor_parser -r -K` — it therefore survives reboots
#     (the host's apparmor init reloads /etc/apparmor.d at boot, and this
#     pod re-runs after every restart anyway);
#   * writes the seccomp profile to <kubeletRoot>/seccomp/profiles/
#     chromium-userns.json atomically (temp + rename, sha256-verified);
#   * then exits (ONESHOOT, initContainer) or idles; the readinessProbe
#     (verify.sh, exec'd by the unprivileged verifier) reports Ready only
#     while the installed state still holds.
#
# NODE_PROFILES_MODE=remove reverses it: unloads the AppArmor profile with
# the host parser and deletes both files (idempotent), then idles.
#
# Environment seams — defaults are the production values. The offline
# harness (deploy/helm/proof/node-profiles) overrides them to point at a
# simulated host root; the DaemonSet also sets NODE_PROFILES_ONESHOT/MODE
# here and the direct-mode paths on the unprivileged verifier.
#   NODE_PROFILES_MODE            install | remove   (default: install)
#   NODE_PROFILES_PROFILE_DIR     source dir         (default: /profiles)
#   NODE_PROFILES_SECCOMP_DIR     mounted host <kubeletRoot>/seccomp/profiles
#                                 (default: /host/seccomp-profiles)
#   NODE_PROFILES_APPARMOR_DIR    mounted /etc/apparmor.d (default: /host/apparmor.d)
#   NODE_PROFILES_APPARMOR_HOST_DIR  same dir, host path (default: /etc/apparmor.d)
#   NODE_PROFILES_PROC            host /proc (hostPID) (default: /proc)
#   NODE_PROFILES_AA_SYSFS        loaded-profiles file on the HOST
#                                 (default: /sys/kernel/security/apparmor/profiles)
#   NODE_PROFILES_NSENTER         host-exec prefix; empty = run directly
#                                 (default: nsenter -t 1 -m --)
#   NODE_PROFILES_ONESHOT         when set, exit instead of idling
set -eu

MODE=${NODE_PROFILES_MODE:-install}
SRC=${NODE_PROFILES_PROFILE_DIR:-/profiles}
SEC_DIR=${NODE_PROFILES_SECCOMP_DIR:-/host/seccomp-profiles}
AA_DIR=${NODE_PROFILES_APPARMOR_DIR:-/host/apparmor.d}
AA_HOST_DIR=${NODE_PROFILES_APPARMOR_HOST_DIR:-/etc/apparmor.d}
PROC_DIR=${NODE_PROFILES_PROC:-/proc}
AA_SYSFS=${NODE_PROFILES_AA_SYSFS:-/sys/kernel/security/apparmor/profiles}
NSENTER=${NODE_PROFILES_NSENTER-nsenter -t 1 -m --}
ONESHOT=${NODE_PROFILES_ONESHOT:-}

SECCOMP_NAME=chromium-userns.json
AA_NAME=tinycdi-browser
NODE=${NODE_NAME:-unknown}
STATUS_DIR=/run/node-profiles
STATUS=$STATUS_DIR/status

SEC_FILE=$SEC_DIR/$SECCOMP_NAME
AA_FILE=$AA_DIR/$AA_NAME
AA_HOST_FILE=$AA_HOST_DIR/$AA_NAME
AA_TMP=".$AA_NAME.tmp.$$"
SEC_TMP=".$SECCOMP_NAME.tmp.$$"
STUB=".$AA_NAME.remove.$$"

log() { echo "[node-profiles][$MODE][node:$NODE] $*"; }
state() { if [ -d "$STATUS_DIR" ]; then printf 'state=%s node=%s %s\n' "$1" "$NODE" "$2" >"$STATUS" 2>/dev/null || true; fi; }
fail() { log "ERROR: $*" >&2; state error "msg=$*"; exit 1; }
refuse() { log "REFUSE: $*" >&2; state refused "msg=$*"; exit 1; }

cleanup() { rm -f "$AA_DIR/$AA_TMP" "$SEC_DIR/$SEC_TMP" "$AA_DIR/$STUB" 2>/dev/null || true; }
trap cleanup EXIT
mkdir -p "$STATUS_DIR" 2>/dev/null || true
mkdir -p "$AA_DIR" 2>/dev/null || true

# Run a command against the host: nsenter into PID 1's mount namespace so
# paths resolve on the host filesystem and the HOST's binaries run.
host() {
  if [ -n "$NSENTER" ]; then
    # deliberate word-splitting: NSENTER is a multi-word prefix
    $NSENTER "$@"
  else
    "$@"
  fi
}
hostsh() { host sh -c "$1"; }

aa_enabled()   { hostsh "test -r '$AA_SYSFS'"; }
aa_parser()    { hostsh "command -v apparmor_parser >/dev/null 2>&1"; }
aa_is_loaded() { hostsh "grep -q '^$AA_NAME ' '$AA_SYSFS' 2>/dev/null"; }

finish() {
  state "$1" "$2"
  log "$2"
  [ -n "$ONESHOT" ] && exit 0
  log "idling — readiness reported by verify.sh"
  exec sleep infinity
}

# --- containerd daemon AppArmor label discovery (ported from the trial) ---
# The profile's `signal (receive) peer=<daemon>` placeholder must carry the
# label of the node's containerd process; an unconfined containerd maps to
# the literal `unconfined`.
#
# SEC-37: /proc/<pid>/comm alone is not enough — comm is just 15 chars of
# free-form task name. A comm match is only a CANDIDATE: the daemon is
# verified via /proc/<pid>/exe (symlink basename == containerd), falling
# back to argv[0] of /proc/<pid>/cmdline when exe is unreadable. Anything
# that fails verification (e.g. a renamed helper, a containerd-shim that
# managed to set comm=containerd) is skipped; refusing on zero matches is
# fail-closed.
#
# CHTR-4: comm+exe is still not enough — a TENANT workload can run a real
# containerd binary inside its own container (spoofing exe too, and its
# AppArmor label is the container's, not the daemon's). With hostPID, /proc
# is the host's procfs and /proc/1 is host init: only a candidate whose
# pid AND mount namespaces equal PID 1's is a genuine host process.
daemon_profile() {
  ref_pid=$(readlink "$PROC_DIR/1/ns/pid" 2>/dev/null)
  ref_mnt=$(readlink "$PROC_DIR/1/ns/mnt" 2>/dev/null)
  [ -n "$ref_pid" ] && [ -n "$ref_mnt" ] \
    || refuse "cannot resolve $PROC_DIR/1/ns/{pid,mnt} — host-namespace reference is required to identify containerd"
  pid=""
  for c in "$PROC_DIR"/[0-9]*/comm; do
    [ "$(cat "$c" 2>/dev/null)" = "containerd" ] || continue
    cand=${c%/comm}; cand=${cand##*/}
    exe=$(readlink "$PROC_DIR/$cand/exe" 2>/dev/null)
    if [ -n "$exe" ]; then
      [ "${exe##*/}" = "containerd" ] || continue
    else
      # exe unreadable — require argv[0]'s basename to be containerd.
      arg0=$(tr '\0' '\n' <"$PROC_DIR/$cand/cmdline" 2>/dev/null | head -1)
      [ -n "$arg0" ] || continue
      [ "${arg0##*/}" = "containerd" ] || continue
    fi
    # host processes only — skip a containerized imposter (CHTR-4).
    [ "$(readlink "$PROC_DIR/$cand/ns/pid" 2>/dev/null)" = "$ref_pid" ] || continue
    [ "$(readlink "$PROC_DIR/$cand/ns/mnt" 2>/dev/null)" = "$ref_mnt" ] || continue
    pid=$cand; break
  done
  [ -n "$pid" ] || refuse "containerd process not found — the browser AppArmor profile targets containerd-managed pods"
  attr=$(cat "$PROC_DIR/$pid/attr/current" 2>/dev/null) \
    || refuse "cannot read /proc/$pid/attr/current (containerd AppArmor label)"
  [ -n "$attr" ] || refuse "empty AppArmor label for containerd pid $pid"
  daemon=${attr%% *}    # strip " (enforce)" etc.
  case "$daemon" in
    ""|*[!A-Za-z0-9._-]*) refuse "unexpected containerd AppArmor label '$attr'" ;;
  esac
  echo "$daemon"
}

install_profiles() {
  [ -r "$SRC/$SECCOMP_NAME" ] || fail "source $SRC/$SECCOMP_NAME missing"
  [ -r "$SRC/$AA_NAME" ]      || fail "source $SRC/$AA_NAME missing"
  aa_enabled || refuse "AppArmor not enabled on this node ($AA_SYSFS missing) — the browser sandbox needs it"
  aa_parser  || refuse "apparmor_parser not found on this node (install apparmor-utils / apparmor-parser)"

  daemon=$(daemon_profile)
  log "containerd AppArmor label: $daemon"

  # --- AppArmor: render -> host compile-check -> atomic install -> load ---
  sed "s|peer=cri-containerd|peer=$daemon|g" "$SRC/$AA_NAME" >"$AA_DIR/$AA_TMP" \
    || fail "render into $AA_DIR/$AA_TMP failed"
  chmod 0644 "$AA_DIR/$AA_TMP"
  grep -q "profile $AA_NAME " "$AA_DIR/$AA_TMP" || fail "rendered profile lost 'profile $AA_NAME' declaration"
  grep -q "peer=$daemon" "$AA_DIR/$AA_TMP"      || fail "daemon peer substitution missing ($daemon)"
  grep -q 'peer=cri-containerd,' "$AA_DIR/$AA_TMP" && fail "unsubstituted daemon placeholder remains"
  # compile-check via the HOST parser, before the final path is touched
  hostsh "apparmor_parser -Q -K '$AA_HOST_DIR/$AA_TMP'" \
    || fail "rendered AppArmor profile fails the host parser compile check"
  if [ -f "$AA_FILE" ] && cmp -s "$AA_DIR/$AA_TMP" "$AA_FILE"; then
    rm -f "$AA_DIR/$AA_TMP"
    log "apparmor file unchanged ($AA_HOST_FILE)"
  else
    mv -f "$AA_DIR/$AA_TMP" "$AA_FILE" || fail "install $AA_FILE failed"
    log "installed $AA_HOST_FILE"
  fi
  # load/replace in the kernel with the HOST parser; -K skips the cache
  hostsh "apparmor_parser -r -K '$AA_HOST_FILE'" || fail "apparmor_parser -r failed on host"
  aa_is_loaded || fail "$AA_NAME not present in $AA_SYSFS after load"
  log "apparmor profile loaded: $AA_NAME (enforce)"

  # --- seccomp: atomic temp+rename under <kubeletRoot>/seccomp/profiles ---
  mkdir -p "$SEC_DIR" || fail "cannot create $SEC_DIR"
  src_sha=$(sha256sum "$SRC/$SECCOMP_NAME" | awk '{print $1}')
  [ -n "$src_sha" ] || fail "cannot hash $SRC/$SECCOMP_NAME"
  if [ -f "$SEC_FILE" ] && [ "$(sha256sum "$SEC_FILE" | awk '{print $1}')" = "$src_sha" ]; then
    log "seccomp file unchanged ($SEC_FILE, sha256=$src_sha)"
  else
    cat "$SRC/$SECCOMP_NAME" >"$SEC_DIR/$SEC_TMP" || fail "write $SEC_DIR/$SEC_TMP failed"
    chmod 0644 "$SEC_DIR/$SEC_TMP"
    echo "$src_sha  $SEC_DIR/$SEC_TMP" | sha256sum -c - >/dev/null || fail "staged seccomp sha256 mismatch"
    mv -f "$SEC_DIR/$SEC_TMP" "$SEC_FILE" || fail "install $SEC_FILE failed"
    log "installed $SEC_FILE (sha256=$src_sha)"
  fi
  echo "$src_sha  $SEC_FILE" | sha256sum -c - >/dev/null || fail "installed seccomp sha256 mismatch"

  finish installed "installed: seccomp=$SEC_FILE sha256=$src_sha apparmor=$AA_NAME (enforce, peer=$daemon)"
}

remove_profiles() {
  # Unload first — apparmor_parser -R needs the profile text. A missing
  # parser or missing apparmorfs only means "cannot be loaded anyway":
  # removal of the files still proceeds (idempotent).
  if aa_enabled && aa_is_loaded; then
    if [ -f "$AA_FILE" ] && aa_parser; then
      hostsh "apparmor_parser -R -K '$AA_HOST_FILE'" || fail "apparmor_parser -R failed on host"
      log "unloaded $AA_NAME"
    else
      # file already gone but profile still loaded — unload via a stub with
      # the same profile name staged in the apparmor dir.
      printf 'profile %s {\n}\n' "$AA_NAME" >"$AA_DIR/$STUB" || fail "cannot stage unload stub"
      if aa_parser && hostsh "apparmor_parser -R -K '$AA_HOST_DIR/$STUB'"; then
        log "unloaded $AA_NAME via stub"
      else
        log "WARN: could not unload $AA_NAME — a node reboot clears it"
      fi
      rm -f "$AA_DIR/$STUB"
    fi
  else
    log "$AA_NAME not loaded on host (or AppArmor absent) — nothing to unload"
  fi
  if [ -e "$AA_FILE" ]; then rm -f "$AA_FILE" && log "removed $AA_HOST_FILE"; else log "$AA_HOST_FILE already absent"; fi
  if [ -e "$SEC_FILE" ]; then rm -f "$SEC_FILE" && log "removed $SEC_FILE"; else log "$SEC_FILE already absent"; fi
  finish removed "removed: seccomp $SEC_FILE, apparmor $AA_NAME"
}

case "$MODE" in
  install) install_profiles ;;
  remove)  remove_profiles ;;
  *) fail "unknown NODE_PROFILES_MODE '$MODE' (install|remove)" ;;
esac
