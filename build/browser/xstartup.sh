#!/bin/bash
# Browser profile session: openbox plus the policy browser maximized.
# The Chromium sandbox must stay engaged - never disable it here.
# TCDI_BROWSER selects the fallback (firefox-esr) for nodes without the measured
# security-profile pair; default is chromium.
openbox &
sleep 1
case "${TCDI_BROWSER:-chromium}" in
  chromium)
    exec chromium --no-first-run --no-default-browser-check \
      --start-maximized about:blank
    ;;
  firefox|firefox-esr)
    exec firefox-esr about:blank
    ;;
  *)
    echo "tcdi-xstartup: unknown TCDI_BROWSER '${TCDI_BROWSER}'" >&2
    exec xterm
    ;;
esac
