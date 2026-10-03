#!/bin/bash
# Readiness gate: succeed ONLY when the X display is alive AND the KasmVNC
# HTTPS endpoint answers on :8443. Either leg failing => not ready.
# (kasmvncserver itself uses xdpyinfo for the display check.)
set -u

DISPLAY_NUM="${TCDI_DISPLAY:-1}"

xdpyinfo -display ":$DISPLAY_NUM" >/dev/null 2>&1 || exit 1

# The probe authenticates with the mounted Secret the entrypoint already
# reads (never printed, never in argv: curl takes it from a config on
# stdin). An ANONYMOUS probe is an authentication failure to KasmVNC: every
# probe logged "Authentication attempt failed" and, after the
# brute_force_protection threshold (kasmvnc.yaml), 127.0.0.1 was
# blacklisted - dropping any real client that reaches KasmVNC as loopback
# (sidecar proxy, port-forward, hostNetwork ingress). A valid login is not
# a failure, so the probe is invisible to the lockout.
SECRET_DIR="${TCDI_SECRET_DIR:-/run/secrets/tcdi}"
# Fail closed: a runtime whose password file is missing/unreadable cannot serve a session either, so NotReady is the truth.
[ -r "$SECRET_DIR/password" ] || exit 1
user="kasm_user"
if [ -r "$SECRET_DIR/username" ]; then
  user="$(tr -d '[:space:]' < "$SECRET_DIR/username")"
fi
[ -n "$user" ] || exit 1
# curl config quoting: escape backslash and double quote.
cred="$(printf '%s:%s' "$user" "$(cat "$SECRET_DIR/password")" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"

code="$(printf 'user = "%s"\n' "$cred" \
  | curl -sk -K - -o /dev/null -w '%{http_code}' --max-time 4 \
    "https://127.0.0.1:8443/" 2>/dev/null || echo 000)"
# curl may print the code AND exit non-zero (KasmVNC loopback quirk,
# "200000"): keep the first three characters.
code="${code:0:3}"

# 200 (or a redirect) with valid credentials proves the endpoint is
# serving AND still accepts the mounted Secret; 401 (credential mismatch),
# 000/refused/timeout/blacklisted mean not ready.
case "$code" in
  200|301|302) exit 0 ;;
  *) exit 1 ;;
esac
