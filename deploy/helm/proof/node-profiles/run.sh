#!/bin/sh
# Node-profile installer proof — runs INSIDE an alpine container that
# simulates a host (see TestNodeProfilesInstallerDockerProof in
# deploy/helm/chart_test.go). The test mounts:
#   /installer   = files/node-profiles/installer (install.sh, verify.sh)
#   /profiles-src = the two profiles flattened (chromium-userns.json,
#                   tinycdi-browser)
#   /proof       = this script
#
# Env seams point the installer at a fake host root under /tmp/h with a
# fake apparmor_parser and a fake loaded-profiles file. The fake /proc
# carries a comm AND a verified exe symlink — SEC-37 hardened discovery
# requires exe (or argv[0]) to resolve to "containerd", comm alone is not
# enough; scenario 5b proves a comm-spoofed process is skipped. CHTR-4 adds
# the namespace check: the daemon's /proc/<pid>/ns/{pid,mnt} must equal
# /proc/1's; scenario 5c proves a containerized imposter (real containerd
# binary in a different pid/mnt namespace) is skipped.
#
# Proves: install (placeholder resolution, atomic write, sha256, host-parser
# -Q/-r invocation), idempotent re-run, daemon-label switch to unconfined,
# readiness gate (verify.sh) catching a corrupted seccomp file + repair,
# refusal without AppArmor / without parser / with a comm-only fake
# containerd, and mode=remove.
set -u
H=/tmp/h
PROFILES=$H/sys/kernel/security/apparmor/profiles
PASS=0; FAIL=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAIL=$((FAIL+1)); }

# ---- fake host ---------------------------------------------------------
mkdir -p "$H/bin" "$H/proc/1/ns" "$H/proc/4321/attr" "$H/proc/4321/ns" \
         "$H/sys/kernel/security/apparmor" \
         "$H/var/lib/kubelet" "$H/etc/apparmor.d" /profiles-src-d
printf 'cri-containerd.apparmor.d (enforce)\nunconfined\n' > "$PROFILES"
echo init > "$H/proc/1/comm"
ln -s 'pid:[4026531836]' "$H/proc/1/ns/pid"            # host pid namespace
ln -s 'mnt:[4026531840]' "$H/proc/1/ns/mnt"            # host mount namespace
echo containerd > "$H/proc/4321/comm"
ln -s /usr/bin/containerd "$H/proc/4321/exe"          # exe-verified daemon
ln -s 'pid:[4026531836]' "$H/proc/4321/ns/pid"         # host namespaces (CHTR-4)
ln -s 'mnt:[4026531840]' "$H/proc/4321/ns/mnt"
echo 'cri-containerd.apparmor.d (enforce)' > "$H/proc/4321/attr/current"
# a comm-spoofed imposter (SEC-37): comm says containerd, exe does not.
# Its namespaces match PID 1's so only the exe check rejects it.
mkdir -p "$H/proc/4400/attr" "$H/proc/4400/ns"
echo containerd > "$H/proc/4400/comm"
ln -s /usr/bin/not-containerd "$H/proc/4400/exe"
ln -s 'pid:[4026531836]' "$H/proc/4400/ns/pid"
ln -s 'mnt:[4026531840]' "$H/proc/4400/ns/mnt"
echo 'unconfined' > "$H/proc/4400/attr/current"

cat > "$H/bin/apparmor_parser" <<'EOF'
#!/bin/sh
# fake host parser: -Q = compile-check, -r/-a = load, -R = unload
P=/tmp/h/sys/kernel/security/apparmor/profiles
echo "parser-call: $*" >> /tmp/h/parser-calls.log
mode=check; file=
for a in "$@"; do
  case "$a" in
    -Q) mode=check;; -r) mode=load;; -a) mode=load;; -R) mode=remove;; -K) :;;
    *) file=$a;;
  esac
done
[ -n "$file" ] || { echo "fake-parser: no file arg" >&2; exit 1; }
name=$(sed -n 's/^profile \([^ ]*\).*/\1/p' "$file" | head -1)
[ -n "$name" ] || { echo "fake-parser: no profile declaration in $file" >&2; exit 1; }
case "$mode" in
  check)  grep -q 'userns,' "$file" || { echo "fake-parser: compile check failed" >&2; exit 1; };;
  load)   grep -q "^$name " "$P" || echo "$name (enforce)" >> "$P";;
  remove) grep -v "^$name " "$P" > "$P.t" 2>/dev/null; mv "$P.t" "$P";;
esac
exit 0
EOF
chmod +x "$H/bin/apparmor_parser"

