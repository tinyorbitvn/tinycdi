#!/bin/bash
# Copyright (c) 2026 TinyOrbit
# SPDX-License-Identifier: MIT
#
# Readiness gate for the kasm-adapter runtime: succeed ONLY when the X
# display is alive AND the KasmVNC HTTPS endpoint answers on :8443.
set -u

DISPLAY_NUM="${TCDI_DISPLAY:-1}"

xdpyinfo -display ":$DISPLAY_NUM" >/dev/null 2>&1 || exit 1

# KasmVNC 1.4.x brute-force protection drops loopback after repeated
# anonymous 401s: curl then prints the code it saw AND exits non-zero
# (yielding e.g. "401000"). Take the first 3 chars.
raw="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 4 \
  "https://127.0.0.1:8443/" 2>/dev/null || echo 000)"
code="${raw:0:3}"

case "$code" in
  200|301|302|401|403) exit 0 ;;
  *) exit 1 ;;
esac
