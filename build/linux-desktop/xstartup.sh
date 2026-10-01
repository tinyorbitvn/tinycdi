#!/bin/bash
# Desktop profile session: openbox WM plus a terminal. The browser image
# overrides this file at /opt/tcdi/xstartup.sh with a browser session.
openbox &
sleep 1
exec xterm -geometry 140x40+40+40 -fa 'Monospace' -fs 12
