#!/usr/bin/env bash
# clipboard-policy.test.sh — contract coverage for the runtime clipboard
# wiring: the template spec.clipboardPolicy reaches the pod as
# TCDI_CLIPBOARD_POLICY and BOTH session entrypoints render the KasmVNC
# data_loss_prevention block from one shared function:
#
#   build/linux-base/entrypoint.sh    -> $HOME/.vnc/kasmvnc.yaml (merges
#                                        after /etc and wins per key)
#   build/kasm-adapter/entrypoint.sh  -> $HOME/.vnc/kasmvnc.yaml (the image's
#                                        /etc file is untrusted there)
#
# Direction semantics are defined once in internal/api/openapi.yaml
# (user's view): Send = client->workspace = client_to_server; Receive =
# workspace->client = server_to_client; Bidirectional = both; Disabled =
# neither; primary_clipboard_enabled follows server_to_client.
# Unset/unknown renders Disabled (fail closed), DLP logging stays off and
# client-side overrides stay disabled.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BASE_EP="$ROOT/build/linux-base/entrypoint.sh"
ADAPTER_EP="$ROOT/build/kasm-adapter/entrypoint.sh"
SYS_YAML="$ROOT/build/linux-base/kasmvnc.yaml"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

bad() { echo "FAIL: $1"; fails=1; }
ok() { echo "ok: $1"; }

# ---- 1. lint -------------------------------------------------------------
for f in "$BASE_EP" "$ADAPTER_EP"; do
  bash -n "$f" || bad "bash -n $f"
done
command -v shellcheck >/dev/null 2>&1 || bad "shellcheck not installed"
# -S warning: the entrypoints carry grandfathered SC2015 info notes; this
# gate guards errors/warnings, not a style re-litigation of existing code.
shellcheck -x -S warning "$BASE_EP" "$ADAPTER_EP" || bad "shellcheck"
ok "lint"

# ---- 2. both entrypoints carry the same render function ------------------
extract() { awk '/^tcdi_clipboard_dlp\(\)/,/^}/' "$1"; }
base_fn="$(extract "$BASE_EP")"
adapter_fn="$(extract "$ADAPTER_EP")"
[ -n "$base_fn" ] || bad "tcdi_clipboard_dlp missing from $BASE_EP"
[ -n "$adapter_fn" ] || bad "tcdi_clipboard_dlp missing from $ADAPTER_EP"
if [ "$base_fn" != "$adapter_fn" ]; then
  bad "tcdi_clipboard_dlp drifted between linux-base and kasm-adapter entrypoints"
fi
# Both entrypoints must actually emit the rendered block into the config
# they write (a dead function is not a wiring).
grep -q '$(tcdi_clipboard_dlp)' "$BASE_EP" ||
  bad "linux-base entrypoint does not emit \$(tcdi_clipboard_dlp) into \$HOME/.vnc/kasmvnc.yaml"
grep -q '$(tcdi_clipboard_dlp)' "$ADAPTER_EP" ||
  bad "kasm-adapter entrypoint does not emit \$(tcdi_clipboard_dlp) into the rewritten kasmvnc.yaml"
ok "shared render function, invoked by both entrypoints"

# ---- 3. render contract per policy value ----------------------------------
eval "$base_fn"

# dlp_val <yaml-on-stdin> <server_to_client|client_to_server> <key>
# prints the scalar of <key> inside the named clipboard section.
dlp_val() {
  awk -v sec="$2" -v key="$3" '
    $0 ~ "^    "sec":" { insec=1; next }
    insec && /^    [a-z]/ { insec=0 }
    insec && $1 == key":" { print $2; exit }' <<<"$1"
}

check() { # <policy-arg> <want-s2c> <want-primary> <want-c2s> [label]
  local policy="$1" ws2c="$2" wpri="$3" wc2s="$4" label="${5:-$1}"
  local out rc=0
  if [ "$policy" = "__UNSET__" ]; then
    out="$(unset TCDI_CLIPBOARD_POLICY; tcdi_clipboard_dlp)" || rc=$?
  else
    out="$(TCDI_CLIPBOARD_POLICY="$policy" tcdi_clipboard_dlp)" || rc=$?
  fi
  [ "$rc" = 0 ] || bad "render '$label' exited $rc"
  local s2c pri c2s
  s2c="$(dlp_val "$out" server_to_client enabled)"
  pri="$(dlp_val "$out" server_to_client primary_clipboard_enabled)"
  c2s="$(dlp_val "$out" client_to_server enabled)"
  [ "$s2c" = "$ws2c" ] || bad "'$label': server_to_client.enabled=$s2c, want $ws2c"
  [ "$pri" = "$wpri" ] || bad "'$label': primary_clipboard_enabled=$pri, want $wpri (follows server_to_client)"
  [ "$c2s" = "$wc2s" ] || bad "'$label': client_to_server.enabled=$c2s, want $wc2s"
  grep -q 'level: off' <<<"$out" || bad "'$label': data_loss_prevention.logging.level must stay off"
  grep -q '^data_loss_prevention:$' <<<"$out" || bad "'$label': render must emit the data_loss_prevention root key"
}