# empty default = run host commands directly (container == simulated host);
# the shipped verifier runs this way over its read-only bind-mounts.
export NODE_PROFILES_NSENTER=${NODE_PROFILES_NSENTER-}
export NODE_PROFILES_ONESHOT=1
export NODE_PROFILES_PROC=$H/proc
export NODE_PROFILES_AA_SYSFS=$PROFILES
export NODE_PROFILES_SECCOMP_DIR=$H/var/lib/kubelet/seccomp/profiles
export NODE_PROFILES_APPARMOR_DIR=$H/etc/apparmor.d
export NODE_PROFILES_APPARMOR_HOST_DIR=$H/etc/apparmor.d
export NODE_PROFILES_PROFILE_DIR=/profiles-src
export NODE_NAME=fake-node-01
export PATH=$H/bin:$PATH
unset NODE_PROFILES_MODE || true

SEC_FILE=$H/var/lib/kubelet/seccomp/profiles/chromium-userns.json
AA_FILE=$H/etc/apparmor.d/tinycdi-browser
SRC_SHA=$(sha256sum /profiles-src/chromium-userns.json | awk '{print $1}')

echo "=== scenario 1: install ==="
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -eq 0 ] && ok "install exits 0" || bad "install rc=$rc"
[ -f "$SEC_FILE" ] && ok "seccomp file installed" || bad "seccomp file missing"
[ "$(sha256sum "$SEC_FILE" | awk '{print $1}')" = "$SRC_SHA" ] \
  && ok "seccomp sha256 matches source ($SRC_SHA)" || bad "seccomp sha mismatch"
[ -f "$AA_FILE" ] && ok "apparmor file installed" || bad "apparmor file missing"
grep -q 'peer=cri-containerd.apparmor.d,' "$AA_FILE" \
  && ok "placeholder resolved to daemon label cri-containerd.apparmor.d" \
  || bad "daemon peer not substituted: $(grep 'peer=' "$AA_FILE" | head -5)"
grep -q 'peer=cri-containerd,' "$AA_FILE" \
  && bad "unsubstituted placeholder remains" || ok "no unsubstituted placeholder"
grep -q 'tinycdi-browser (enforce)' "$PROFILES" \
  && ok "profile loaded in fake host apparmorfs" || bad "profile not in loaded list"
grep -q 'parser-call: -Q -K' $H/parser-calls.log \
  && ok "host parser compile-check (-Q -K) ran" || bad "no compile check call"
grep -q 'parser-call: -r -K' $H/parser-calls.log \
  && ok "host parser load (-r -K) ran" || bad "no load call"
sh /installer/verify.sh >/dev/null 2>&1 \
  && ok "verify.sh reports ready after install" || bad "verify.sh fails after install"

echo "=== scenario 2: idempotent re-run ==="
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -eq 0 ] && ok "re-run exits 0" || bad "re-run rc=$rc"
echo "$out" | grep -q 'apparmor file unchanged' \
  && ok "apparmor re-run is idempotent (unchanged)" || bad "apparmor re-run not idempotent"
echo "$out" | grep -q 'seccomp file unchanged' \
  && ok "seccomp re-run is idempotent (unchanged)" || bad "seccomp re-run not idempotent"

echo "=== scenario 3: unconfined containerd ==="
echo 'unconfined' > "$H/proc/4321/attr/current"
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -eq 0 ] || bad "install rc=$rc with unconfined daemon"
grep -q 'peer=unconfined,' "$AA_FILE" \
  && ok "unconfined containerd -> peer=unconfined" || bad "unconfined peer missing"
grep -q 'peer=cri-containerd' "$AA_FILE" \
  && bad "stale cri-containerd peer remains" || ok "no stale daemon peer"
# restore confined daemon for the remaining scenarios
echo 'cri-containerd.apparmor.d (enforce)' > "$H/proc/4321/attr/current"
sh /installer/install.sh >/dev/null 2>&1

echo "=== scenario 4: corrupted seccomp -> verify fails -> repair ==="
echo 'corruption' >> "$SEC_FILE"
if sh /installer/verify.sh >/dev/null 2>&1; then
  bad "verify.sh passed with corrupted seccomp file"
else
  ok "verify.sh fails on corrupted seccomp file"
fi
sh /installer/install.sh >/dev/null 2>&1
[ "$(sha256sum "$SEC_FILE" | awk '{print $1}')" = "$SRC_SHA" ] \
  && ok "re-run repaired corrupted seccomp file" || bad "seccomp not repaired"
