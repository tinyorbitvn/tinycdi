# Desktop workspace profile: the linux-base runtime plus an XFCE4 desktop
# and Firefox ESR.
#
# Builds on the local linux-base image (same KasmVNC/HTTPS/healthcheck/
# credential contract); override BASE_IMAGE to consume a registry digest.
# The session script (xstartup.sh) starts XFCE4 under its own D-Bus
# session bus.
#
# What is installed, and what is deliberately not, is documented in
# docs/images.md ("The desktop profile"). In short: session, window
# manager, panel, desktop, file manager, terminal, settings, text editor,
# archive tool and image viewer; Firefox ESR doubles as the PDF viewer.
# No display manager, screensaver/lock, power management, polkit agent,
# sudo or package-manager front end; compositing is off.
#
# Firefox ESR pin: the SAME apt pin as build/browser/Dockerfile
# (docs/compatibility.md); runtime-freshness bumps both together and
# .github/tests/check-browser-freshness.test.sh asserts they agree.
ARG BASE_IMAGE=ghcr.io/tinyorbitvn/tinycdi-linux-base:local
FROM ${BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG FIREFOX_ESR_APT_VERSION=153.4.0esr-1~deb12u1

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-settings \
      thunar thunar-archive-plugin xfce4-terminal mousepad xarchiver \
      ristretto \
      greybird-gtk-theme adwaita-icon-theme xdg-utils \
      "firefox-esr=${FIREFOX_ESR_APT_VERSION}" \
 && rm -rf /var/lib/apt/lists/*

# Ship the license/notice set inside the image even when BASE_IMAGE is
# overridden to a build that predates the linux-base notices.
COPY LICENSE NOTICE THIRD_PARTY_LICENSES.md SOURCE-OFFER /usr/share/doc/tinycdi/
COPY build/linux-desktop/xfce4/ /etc/xdg/xfce4/
COPY build/linux-desktop/mimeapps.list /etc/xdg/mimeapps.list
COPY build/linux-desktop/firefox/policies.json /usr/lib/firefox-esr/distribution/policies.json
COPY build/linux-desktop/xstartup.sh /opt/tcdi/xstartup.sh
# Prune what the packages ship but this desktop must not offer: the
# screensaver autostart (xfce4-session ships it; the TryExec would not
# resolve anyway), the Log Out entry, the mail-reader launcher (no mail
# client) and the Python interpreter's menu entry (python3 is only here
# because xfce4-settings depends on it).
# The two default keyboard shortcuts that reach log-out/lock and the
# log-out and shutdown-helper binaries go too: a log-out ends the session
# (the container would restart) and there is no logind, polkit or sudo
# for a shutdown to reach anyway - the buttons simply must not exist.
# Nothing in the desktop needs a setuid/setgid bit (no sudo, no polkit
# agent, Firefox ESR uses seccomp-bpf, not a setuid sandbox helper).
RUN chmod 755 /opt/tcdi/xstartup.sh \
 && rm -f /etc/xdg/autostart/xscreensaver.desktop \
          /usr/share/applications/xfce4-session-logout.desktop \
          /usr/share/applications/xfce4-mail-reader.desktop \
          /usr/share/applications/python3.11.desktop \
          /usr/bin/xfce4-session-logout /usr/bin/xflock4 \
          /usr/lib/x86_64-linux-gnu/xfce4/session/xfsm-shutdown-helper \
 && sed -i -e '/xfce4-session-logout/d' -e '/xflock4/d' \
      /etc/xdg/xfce4/xfconf/xfce-perchannel-xml/xfce4-keyboard-shortcuts.xml \
 && ! grep -rqE 'xfce4-session-logout|xflock4' /etc/xdg/xfce4/xfconf \
 && find / -xdev -type f \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} +

USER 1000:1000
