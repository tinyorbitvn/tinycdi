#!/bin/bash
# Copyright (c) 2026 TinyOrbit
# SPDX-License-Identifier: MIT
#
# Session launcher for kasm-adapter runtimes.
#
# TCDI_SESSION_CMD selects the session payload (spec.linux.sessionCmd). For
# Chromium-family Kasm images it MUST point at the real binary
# (/usr/bin/chromium-orig, /opt/google/chrome/google-chrome): the image's
# /usr/bin/chromium and /usr/bin/google-chrome are Kasm wrappers that
# hardcode --no-sandbox, which is forbidden by the TinyCDI security model
# (ADR 0001). See docs/kasm-images.md — per-image wrapper audit.
#
# Default (no sessionCmd): the image's XFCE session.
if command -v dbus-launch >/dev/null 2>&1; then
  eval "$(dbus-launch --sh-syntax)" 2>/dev/null || true
fi

if [ -n "${TCDI_SESSION_CMD:-}" ]; then
  if [ "${TCDI_WITH_WM:-1}" = "1" ] && command -v startxfce4 >/dev/null 2>&1; then
    startxfce4 >/dev/null 2>&1 &
    sleep 1
  fi
  exec bash -c "$TCDI_SESSION_CMD"
fi

if command -v startxfce4 >/dev/null 2>&1; then
  exec startxfce4
fi
exec xterm
