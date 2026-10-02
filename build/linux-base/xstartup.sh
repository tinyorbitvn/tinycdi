#!/bin/bash
# Base-image session: a plain root window and nothing else. Profiles
# (build/browser, build/linux-desktop, custom images) replace this file at
# /opt/tcdi/xstartup.sh. kasmvncserver ends the session when xstartup
# exits, so this idles instead of returning.
xsetroot -solid '#14284b' 2>/dev/null || true
exec sleep infinity
