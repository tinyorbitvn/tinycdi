#!/bin/bash
# Linux desktop runtime entrypoint.
#
# Boot sequence:
#   1. Read credentials from mounted Secret FILES ($TCDI_SECRET_DIR, default
#      /run/secrets/tcdi). Values are never passed via env, argv, or logs.
#   2. Stage TLS material and the kasmpasswd file on the ephemeral /run/tcdi
#      mount - outside the persistent home so nothing sensitive survives
#      on the volume.
#   3. Neutralize the two $HOME paths KasmVNC would otherwise trust
#      (~/.kasmpasswd, ~/.vnc/kasmvnc.yaml): a stale file left on the
#      persistent home volume must never override the mounted Secret or
#      the system policy in /etc/kasmvnc/kasmvnc.yaml.
#   4. Start KasmVNC (:1, HTTPS :8443) and supervise the X server; exit so
#      the orchestrator restarts us when the display dies.
set -euo pipefail

# File-descriptor bound for the whole process tree: Xvnc multiplexes
# every client connection with select() — FD_SET on the socket fd is
# undefined once an fd reaches FD_SETSIZE (1024): the bit write lands
# past the stack-allocated fd_set and the server dies SIGSEGV. Each
# websocket client costs ~3 fds inside the single Xvnc process on top of
# a ~23-fd baseline, so an unbounded fd table turns a connection burst
# into stack corruption (~334 concurrent streams on a clean pod). A soft
# RLIMIT_NOFILE below FD_SETSIZE makes accept() fail EMFILE instead, so
# excess connections are refused while every fd Xvnc can hold stays
# select()-safe. Soft limit only — inherited by every child (Xvnc, the
# websockify threads, the session), and 1000 still allows ~325
# concurrent streams, far above the gateway's per-workspace stream cap;
# the supervisor loop and helpers each need only a handful of fds. The
# hard limit is left alone so a debug shell can still raise it.
# Residual: under sustained saturation a racing handshake thread can
# still exit(1) via KasmVNC's fatal() on the EMFILE'd tls.key open —
# a clean exit, not memory corruption (upstream robustness bug).
#
# The target is min(1000, hard): a pod may cap RLIMIT_NOFILE below the
# target (exotic, but legal) — soft can never exceed hard, so take the
# ambient bound in that case. The clamp must never kill the boot: on any
# failure warn and continue — a loud line beats a pod that never starts.
NOFILE_HARD="$(ulimit -Hn)"
NOFILE_TARGET=1000
case "$NOFILE_HARD" in
  ''|*[!0-9]*) ;;  # 'unlimited' or unparsable: keep the 1000 target
  *) [ "$NOFILE_HARD" -lt "$NOFILE_TARGET" ] && NOFILE_TARGET="$NOFILE_HARD" ;;
esac
if ulimit -Sn "$NOFILE_TARGET" 2>/dev/null; then
  echo "tcdi-entrypoint: RLIMIT_NOFILE soft=$(ulimit -Sn) (hard=$NOFILE_HARD) — client fds stay below FD_SETSIZE"
else
  echo "tcdi-entrypoint: WARNING: could not set RLIMIT_NOFILE=$NOFILE_TARGET (soft=$(ulimit -Sn), hard=$NOFILE_HARD)" >&2
fi

SECRET_DIR="${TCDI_SECRET_DIR:-/run/secrets/tcdi}"
PASS_FILE="$SECRET_DIR/password"
USER_FILE="$SECRET_DIR/username"
CERT_FILE="$SECRET_DIR/tls.crt"
KEY_FILE="$SECRET_DIR/tls.key"
RT=/run/tcdi
DISPLAY_NUM=1

fail() { echo "tcdi-entrypoint: $*" >&2; exit 1; }

[ -f "$PASS_FILE" ] || fail "mounted Secret file missing: $PASS_FILE"
[ -f "$CERT_FILE" ] || fail "mounted Secret file missing: $CERT_FILE"
[ -f "$KEY_FILE" ]  || fail "mounted Secret file missing: $KEY_FILE"
[ -d "$RT" ] && [ -w "$RT" ] || fail "$RT is not a writable mount (need tmpfs/emptyDir owned by uid $(id -u))"

