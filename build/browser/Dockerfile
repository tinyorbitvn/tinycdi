# Browser workspace profile: the linux-base runtime plus the openbox kiosk
# window manager and pinned browsers.
#
# Builds on the local linux-base image (same KasmVNC/HTTPS/healthcheck/
# credential contract); override BASE_IMAGE to consume a registry digest.
# The session script launches the policy-selected browser maximized.
# This image's content is the pre-split linux-desktop+browser content
# (V3.26 moved openbox/xterm here from the base): the package list and
# size are asserted against tests/integration/testdata/browser-baseline.json.
#
# Browser pins (docs/compatibility.md):
#   - chromium: primary browser. Its sandbox (nested userns + seccomp-bpf)
#     requires the node Localhost seccomp+AppArmor pair on Kubernetes
#     nodes; for local Docker runs pass --security-opt
#     seccomp=tests/integration/testdata/seccomp-runtime.json.
#   - firefox-esr: measured no-node-mutation fallback (reduced isolation —
#     seccomp-bpf only, no userns layer).
ARG BASE_IMAGE=ghcr.io/tinyorbitvn/tinycdi-linux-base:local
FROM ${BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
# apt pins per docs/compatibility.md (browser pins) and the locked
# firefox-esr deb.
ARG CHROMIUM_APT_VERSION=154.0.8037.92-1~deb12u1
ARG FIREFOX_ESR_APT_VERSION=153.4.0esr-1~deb12u1

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      openbox xterm \
      "chromium=${CHROMIUM_APT_VERSION}" \
      "chromium-sandbox=${CHROMIUM_APT_VERSION}" \
      "firefox-esr=${FIREFOX_ESR_APT_VERSION}" \
 && rm -rf /var/lib/apt/lists/*

# Ship the license/notice set inside the image even when BASE_IMAGE is
# overridden to a build that predates the linux-base notices.
COPY LICENSE NOTICE THIRD_PARTY_LICENSES.md SOURCE-OFFER /usr/share/doc/tinycdi/
COPY build/browser/openbox-rc.xml /etc/xdg/openbox/rc.xml
COPY build/browser/xstartup.sh /opt/tcdi/xstartup.sh
# chrome-sandbox keeps its setuid bit (chromium's sandbox helper needs it);
# nothing else in this layer is setuid/setgid.
RUN chmod 755 /opt/tcdi/xstartup.sh \
 && find / -xdev -type f \( -perm -4000 -o -perm -2000 \) \
      ! -path /usr/lib/chromium/chrome-sandbox -exec chmod a-s {} +

USER 1000:1000
