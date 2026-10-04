# Linux runtime base image: everything the TinyCDI runtime contract needs
# and nothing a profile decides.
#
# Profiles build FROM this image: build/browser (kiosk: openbox + Chromium
# + Firefox ESR) and build/linux-desktop (XFCE4 desktop). A custom desktop
# image does the same (docs/images.md, "Build a custom desktop image").
# Contents: KasmVNC (X server + HTTPS/websocket endpoint), X utilities,
# D-Bus, fonts (DejaVu, Noto and WenQuanYi, trimmed to the faces below),
# the entrypoint/healthcheck, the system KasmVNC policy, the non-root
# workspace user and the setuid/setgid strip. No window manager,
# no terminal, no browser.
#
# Pinned per docs/compatibility.md:
#   - base image: debian:bookworm-slim by digest
#   - KasmVNC: upstream 1.5.0 .deb verified by sha256
#
# Contract (see docs/images.md; unchanged by the base/profile split):
#   - HTTPS streaming endpoint on :8443 inside the container (websocket
#     transport only; KasmVNC 1.5.0 has no HTTP tunnel fallback).
#   - HEALTHCHECK passes only while the X display AND the endpoint answer.
#   - Credentials arrive as mounted Secret files under /run/secrets/tcdi
#     (never env, never logged); runtime credential material lives on the
#     ephemeral /run/tcdi mount, outside the persistent home.
#   - Home is /home/workspace (persistent mount point); run as uid 1000.
#   - The session is /opt/tcdi/xstartup.sh; a profile replaces it. The
#     base default only paints a plain root window and idles, so a profile
#     that forgets its xstartup is visibly empty rather than broken.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

ARG KASMVNC_VERSION=1.5.0
ARG KASMVNC_DEB_SHA256=770fd3df51510beecc89666879d82faf411276e68c6e11df612f736b891b5f71
ARG KASMVNC_DEB_URL=https://github.com/kasmtech/KasmVNC/releases/download/v${KASMVNC_VERSION}/kasmvncserver_bookworm_${KASMVNC_VERSION}_amd64.deb
# apt pin per docs/compatibility.md: the base
# digest ships 2.5.0-1+deb12u3; deb12u4 carries the pending HIGH fixes.
ARG LIBEXPAT1_APT_VERSION=2.5.0-1+deb12u4
# apt pin per docs/compatibility.md: the base digest ships
# 10.42-1+deb12u1; deb12u2 fixes CVE-2026-103111 (HIGH).
ARG LIBPCRE2_APT_VERSION=10.42-1+deb12u2
ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl \
      x11-xserver-utils x11-utils dbus-x11 \
      fontconfig fonts-dejavu-core fonts-noto-core fonts-noto-mono \
      fonts-wqy-microhei \
      perl libswitch-perl libyaml-tiny-perl libhash-merge-simple-perl \
      libscalar-list-utils-perl liblist-moreutils-perl libtry-tiny-perl \
      libdatetime-perl libdatetime-timezone-perl \
      ssl-cert xauth x11-xkb-utils xkb-data procps \
      libbsd0 libcrypt1 "libexpat1=${LIBEXPAT1_APT_VERSION}" libfreetype6 \
      libgbm1 libgl1 "libpcre2-8-0=${LIBPCRE2_APT_VERSION}" libpixman-1-0 \
      libpng16-16 libsystemd0 libunwind8 libx11-6 libxau6 libxcursor1 \
      libxdmcp6 libxext6 libxfixes3 libxfont2 libxrandr2 libxshmfence1 \
      libxtst6 zlib1g libssl3 \
 && curl -fsSL -o /tmp/kasmvncserver.deb "$KASMVNC_DEB_URL" \
 && echo "${KASMVNC_DEB_SHA256}  /tmp/kasmvncserver.deb" | sha256sum -c - \
 && apt-get install -y --no-install-recommends /tmp/kasmvncserver.deb \
 && rm -f /tmp/kasmvncserver.deb \
# fonts-noto-core ships 268 faces covering every Unicode script; keep the
# Latin/Greek/Cyrillic core (Vietnamese included) plus the
# fonts-noto-mono faces — DejaVu Sans Mono lacks the Vietnamese block
# (U+1EA0-1EF9), so without Noto Sans Mono fontconfig resolves
# monospace:lang=vi to the proportional DejaVu Sans. CJK is
# fonts-wqy-microhei: the smallest package covering zh/ja/ko — the Noto
# CJK faces alone would exceed the browser image's +2 % size budget
# (docs/images.md). Installed unpinned like the other runtime-base
# packages.
 && find /usr/share/fonts/truetype/noto -type f \
      ! -name 'NotoSans-*.ttf' ! -name 'NotoSerif-*.ttf' \
      ! -name 'NotoSansMono-*.ttf' ! -name 'NotoMono-*.ttf' -delete \
 && fc-cache -f >/dev/null \
 && rm -f /etc/ssl/certs/ssl-cert-snakeoil.pem \
          /etc/ssl/private/ssl-cert-snakeoil.key \
 && rm -rf /var/lib/apt/lists/*

COPY LICENSE NOTICE THIRD_PARTY_LICENSES.md SOURCE-OFFER /usr/share/doc/tinycdi/
COPY build/linux-base/kasmvnc.yaml /etc/kasmvnc/kasmvnc.yaml
COPY build/linux-base/entrypoint.sh build/linux-base/healthcheck.sh build/linux-base/xstartup.sh /opt/tcdi/
RUN chmod 755 /opt/tcdi/entrypoint.sh /opt/tcdi/healthcheck.sh /opt/tcdi/xstartup.sh \
 && useradd -m -u 1000 -d /home/workspace -s /bin/bash workspace \
 && mkdir -p /run/tcdi /run/secrets/tcdi \
 && chown workspace:workspace /run/tcdi \
 && find / -xdev -type f \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} +

USER 1000:1000
ENV HOME=/home/workspace
EXPOSE 8443
VOLUME ["/home/workspace"]

HEALTHCHECK --interval=5s --timeout=5s --start-period=25s --retries=6 \
  CMD /opt/tcdi/healthcheck.sh

ENTRYPOINT ["/opt/tcdi/entrypoint.sh"]
