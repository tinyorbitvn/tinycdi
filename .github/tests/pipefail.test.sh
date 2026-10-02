#!/usr/bin/env bash
# pipefail.test.sh — a `run:` step whose script pipes one command into
# another must run under a shell with pipefail semantics. GitHub's
# unspecified default is `bash -e` (no pipefail), which lets a failing
# left-hand command exit 0 and silently masks a gate failure (KASM-4:
# `go test | tee` in kasm-contract exited 0 while the test failed).
#
# Coverage = `shell: bash` on the step itself, or `defaults.run.shell:
# bash` on the job, or the same at workflow level (`bash` selects
# `bash --noprofile --norc -eo pipefail` on the ubuntu runners).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
fails=0

for wf in "$ROOT"/.github/workflows/*.yml; do
  # Indentation-aware scan: GitHub layout is jobs at 2 cols, job keys at
  # 4, step items at 6 ("- key:"), step keys at 8, run-script lines
  # deeper than 8. A "pipeline" is a '|' adjacent to whitespace after
  # '||' and '\|' are stripped — that catches `cmd | tee`, leading `|`
  # continuations and quoted `a | b` while ignoring regex alternation
  # ('a|b') and `case` globs (schedule|push).
  out="$(awk '
    function flush(  ) {
      if (inrun && haspipe && !(wf_shell || job_shell || step_shell))
        printf "%d\n", runline
      inrun = 0; haspipe = 0; step_shell = 0
    }
    function scanline(s,   t) {
      t = s
      gsub(/\|\|/, "", t); gsub(/\\\|/, "", t)
      if (t ~ /[[:space:]][|]/ || t ~ /[|][[:space:]]/) haspipe = 1
    }
    {
      # still inside a run block: deeper-indented lines are script text
      if (inrun && ($0 ~ /^[[:space:]]{9,}/ || $0 ~ /^[[:space:]]*$/)) {
        scanline($0); next
      }
      if (inrun) flush()

      # ---- column-0 keys
      if (/^defaults:[[:space:]]*$/) { wfd=1; wfr=0; next }
      if (/^[^[:space:]#]/) {
        wfd=0; wfr=0; jd=0; jr=0
        injobs = ($0 ~ /^jobs:[[:space:]]*$/)
        next
      }

      # ---- workflow-level defaults.run.shell: bash
      if (wfd && /^  run:[[:space:]]*$/)            { wfr=1; next }
      if (wfd && /^  [^[:space:]]/)                 { wfr=0 }
      if (wfd && wfr && /^    shell:[[:space:]]*bash[[:space:]]*$/) wf_shell=1

      # ---- job boundary + job-level defaults.run.shell: bash
      if (injobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/) {
        job_shell=0; jd=0; jr=0; next
      }
      if (injobs && /^    defaults:[[:space:]]*$/)  { jd=1; jr=0; next }
      if (injobs && jd && /^      run:[[:space:]]*$/) { jr=1; next }
      if (injobs && jd && /^      [^[:space:]]/)    { jr=0 }
      if (injobs && jd && jr &&
          /^        shell:[[:space:]]*bash[[:space:]]*$/) job_shell=1
      if (injobs && /^    [A-Za-z0-9_-]+:/ && $0 !~ /^    defaults:/) jd=0

      # ---- steps
      if (/^      - /) {
        flush()
        rest = $0; sub(/^      - /, "", rest)
        if (rest ~ /^run:/) {
          inrun=1; runline=NR; haspipe=0
          sub(/^run:[[:space:]]*/, "", rest)
          if (rest !~ /^[|>][-+0-9]*[[:space:]]*$/) scanline(rest)
        }
        next
      }
      if (/^        shell:[[:space:]]*/) {
        step_shell = ($0 ~ /^        shell:[[:space:]]*bash[[:space:]]*$/)
        next
      }
      if (/^        run:/) {
        inrun=1; runline=NR; haspipe=0
        rest = $0; sub(/^        run:[[:space:]]*/, "", rest)
        if (rest !~ /^[|>][-+0-9]*[[:space:]]*$/) scanline(rest)
        next
      }
    }
    END { flush() }
  ' "$wf")"
  if [ -n "$out" ]; then
    while IFS= read -r l; do
      echo "FAIL: $(basename "$wf"):$l run step pipes but has no pipefail shell (shell: bash / defaults.run.shell: bash)"
    done <<< "$out"
    fails=1
  fi
done

[ "$fails" -eq 0 ] && echo "pipefail: every piped run step has pipefail semantics"
exit "$fails"