check Disabled      false false false
check Send          false false true
check Receive       true  true  false
check Bidirectional true  true  true
check __UNSET__     false false false "unset"
check ""            false false false "empty"
check garbage       false false false "garbage"
check send          false false false "lowercase-garbage"
check "'; rm -rf /" false false false "injection-garbage"
ok "render matrix (4 policies + unset/empty/garbage, fail closed)"

# unknown values must warn, not silently accept
warn="$(TCDI_CLIPBOARD_POLICY=garbage tcdi_clipboard_dlp 2>&1 >/dev/null)"
grep -qi "unknown TCDI_CLIPBOARD_POLICY" <<<"$warn" ||
  bad "unknown policy must log a fail-closed warning, got: $warn"
ok "unknown policy warns"

# ---- 4. hardening pins ----------------------------------------------------
# The client must never loosen server policy, on either path.
grep -q 'allow_client_to_override_kasm_server_settings: false' "$SYS_YAML" ||
  bad "system kasmvnc.yaml dropped allow_client_to_override_kasm_server_settings: false"
grep -q 'allow_client_to_override_kasm_server_settings: false' "$ADAPTER_EP" ||
  bad "kasm-adapter policy dropped allow_client_to_override_kasm_server_settings: false"
# The system image config stays the fail-closed floor (all off).
for sec in server_to_client client_to_server; do
  [ "$(dlp_val "$(sed -n '/^data_loss_prevention:/,$p' "$SYS_YAML")" "$sec" enabled)" = "false" ] ||
    bad "system kasmvnc.yaml $sec.enabled must stay false (fail-closed floor)"
done
[ "$(dlp_val "$(sed -n '/^data_loss_prevention:/,$p' "$SYS_YAML")" server_to_client primary_clipboard_enabled)" = "false" ] ||
  bad "system kasmvnc.yaml primary_clipboard_enabled must stay false"
# DLP logging off in the system file too (verbose logs keypresses/clipboard).
grep -q 'level: off' < <(sed -n '/^data_loss_prevention:/,$p' "$SYS_YAML") ||
  bad "system kasmvnc.yaml dropped data_loss_prevention.logging.level: off"
# R1-06: without allow_override_standard_vnc_server_settings: true the
# KasmVNC launcher passes NO -AllowOverride, so Xvnc's compiled default
# (desktop,AcceptPointerEvents,SendCutText,AcceptCutText,SendPrimary,
# SetPrimary) lets any X client re-enable the clipboard with
# `vncconfig -set SendCutText=1`. Both configs must turn the override
# list on and leave it empty, so the launcher renders -AllowOverride "".
for f in "$SYS_YAML" "$ADAPTER_EP"; do
  rc_block="$(awk '/^runtime_configuration:/{on=1; print; next} on && /^[^ ]/{exit} on && /^[[:space:]]*#/{next} on{print}' "$f")"
  grep -qx '  allow_override_standard_vnc_server_settings: true' <<<"$rc_block" ||
    bad "$(basename "$(dirname "$f")")/$(basename "$f"): runtime_configuration must set allow_override_standard_vnc_server_settings: true (else Xvnc keeps its default AllowOverride list)"
  grep -qx '  allow_override_list: \[\]' <<<"$rc_block" ||
    bad "$(basename "$(dirname "$f")")/$(basename "$f"): runtime_configuration must set allow_override_list: [] (no runtime-overridable VNC parameter)"
  if grep -qiE 'clipboard|cut_text|primary' <<<"$rc_block"; then
    bad "$(basename "$(dirname "$f")")/$(basename "$f"): runtime_configuration names a clipboard key"
  fi
done
ok "hardening pins (client override off, override list empty, system floor closed, DLP logging off)"

echo
if [ "$fails" = 0 ]; then echo "clipboard-policy.test.sh: PASS"; else echo "clipboard-policy.test.sh: FAIL"; fi
exit "$fails"
