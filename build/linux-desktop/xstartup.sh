#!/bin/bash
# Desktop profile session: XFCE4 under a private D-Bus session bus.
#
# Everything writable lives on a mount the pod already provides: the
# session runtime dir on the ephemeral /run/tcdi, caches and settings in
# the persistent home. Compositing is off (xfwm4.xml), there is no
# screensaver, lock or power manager, and no session is saved on exit.
cd "$HOME" || cd /
export XDG_RUNTIME_DIR=/run/tcdi/xdg
mkdir -p "$XDG_RUNTIME_DIR" && chmod 700 "$XDG_RUNTIME_DIR"
export XDG_CURRENT_DESKTOP=XFCE XDG_SESSION_DESKTOP=xfce XDG_SESSION_TYPE=x11
export XDG_MENU_PREFIX=xfce-
# A VNC framebuffer never blanks; make sure nothing tries to.
xset s off -dpms 2>/dev/null || true
# --exit-with-session: the bus goes away with xfce4-session. The session
# ending ends this script, which ends the KasmVNC session (and the
# container restarts) - there is deliberately no log-out button.
exec dbus-launch --exit-with-session xfce4-session
