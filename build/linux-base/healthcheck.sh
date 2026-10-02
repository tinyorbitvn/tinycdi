#!/bin/bash
# Readiness gate: succeed ONLY when the X display is alive AND the KasmVNC
# HTTPS endpoint answers on :8443. Either leg failing => not ready.
# (kasmvncserver itself uses xdpyinfo for the display check.)
set -u

DISPLAY_NUM="${TCDI_DISPLAY:-1}"

xdpyinfo -display ":$DISPLAY_NUM" >/dev/null 2>&1 || exit 1

code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 4 \
  "https://127.0.0.1:8443/" 2>/dev/null || echo 000)"

# Any real HTTP answer (401 auth challenge, 200 page, redirect) proves the
# endpoint is serving; 000/refused/timeout means not ready.
case "$code" in
  200|301|302|401|403) exit 0 ;;
  *) exit 1 ;;
esac
