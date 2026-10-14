#!/usr/bin/env bash
# xfer-entrypoint.test.sh — contract coverage for the in-pod transfer
# helper wiring (ADR 0009 §5 (i)): both runtime entrypoints start
# tcdi-xferd, supervise it next to Xvnc and stop it on shutdown; KasmVNC
# listens on 127.0.0.1:8444 only; the images ship the helper.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BASE_EP="$ROOT/build/linux-base/entrypoint.sh"
ADAPTER_EP="$ROOT/build/kasm-adapter/entrypoint.sh"
SYS_YAML="$ROOT/build/linux-base/kasmvnc.yaml"
fails=0
bad() { echo "FAIL: $1"; fails=1; }
ok() { echo "ok: $1"; }

for f in "$BASE_EP" "$ADAPTER_EP"; do
  bash -n "$f" || bad "bash -n $f"
done
command -v shellcheck >/dev/null 2>&1 || bad "shellcheck not installed"
shellcheck -x -S warning "$BASE_EP" "$ADAPTER_EP" || bad "shellcheck"
ok "lint"

for f in "$BASE_EP" "$ADAPTER_EP"; do
  grep -q 'XFERD_PID=\$!' "$f" || bad "$f does not start tcdi-xferd in the background"
  grep -q 'kill -0 "\$XFERD_PID"' "$f" || bad "$f does not supervise tcdi-xferd"
  grep -q 'kill "\$XFERD_PID"' "$f" || bad "$f does not stop tcdi-xferd on shutdown"
done
ok "both entrypoints start, supervise and stop the helper"

grep -Eq '^\s*interface: 127\.0\.0\.1' "$SYS_YAML" || bad "linux-base kasmvnc.yaml not loopback-bound"
grep -Eq '^\s*websocket_port: 8444' "$SYS_YAML" || bad "linux-base kasmvnc.yaml not on 8444"
grep -Eq '^\s*interface: 127\.0\.0\.1' "$ADAPTER_EP" || bad "adapter yaml not loopback-bound"
grep -Eq '^\s*websocket_port: 8444' "$ADAPTER_EP" || bad "adapter yaml not on 8444"
grep -q -- '-websocketPort 8444' "$ADAPTER_EP" || bad "adapter kasmvncserver flag not 8444"
grep -q -- '-interface 127.0.0.1' "$ADAPTER_EP" || bad "adapter kasmvncserver still binds beyond loopback"
if grep -Eq 'websocket_port: 8443|-websocketPort 8443|interface: 0\.0\.0\.0|-interface 0\.0\.0\.0' "$SYS_YAML" "$ADAPTER_EP"; then
  bad "a KasmVNC listener is still on :8443 or 0.0.0.0"
fi
ok "KasmVNC loopback-only on 8444"

grep -q 'COPY --from=xferd /out/tcdi-xferd /usr/local/bin/tcdi-xferd' "$ROOT/build/linux-base/Dockerfile" \
  || bad "linux-base image does not ship tcdi-xferd"
grep -q 'go build -trimpath -o tcdi-xferd ./cmd/tcdi-xferd' "$ROOT/build/kasm-adapter/Dockerfile" \
  || bad "kasm-adapter image does not build tcdi-xferd"
grep -q '"tcdi-xferd"' "$ROOT/build/kasm-adapter/main.go" || bad "install-adapter does not install tcdi-xferd"
ok "images ship the helper"

if grep -En 'xfer-token' "$BASE_EP" "$ADAPTER_EP" | grep -Ev '^\S+:\s*#'; then
  bad "an entrypoint reads the transfer credential (only the helper may)"
fi
ok "credential stays inside the helper"

exit "$fails"