# A Secret value written with a line ending carries the same credential:
# $(cat) drops the trailing newline(s); ${v%$'\r'} drops the CR a CRLF
# ending leaves behind. Any other whitespace stays part of the value.
KASMVNC_USER="kasm_user"
if [ -f "$USER_FILE" ]; then
  KASMVNC_USER="$(cat "$USER_FILE")"
  KASMVNC_USER="${KASMVNC_USER%$'\r'}"
fi
[ -n "$KASMVNC_USER" ] || fail "$USER_FILE is empty"

mkdir -p "$HOME/.vnc"

# TLS material for the KasmVNC web endpoint: copied to the ephemeral mount
# because kasmvnc.yaml points there and $HOME must not accumulate secrets.
cp "$CERT_FILE" "$RT/tls.crt"
cp "$KEY_FILE" "$RT/tls.key"
chmod 600 "$RT/tls.key"
chmod 644 "$RT/tls.crt"

# kasmvncpasswd prompts twice; feed the mounted secret on stdin so the value
# never lands in argv or logs. The value is normalized first (one line
# ending stripped) so a Secret file written '\n'- or '\r\n'-terminated
# stores the same credential the broker reads (it trims the Secret value).
PASS="$(cat "$PASS_FILE")"
PASS="${PASS%$'\r'}"
[ -n "$PASS" ] || fail "$PASS_FILE is empty"
# NOTE: no -o - the runtime user must NOT be an owner. Owner rights unlock
# the management/API surface (port relay, upload/download); with a write-only
# user those endpoints deny every request.
{ printf '%s\n' "$PASS"; printf '%s\n' "$PASS"; } | kasmvncpasswd -u "$KASMVNC_USER" -w "$RT/kasmpasswd" >/dev/null
chmod 600 "$RT/kasmpasswd"

# The launcher's "any users configured" check reads $HOME/.kasmpasswd before
# config is applied. Force it to be a symlink to the tmpfs file - rm first so
# a stale regular file on the persistent volume can never shadow the Secret.
rm -rf "$HOME/.kasmpasswd"
ln -s "$RT/kasmpasswd" "$HOME/.kasmpasswd"

# kasmvncserver merges $HOME/.vnc/kasmvnc.yaml AFTER /etc/kasmvnc/kasmvnc.yaml
# and auto-creates it with logging.level=100 when absent. Rewrite it every
# boot with a fixed minimal file: stale content (e.g. require_ssl: false,
# another port, a different password file) can never take effect.
cat > "$HOME/.vnc/kasmvnc.yaml" <<'YAML'
# Managed by the tcdi entrypoint - rewritten on every container start.
# The enforceable policy is /etc/kasmvnc/kasmvnc.yaml.
logging:
  level: 30
YAML

# -SecurityTypes None: RFB security is None because authentication happens at
# the HTTPS layer (Basic auth against $RT/kasmpasswd, injected server-side by
# the gateway). The raw RFB port is never published. -xstartup is explicit so
# a stale $HOME/.vnc/xstartup can never take over the session.
kasmvncserver ":$DISPLAY_NUM" \
  -xstartup /opt/tcdi/xstartup.sh \
  -SecurityTypes None >> "$HOME/.vnc/entrypoint.log" 2>&1

shutdown() {
  kasmvncserver -kill ":$DISPLAY_NUM" >/dev/null 2>&1 || true
  exit 0
}
trap shutdown TERM INT

tail -F "$HOME"/.vnc/*.log 2>/dev/null &
TAILPID=$!

# Supervise Xvnc (the kasmvncserver launcher execs /usr/bin/Xvnc, which also
# serves the HTTPS/websocket endpoint). On death: wait one healthcheck grace
# window so Docker records "unhealthy", then exit for restart semantics.
while pgrep -x Xvnc >/dev/null 2>&1; do sleep 2; done
sleep 12
kill "$TAILPID" 2>/dev/null || true
shutdown
