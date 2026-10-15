#!/usr/bin/env bash
# allowlist-grep.sh — fail on every match of an ERE in the files a git
# pathspec selects (tracked, plus untracked files not ignored, so a new file
# is checked before `git add`), unless a reviewed allowlist entry covers it.
#
#   allowlist-grep.sh -p '<ERE>' -a <allowlist> [-i] [-s] [-r <repo root>] -- <pathspec>...
#
#   -i  case-insensitive match of <ERE>
#   -s  skip the stale-entry check (use when checking part of the scope)
#
# Allowlist: one entry per line, "<glob> :: <ERE> :: <reason>"; blank lines
# and lines starting with # are ignored. A hit is allowed when its path
# matches <glob> (bash [[ == ]] semantics: * also matches /) and the hit's
# line matches <ERE> (case-sensitive). The reason must be at least 10
# characters. Unless -s is given, an entry that covers no hit is stale and
# fails the run. Exit: 0 clean, 1 findings, 2 usage.
set -uo pipefail

usage() { echo "usage: $0 -p ERE -a allowlist [-i] [-s] [-r root] -- pathspec..." >&2; exit 2; }
pat="" allow="" icase="" skipstale=0 root="."
while getopts "p:a:isr:" o; do
  case "$o" in
    p) pat="$OPTARG" ;;
    a) allow="$OPTARG" ;;
    i) icase="-i" ;;
    s) skipstale=1 ;;
    r) root="$OPTARG" ;;
    *) usage ;;
  esac
done
shift $((OPTIND - 1))
[ "${1:-}" = "--" ] && shift
{ [ -n "$pat" ] && [ -n "$allow" ] && [ "$#" -gt 0 ]; } || usage
[ -f "$allow" ] || { echo "FAIL: allowlist $allow not found"; exit 1; }

globs=() eres=() used=() fails=0
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in ''|'#'*) continue ;; esac
  g="${line%% :: *}"; rest="${line#* :: }"
  e="${rest%% :: *}"; why="${rest#* :: }"
  if [ "$g" = "$line" ] || [ "$e" = "$rest" ] || [ "${#why}" -lt 10 ]; then
    echo "FAIL: malformed allowlist entry (want '<glob> :: <ERE> :: <reason of 10+ chars>'): $line"
    fails=1; continue
  fi
  globs+=("$g"); eres+=("$e"); used+=(0)
done < "$allow"

while IFS= read -r -d '' f; do
  while IFS= read -r hit; do
    n="${hit%%:*}"; text="${hit#*:}"; ok=0
    for i in "${!globs[@]}"; do
      # shellcheck disable=SC2053  # the glob is intentionally unquoted
      if [[ "$f" == ${globs[$i]} ]] && grep -qE -- "${eres[$i]}" <<< "$text"; then
        used[$i]=1; ok=1
      fi
    done
    [ "$ok" -eq 1 ] || { echo "FAIL: $f:$n: ${text:0:160}"; fails=1; }
  done < <(grep -nIE $icase -- "$pat" "$root/$f" 2>/dev/null)
done < <(git -C "$root" ls-files -z --cached --others --exclude-standard -- "$@")

if [ "$skipstale" -eq 0 ]; then
  for i in "${!globs[@]}"; do
    [ "${used[$i]}" -eq 1 ] || { echo "FAIL: stale allowlist entry (covers no hit): ${globs[$i]} :: ${eres[$i]}"; fails=1; }
  done
fi
exit "$fails"
