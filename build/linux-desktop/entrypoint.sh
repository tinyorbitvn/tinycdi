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

KASMVNC_USER="kasm_user"
if [ -f "$USER_FILE" ]; then
  KASMVNC_USER="$(tr -d '[:space:]' < "$USER_FILE")"
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
# never lands in argv or logs.
# NOTE: no -o - the runtime user must NOT be an owner. Owner rights unlock
# the management/API surface (port relay, upload/download); with a write-only
# user those endpoints deny every request.
{ cat "$PASS_FILE"; cat "$PASS_FILE"; } | kasmvncpasswd -u "$KASMVNC_USER" -w "$RT/kasmpasswd" >/dev/null
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
