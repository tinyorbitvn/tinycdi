#!/bin/bash
# Copyright (c) 2026 TinyOrbit
# SPDX-License-Identifier: MIT
#
# TinyCDI runtime adapter for unmodified kasmweb/* workspace images.
#
# Replaces /dockerstartup/vnc_startup.sh wholesale: Kasm's startup creates an
# OWNER-capable kasm_user (-wo), a self-signed cert, port 6901, and a fleet of
# side services (audio/upload/gamepad/webcam/printer/smartcard). None of that
# matches the TinyCDI runtime contract, so this adapter drives kasmvncserver
# directly with the same contract as build/linux-base/entrypoint.sh:
#
#   1. Credentials/TLS arrive as mounted Secret FILES ($TCDI_SECRET_DIR).
#   2. Runtime material lives on the ephemeral /run/tcdi mount.
#   3. Stale $HOME files (~/.kasmpasswd, ~/.vnc/kasmvnc.yaml) are neutralized:
#      the home volume is persistent and must never override the Secret or
#      the policy written here. The image's /etc/kasmvnc/kasmvnc.yaml is
#      Kasm's permissive file AND world-writable (0666 in the kasmweb
#      images), so the FULL enforceable policy is written into
#      $HOME/.vnc/kasmvnc.yaml on every boot — it merges after /etc and
#      wins for every key we set. The file is written via rm+rename so a
#      planted symlink on the persistent home can never redirect it.
#   4. Session runs with HOME=/home/workspace (the mounted volume), seeded
#      from /home/kasm-default-profile on first boot.
#   5. KasmVNC :1, HTTPS websocket on :8443, -SecurityTypes None (auth at the
#      HTTPS layer via write-only kasmpasswd; RFB port is never published).
set -euo pipefail

SECRET_DIR="${TCDI_SECRET_DIR:-/run/secrets/tcdi}"
PASS_FILE="$SECRET_DIR/password"
USER_FILE="$SECRET_DIR/username"
CERT_FILE="$SECRET_DIR/tls.crt"
KEY_FILE="$SECRET_DIR/tls.key"
RT=/run/tcdi
DISPLAY_NUM=1

fail() { echo "tcdi-kasm-adapter: $*" >&2; exit 1; }

[ -f "$PASS_FILE" ] || fail "mounted Secret file missing: $PASS_FILE"
[ -f "$CERT_FILE" ] || fail "mounted Secret file missing: $CERT_FILE"
[ -f "$KEY_FILE" ]  || fail "mounted Secret file missing: $KEY_FILE"
[ -d "$RT" ] && [ -w "$RT" ] || fail "$RT is not a writable mount (need tmpfs/emptyDir owned by uid $(id -u))"

# TinyCDI mounts the home volume at /home/workspace regardless of the
# image's native home (/home/kasm-user). Session state, KasmVNC dotfiles and
# app profiles all live on the persistent volume.
export HOME="${TCDI_HOME:-/home/workspace}"
mkdir -p "$HOME" || fail "home mount $HOME not writable by uid $(id -u)"

KASMVNC_USER="kasm_user"
if [ -f "$USER_FILE" ]; then
  KASMVNC_USER="$(tr -d '[:space:]' < "$USER_FILE")"
fi
[ -n "$KASMVNC_USER" ] || fail "$USER_FILE is empty"

# Seed the Kasm default profile on first boot (kasm_default_profile.sh
# equivalent): desktop icons, mime handlers, panel config shipped under
# /home/kasm-default-profile at image build time.
if [ ! -e "$HOME/.tcdi-profile-seeded" ] && [ -d /home/kasm-default-profile ]; then
  # --no-preserve=mode: the kasmweb default-profile tree is mode 0777;
  # copying modes would leave $HOME and ~/.config world-writable.
  cp -r --no-preserve=mode /home/kasm-default-profile/. "$HOME/" 2>/dev/null || true
  chmod -R go-w "$HOME" 2>/dev/null || true
  touch "$HOME/.tcdi-profile-seeded"
fi
# ~/.vnc must be a real directory, not a planted symlink into the rootfs
# or the runtime mount — kasmvnc.yaml and the rfb passwd land inside it.
[ -L "$HOME/.vnc" ] && rm -f "$HOME/.vnc"
mkdir -p "$HOME/.vnc" "$HOME/Desktop"

cp "$CERT_FILE" "$RT/tls.crt"
cp "$KEY_FILE" "$RT/tls.key"
chmod 600 "$RT/tls.key"
chmod 644 "$RT/tls.crt"

# Single write-only (non-owner) user; never -o (ADR 0001: owner rights unlock
# the /api/* management surface).
{ cat "$PASS_FILE"; cat "$PASS_FILE"; } | kasmvncpasswd -u "$KASMVNC_USER" -w "$RT/kasmpasswd" >/dev/null
chmod 600 "$RT/kasmpasswd"
rm -rf "$HOME/.kasmpasswd"
ln -s "$RT/kasmpasswd" "$HOME/.kasmpasswd"

