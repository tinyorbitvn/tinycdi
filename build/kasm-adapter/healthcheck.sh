#!/bin/bash
# Copyright (c) 2026 TinyOrbit
# SPDX-License-Identifier: MIT
#
# Readiness gate for the kasm-adapter runtime: succeed ONLY when the X
# display is alive AND the KasmVNC HTTPS endpoint answers on :8443.
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
# A Secret value written with a line ending carries the same credential:
# $(cat) drops the trailing newline(s); ${v%$'\r'} drops the CR a CRLF
# ending leaves behind. Any other whitespace stays part of the value.
user="kasm_user"
if [ -r "$SECRET_DIR/username" ]; then
  user="$(cat "$SECRET_DIR/username")"
  user="${user%$'\r'}"
fi
[ -n "$user" ] || exit 1
pass="$(cat "$SECRET_DIR/password")"
pass="${pass%$'\r'}"
[ -n "$pass" ] || exit 1
# curl config quoting: escape backslash and double quote.
cred="$(printf '%s:%s' "$user" "$pass" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"

# KasmVNC 1.4.x loopback quirk: when 127.0.0.1 IS blacklisted curl prints
# the code it saw AND exits non-zero (yielding e.g. "200000"). Take the
# first 3 chars (a blacklisted loopback is still never counted ready by
# the credentialed probe: it would see 000 / a dropped connection).
raw="$(printf 'user = "%s"\n' "$cred" \
  | curl -sk -K - -o /dev/null -w '%{http_code}' --max-time 4 \
    "https://127.0.0.1:8443/" 2>/dev/null || echo 000)"
code="${raw:0:3}"

case "$code" in
  200|301|302) exit 0 ;;
  *) exit 1 ;;
esac
