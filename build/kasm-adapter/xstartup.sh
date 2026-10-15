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
# Session lifecycle (same as build/browser/xstartup.sh on the native
# browser image): the payload OWNS the session — when it exits, the X
# display is torn down so the container exits and the restart policy
# relaunches a fresh session. A live desktop must never linger past the
# payload's exit: its icons/menus are relaunch surfaces (KASM-1). The
# read-only browser-shim bind mounts make every one of those paths
# sandboxed regardless, this is the second layer.
#
# Default (no sessionCmd): the image's XFCE session.
if command -v dbus-launch >/dev/null 2>&1; then
  eval "$(dbus-launch --sh-syntax)" 2>/dev/null || true
fi

if [ -n "${TCDI_SESSION_CMD:-}" ]; then
  if [ "${TCDI_WITH_WM:-1}" = "1" ]; then
    # Window manager only — NOT the full xfce4-session: no desktop icons,
    # panel launchers or application menu to relaunch the browser
    # through. xfwm4 first (all kasm XFCE images), then openbox, then the
    # full session as a last resort so the payload never runs WM-less
    # when a window manager exists.
    if command -v xfwm4 >/dev/null 2>&1; then
      xfwm4 >/dev/null 2>&1 &
    elif command -v openbox >/dev/null 2>&1; then
      openbox >/dev/null 2>&1 &
    elif command -v startxfce4 >/dev/null 2>&1; then
      startxfce4 >/dev/null 2>&1 &
    fi
    sleep 1
  fi
  bash -c "$TCDI_SESSION_CMD"
  # Payload exited (browser closed, crash, ...) — end the display. The
  # entrypoint's Xvnc watch then exits the container and the pod restarts
  # into a fresh session instead of leaving a relaunchable desktop.
  kasmvncserver -kill "${DISPLAY:-:1}" >/dev/null 2>&1 || pkill -x Xvnc || true
  exit 0
fi

if command -v startxfce4 >/dev/null 2>&1; then
  exec startxfce4
fi
exec xterm
