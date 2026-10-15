#!/usr/bin/env bash
# check-workflow-expressions.sh — fail when a workflow value holds GitHub
# expression syntax that is NOT wrapped in ${{ }}.
#
# Why: outside `if:` (whose value is an implicit expression), GitHub passes
# the text through literally. `outputs: format('type=registry,name={0}', env.X)`
# without ${{ }} reaches buildx as that exact string and the step dies with
# `invalid value  env.X)` — which is how the runtime train's first publish
# run failed (FX-R16). actionlint does not flag it: it is valid plain text.
#
# Scanned: every scalar value (inline, folded `>-`, literal `|`) except the
# keys where free text is legitimate — `run` (shell), `if` (implicit
# expression), `name`, `description`, `cron`. After removing every
# ${{ ... }} span the remainder must contain neither
#   - an expression function call: format( contains( startsWith( endsWith(
#     join( toJSON( fromJSON( hashFiles(
#   - a context reference: github. env. inputs. matrix. steps. needs.
#     secrets. vars. runner. job. strategy.   (github.com/io/dev excluded;
#     a reference preceded by a path/word character, e.g. a URL host, is
#     not a context)
#
# Usage: check-workflow-expressions.sh [workflow.yml ...]
#   default: every .github/workflows/*.yml
# Exit: 0 clean, 1 findings (file:line: key: text), 2 usage.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if [ "$#" -eq 0 ]; then
  shopt -s nullglob
  set -- "$ROOT"/.github/workflows/*.yml
fi
[ "$#" -gt 0 ] || { echo "::error::no workflow files"; exit 2; }

exec python3 - "$@" <<'PY'
import re
import sys

SKIP_KEYS = {"run", "if", "name", "description", "cron"}
KEY_RE = re.compile(r"^(?P<indent>\s*)(?P<dash>-\s+)?(?P<key>[A-Za-z0-9_.\-]+):(?:\s+(?P<val>.*))?$")
BLOCK_RE = re.compile(r"^[>|][+-]?\d*$")
EXPR_SPAN = re.compile(r"\$\{\{.*?\}\}", re.S)
FUNC_RE = re.compile(r"(?<![\w.])(format|contains|startsWith|endsWith|join|toJSON|fromJSON|hashFiles)\(")
CTX_RE = re.compile(
    r"(?<![\w.$/\-])(github|env|inputs|matrix|steps|needs|secrets|vars|runner|job|strategy)"
    r"\.(?!(?:com|io|dev|org)\b)[A-Za-z_*]"
)


def mask(text):
    """Blank every ${{ ... }} span (newlines kept so line numbers hold)."""
    return EXPR_SPAN.sub(lambda m: re.sub(r"[^\n]", " ", m.group(0)), text)


def scalars(lines):
    """Yield (key, first_line_no, [text per line]) for each checkable value."""
    i = 0
    while i < len(lines):
        line = lines[i]
        m = KEY_RE.match(line)
        if line.lstrip().startswith("#") or not m:
            i += 1
            continue
        key, val = m.group("key"), (m.group("val") or "").strip()
        base = len(m.group("indent")) + (len(m.group("dash")) if m.group("dash") else 0)
        if val and BLOCK_RE.match(re.sub(r"\s#.*$", "", val)):
            # block scalar: every following line indented deeper than the key
            j = i + 1
            while j < len(lines) and (not lines[j].strip() or len(lines[j]) - len(lines[j].lstrip(" ")) > base):
                j += 1
            if key not in SKIP_KEYS:
                yield key, i + 2, lines[i + 1:j]
            i = j
            continue
        if val:
            if not val.startswith(("'", '"')):
                val = re.sub(r"\s#.*$", "", val)
            body, j = [val], i + 1
            # an inline ${{ opened but not closed continues on the next lines
            while "${{" in mask_open(body) and j < len(lines):
                body.append(lines[j])
                j += 1
            if key not in SKIP_KEYS:
                yield key, i + 1, body
            i = j
            continue
        i += 1


def mask_open(body):
    """Text with closed spans removed; an unclosed `${{` survives."""
    return EXPR_SPAN.sub("", "\n".join(body))


rc = 0
for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as fh:
        lines = fh.read().split("\n")
    for key, first, body in scalars(lines):
        for off, text in enumerate(mask("\n".join(body)).split("\n")):
            if FUNC_RE.search(text) or CTX_RE.search(text):
                print(f"{path}:{first + off}: {key}: expression syntax outside ${{{{ }}}}: {body[off].strip()}")
                rc = 1
sys.exit(rc)
PY
