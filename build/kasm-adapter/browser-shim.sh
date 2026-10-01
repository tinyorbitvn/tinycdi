#!/bin/bash
# Copyright (c) 2026 TinyOrbit
# SPDX-License-Identifier: MIT
#
# Chromium-family launcher shim. The TinyCDI adapter bind-mounts this
# script READ-ONLY (subPath from the adapter volume) over every
# Chromium-family wrapper path the kasmweb/* images ship
# (/usr/bin/chromium, /usr/bin/google-chrome, /usr/bin/microsoft-edge, ...
# — the list lives in internal/runtime/linux/backend.go).
#
# Why: Kasm's wrappers hardcode --no-sandbox. The first launch goes
# through spec.linux.sessionCmd (the real binary), but every RELAUNCH
# path — the seeded ~/Desktop/*.desktop icon, the applications menu,
# xdg-open/gtk-launch, and the Debian alternatives chain
# (x-www-browser/gnome-www-browser/sensible-browser) — lands on a wrapper
# path. This shim makes all of them exec the REAL browser binary with the
# sandbox-weakening flags stripped, so renderers always run inside the
# nested userns + seccomp sandbox (ADR 0001). The mount is read-only and
# needs CAP_SYS_ADMIN to undo — the session user cannot remove it.
set -u

# The real binary: entrypoint.sh records the sessionCmd payload binary on
# the ephemeral runtime mount; fall back to the known catalog paths so
# full-desktop images (no sessionCmd) are covered too.
BIN_FILE=/run/tcdi/browser-bin
real=""
if [ -f "$BIN_FILE" ]; then
  real="$(head -n1 "$BIN_FILE" 2>/dev/null || true)"
fi
if [ -z "$real" ] || [ ! -x "$real" ] || [ "$real" -ef "$0" ]; then
  real=""
  for c in \
    /usr/bin/chromium-orig \
    /usr/lib/chromium/chromium \
    /usr/lib/chromium-browser/chromium-browser \
    /opt/google/chrome/google-chrome \
    /opt/microsoft/msedge/microsoft-edge \
    /opt/brave.com/brave/brave-browser \
    /opt/vivaldi/vivaldi \
    /opt/opera/opera \
  ; do
    # -ef guards against exec'ing this same shim file (loop).
    if [ -x "$c" ] && [ ! "$c" -ef "$0" ]; then
      real="$c"
      break
    fi
  done
fi
if [ -z "$real" ]; then
  echo "tcdi-browser-shim: no sandboxed Chromium-family binary found" >&2
  exit 127
fi

# Loop guard: never re-enter the shim more than a few times.
depth=$(( ${TCDI_SHIM_DEPTH:-0} + 1 ))
if [ "$depth" -gt 4 ]; then
  echo "tcdi-browser-shim: launcher loop detected" >&2
  exit 126
fi
export TCDI_SHIM_DEPTH="$depth"

# Drop every flag that disables or weakens the Chromium sandbox; pass
# everything else through unchanged.
args=()
for a in "$@"; do
  case "$a" in
    --no-sandbox|--no-zygote-sandbox|--disable-zygote|\
    --disable-setuid-sandbox|--disable-namespace-sandbox|\
    --disable-seccomp-filter-sandbox|--disable-gpu-sandbox|\
    --single-process|--in-process-gpu|\
    --service-sandbox-type=none|--service-sandbox-type=*)
      continue ;;
  esac
  args+=("$a")
done

exec "$real" "${args[@]}"
