#!/bin/sh
# Real-securityfs proof for the CHTR-1 verifier redesign — runs INSIDE a
# container that bind-mounts the HOST's securityfs read-only at
# /host-securityfs:
#
#   docker run --rm --network none --cap-drop ALL \
#     -v /sys/kernel/security:/host-securityfs:ro \
#     -v .../installer:/installer:ro -v <staged>:/seccomp:ro \
#     -v <staged>:/profiles-src:ro -v .../proof:/proof:ro \
#     alpine sh /proof/node-profiles/run-real-sysfs.sh
#
# It simply execs the SHIPPED verify.sh (/installer/verify.sh) in the
# DaemonSet's direct mode against the REAL kernel state — the caller picks
# the uid (--user / --cap-drop), MODE and AA_NAME so the driving test can
# assert each leg:
#   uid 0 + drop ALL   -> reads the real loaded list  (the shipped model)
#   uid 65534          -> EACCES, probe fails          (why uid 0 is needed)
#   MODE=remove, profile still loaded -> rc=1 "still loaded", never a
#   false clean (the CHTR-1 remove-mode fix).
#
# AA_NAME (default docker-default) names a profile the HOST actually has
# loaded — tinycdi-browser is normally absent from a build host, so the
# proof verifies against docker-default and a bogus name for the negative
# leg.
set -u

export NODE_PROFILES_NSENTER=
export NODE_PROFILES_AA_NAME=${AA_NAME:-docker-default}
export NODE_PROFILES_AA_SYSFS=${NODE_PROFILES_AA_SYSFS:-/host-securityfs/apparmor/profiles}
export NODE_PROFILES_SECCOMP_DIR=${NODE_PROFILES_SECCOMP_DIR:-/seccomp}
export NODE_PROFILES_PROFILE_DIR=${NODE_PROFILES_PROFILE_DIR:-/profiles-src}

echo "real-sysfs verify: uid=$(id -u) mode=${NODE_PROFILES_MODE:-install} aa=$NODE_PROFILES_AA_NAME sysfs=$NODE_PROFILES_AA_SYSFS"
exec sh /installer/verify.sh
