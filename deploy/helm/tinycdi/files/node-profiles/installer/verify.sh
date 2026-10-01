#!/bin/sh
# TinyCDI node-profile readiness gate — the DaemonSet's readinessProbe,
# exec'd by the verifier container. In the shipped DaemonSet it runs in
# "direct mode": NODE_PROFILES_NSENTER is empty and the host files are read
# through read-only bind-mounts (seccomp dir + the host's mounted
# securityfs for the AppArmor loaded list) — no nsenter.
#
# The verifier runs as uid 0 with ALL capabilities dropped,
# allowPrivilegeEscalation=false and a read-only rootfs (CHTR-1). The
# kernel gates reads of /sys/kernel/security/apparmor/profiles on the
# caller being root — the file's 0444 mode is a decoy: a non-root reader
# always gets EACCES regardless of group or the mode bits. uid 0 + an
# empty capability set is therefore the least privilege that can still
# read the real loaded-profiles list, and the read-only mounts keep that
# uid strictly a reader.
#
#   MODE=install (default): Ready only while BOTH hold on the node —
#     * /sys/kernel/security/apparmor/profiles (on the host, via nsenter
#       or the bind-mount) contains `tinycdi-browser (enforce)`, and
#     * <kubeletRoot>/seccomp/profiles/chromium-userns.json exists and its
#       sha256 equals the shipped source's.
#
#   MODE=remove: Ready only once BOTH are gone — the AppArmor profile is
#     no longer listed (or the host has no AppArmor at all) and the
#     seccomp file is absent. A profiles file that EXISTS but is not
#     readable is NOT "absent" — it fails the probe rather than reporting
#     a false clean.
#
# Exit 1 (not ready) is the designed signal for "node not done yet" —
# including a container whose install phase refused/failed and restarted.
#
# Same environment seams as install.sh; defaults are the production values.
# NODE_PROFILES_AA_NAME (default tinycdi-browser) exists for the
# real-securityfs proof, which verifies against whatever profile the host
# actually has loaded.
set -eu

MODE=${NODE_PROFILES_MODE:-install}
SRC=${NODE_PROFILES_PROFILE_DIR:-/profiles}
SEC_DIR=${NODE_PROFILES_SECCOMP_DIR:-/host/seccomp-profiles}
AA_SYSFS=${NODE_PROFILES_AA_SYSFS:-/sys/kernel/security/apparmor/profiles}
NSENTER=${NODE_PROFILES_NSENTER-nsenter -t 1 -m --}

SECCOMP_NAME=chromium-userns.json
AA_NAME=${NODE_PROFILES_AA_NAME:-tinycdi-browser}
SEC_FILE=$SEC_DIR/$SECCOMP_NAME

host() {
  if [ -n "$NSENTER" ]; then
    $NSENTER "$@"
  else
    "$@"
  fi
}
hostsh() { host sh -c "$1"; }

if [ "$MODE" = "remove" ]; then
  [ ! -e "$SEC_FILE" ] || { echo "verify(remove): $SEC_FILE still present"; exit 1; }
  if hostsh "test -e '$AA_SYSFS'"; then
    # Present-but-unreadable is not proof of absence — only a readable
    # list that lacks the profile confirms removal.
    hostsh "test -r '$AA_SYSFS'" \
      || { echo "verify(remove): $AA_SYSFS present but unreadable — cannot confirm $AA_NAME unloaded"; exit 1; }
    set +e
    hostsh "grep -q '^$AA_NAME ' '$AA_SYSFS'"
    rc=$?
    set -e
    case $rc in
      0) echo "verify(remove): $AA_NAME still loaded on host"; exit 1 ;;
      1) : ;;  # absent — confirmed clean
      *) echo "verify(remove): cannot read $AA_SYSFS (grep rc=$rc)"; exit 1 ;;
    esac
  fi
  echo "verify(remove): profiles absent"
  exit 0
fi

[ "$MODE" = "install" ] || { echo "verify: unknown MODE '$MODE'"; exit 1; }

[ -f "$SEC_FILE" ] || { echo "verify: $SEC_FILE missing"; exit 1; }
src_sha=$(sha256sum "$SRC/$SECCOMP_NAME" | awk '{print $1}')
got_sha=$(sha256sum "$SEC_FILE" | awk '{print $1}')
[ "$src_sha" = "$got_sha" ] || { echo "verify: $SEC_FILE sha256 $got_sha != $src_sha"; exit 1; }

hostsh "test -r '$AA_SYSFS'" \
  || { echo "verify: no readable AppArmor profiles file on host ($AA_SYSFS)"; exit 1; }
# grep rc=1 (profile absent) and rc=2 (read error) both mean not-ready.
hostsh "grep -q '^$AA_NAME (enforce)' '$AA_SYSFS'" \
  || { echo "verify: $AA_NAME not loaded in enforce mode on host"; exit 1; }

echo "verify: seccomp sha256=$src_sha, apparmor $AA_NAME (enforce)"
exit 0