sh /installer/verify.sh >/dev/null 2>&1 && ok "verify ready again" || bad "verify still failing"

echo "=== scenario 5: refusal without AppArmor / without parser ==="
mv "$PROFILES" "$PROFILES.away"
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -ne 0 ] && echo "$out" | grep -q 'REFUSE' \
  && ok "refuses when apparmorfs missing" || bad "no refusal without apparmorfs"
mv "$PROFILES.away" "$PROFILES"
mv "$H/bin/apparmor_parser" "$H/bin/apparmor_parser.away"
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -ne 0 ] && echo "$out" | grep -q 'REFUSE' \
  && ok "refuses when apparmor_parser missing" || bad "no refusal without parser"
mv "$H/bin/apparmor_parser.away" "$H/bin/apparmor_parser"
# sanity: still installs fine after restoring the environment
sh /installer/install.sh >/dev/null 2>&1 && ok "installs again after refusal env fixed" || bad "post-refusal install failed"

echo "=== scenario 5b: SEC-37 — comm-spoofed containerd is skipped ==="
# remove the real daemon's marker files; only the imposter (comm=containerd,
# exe=not-containerd) remains -> discovery must REFUSE, not read its label.
mv "$H/proc/4321" "$H/proc/daemon-away"   # non-digit name escapes the /proc/[0-9]* glob
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -ne 0 ] && echo "$out" | grep -qi 'containerd' \
  && ok "refuses when only a comm-spoofed containerd exists" || bad "comm-spoofed containerd accepted"
mv "$H/proc/daemon-away" "$H/proc/4321"
sh /installer/install.sh >/dev/null 2>&1 \
  && ok "installs again with the real daemon back" || bad "post-spoof install failed"

echo "=== scenario 5c: CHTR-4 — containerized imposter (right binary, wrong namespaces) ==="
# pid 1234 sorts before the real daemon in the glob and passes comm+exe,
# but its pid/mnt namespaces differ from /proc/1's — it must be skipped.
mkdir -p "$H/proc/1234/attr" "$H/proc/1234/ns"
echo containerd > "$H/proc/1234/comm"
ln -s /usr/bin/containerd "$H/proc/1234/exe"
ln -s 'pid:[4026539999]' "$H/proc/1234/ns/pid"
ln -s 'mnt:[4026539998]' "$H/proc/1234/ns/mnt"
echo 'unconfined' > "$H/proc/1234/attr/current"
rm -f "$AA_FILE"
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -eq 0 ] || bad "install rc=$rc with ns-spoofed imposter present"
grep -q 'peer=cri-containerd.apparmor.d,' "$AA_FILE" \
  && ok "ns-mismatched containerd skipped — daemon label still wins" \
  || bad "ns-mismatched imposter label used: $(grep 'peer=' "$AA_FILE" | head -3)"
# and discovery must REFUSE when only namespaced imposters remain
mv "$H/proc/4321" "$H/proc/daemon-away"
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -ne 0 ] && echo "$out" | grep -qi 'containerd' \
  && ok "refuses when every containerd candidate is non-host" \
  || bad "ns-mismatched imposter accepted"
mv "$H/proc/daemon-away" "$H/proc/4321"
rm -rf "$H/proc/1234"
sh /installer/install.sh >/dev/null 2>&1 \
  && ok "installs again after imposter removed" || bad "post-imposter install failed"

echo "=== scenario 6: mode=remove ==="
export NODE_PROFILES_MODE=remove
out=$(sh /installer/install.sh 2>&1); rc=$?
echo "$out"
[ $rc -eq 0 ] && ok "remove exits 0" || bad "remove rc=$rc"
[ ! -e "$SEC_FILE" ] && ok "seccomp file removed" || bad "seccomp file still present"
[ ! -e "$AA_FILE" ] && ok "apparmor file removed" || bad "apparmor file still present"
grep -q 'tinycdi-browser' "$PROFILES" \
  && bad "profile still loaded after remove" || ok "profile unloaded from fake apparmorfs"
grep -q 'parser-call: -R -K' $H/parser-calls.log \
  && ok "host parser unload (-R -K) ran" || bad "no unload call"
sh /installer/verify.sh >/dev/null 2>&1 \
  && ok "verify.sh reports ready once removal verified" || bad "verify(remove) fails"
out=$(sh /installer/install.sh 2>&1); rc=$?
[ $rc -eq 0 ] && ok "remove is idempotent (second run rc=0)" || bad "remove re-run rc=$rc"

echo "================= $PASS passed, $FAIL failed ================="
[ $FAIL -eq 0 ]