# Tell the browser shim (mounted over the image's --no-sandbox Chromium
# wrappers) which binary the session payload runs, so every relaunch path
# execs the same sandboxed binary. Only a leading absolute path counts;
# anything else falls back to the shim's built-in candidate list.
if [ -n "${TCDI_SESSION_CMD:-}" ]; then
  first=""
  IFS=' ' read -r first _ <<< "$TCDI_SESSION_CMD"
  case "$first" in
    /*) [ -x "$first" ] && printf '%s\n' "$first" > "$RT/browser-bin" ;;
  esac
fi

# The image's Kasm wrapper removes stale Chromium Singleton* locks and
# rewrites the crash markers before every launch — with the wrapper
# bypassed, the same cleanup must happen here or a restarted pod finds a
# locked profile (Singleton* persists on the home volume) and never opens
# a window.
for d in "$HOME"/.config/chromium "$HOME"/.config/google-chrome \
         "$HOME"/.config/microsoft-edge; do
  rm -f "$d"/Singleton* 2>/dev/null || true
  p="$d/Default/Preferences"
  [ -f "$p" ] && sed -i 's/"exited_cleanly":false/"exited_cleanly":true/; s/"exit_type":"Crashed"/"exit_type":"None"/' "$p" 2>/dev/null || true
done

# Full policy, rewritten every boot: merges after the image's
# /etc/kasmvnc/kasmvnc.yaml so every security-relevant key is pinned.
# server.allow_environment_variables_to_override_config_settings exists only
# on KasmVNC >= 1.5; 1.4.x rejects it as an unsupported key (and cannot be
# env-overridden anyway). Include it only when the image's schema has it.
ENV_OVR=""
if grep -q 'allow_environment_variables_to_override_config_settings' \
    /usr/share/kasmvnc/kasmvnc_defaults.yaml 2>/dev/null; then
  ENV_OVR="  allow_environment_variables_to_override_config_settings: false"
fi
# Write via temp file + rename: a planted ~/.vnc/kasmvnc.yaml symlink on
# the persistent home (e.g. -> /dev/null or -> $RT/tls.key) must never be
# followed — it would crash-loop the display or corrupt runtime files.
VNC_CFG="$HOME/.vnc/kasmvnc.yaml"
VNC_TMP="$HOME/.vnc/.kasmvnc.yaml.$$"
rm -rf "$VNC_CFG"
cat > "$VNC_TMP" <<YAML
# Managed by the tcdi kasm-adapter - rewritten on every container start.
desktop:
  resolution:
    width: 1280
    height: 800
  allow_resize: true
  pixel_depth: 24
  gpu:
    hw3d: false
network:
  protocol: http
  interface: 0.0.0.0
  websocket_port: 8443
  use_ipv4: true
  use_ipv6: false
  # NOTE: KasmVNC 1.4.x has no off-switch for its UDP transport —
  # network.udp.port accepts only 'auto' or an integer (0 resolves to the
  # websocket port), so Xvnc still binds :8443/udp. The NetworkPolicy is
  # TCP-only so nothing can reach it; the contract test asserts no OTHER
  # UDP socket exists. Revisit on a KasmVNC bump (docs/kasm-images.md).
  ssl:
    pem_certificate: /run/tcdi/tls.crt
    pem_key: /run/tcdi/tls.key
    require_ssl: true
command_line:
  prompt: false
data_loss_prevention:
  clipboard:
    server_to_client:
      enabled: false
      primary_clipboard_enabled: false
    client_to_server:
      enabled: false
  logging:
    level: off
runtime_configuration:
  allow_client_to_override_kasm_server_settings: false
  allow_override_standard_vnc_server_settings: false
user_session:
  new_session_disconnects_existing_exclusive_session: true
  concurrent_connections_prompt: false
server:
  http:
    headers:
      - Cross-Origin-Embedder-Policy=require-corp
      - Cross-Origin-Opener-Policy=same-origin
    httpd_directory: /usr/share/kasmvnc/www
  advanced:
    kasm_password_file: /run/tcdi/kasmpasswd
  auto_shutdown:
    no_user_session_timeout: never
    active_user_session_timeout: never
    inactive_user_session_timeout: never
${ENV_OVR}
security:
  brute_force_protection:
    blacklist_threshold: 5
    blacklist_timeout: 10
logging:
  log_writer_name: all
  log_dest: logfile
  level: 30
YAML
mv -f "$VNC_TMP" "$VNC_CFG"

# CLI flags override yaml - pin the port/interface/TLS and the managed
# xstartup so a stale $HOME/.vnc/xstartup can never take over the session.
# -select-de manual is required by KasmVNC 1.4.x to honor -xstartup.
kasmvncserver ":$DISPLAY_NUM" \
  -xstartup /opt/tcdi/xstartup.sh \
  -select-de manual \
  -SecurityTypes None \
  -websocketPort 8443 \
  -interface 0.0.0.0 \
  -sslOnly >> "$HOME/.vnc/entrypoint.log" 2>&1

shutdown() {
  kasmvncserver -kill ":$DISPLAY_NUM" >/dev/null 2>&1 || true
  exit 0
}
trap shutdown TERM INT

tail -F "$HOME"/.vnc/*.log 2>/dev/null &
TAILPID=$!

while pgrep -x Xvnc >/dev/null 2>&1; do sleep 2; done
sleep 12
kill "$TAILPID" 2>/dev/null || true
shutdown
