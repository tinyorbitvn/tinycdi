#!/usr/bin/env bash
# go-fuzz-targets.sh — run each native Go fuzz target for $FUZZTIME via
# 'go test -fuzz'. Env: FUZZTIME (required, e.g. 30s | 10m),
# FUZZ_PARALLEL (worker count, defaults to nproc — the runner's cores).
#
# golang/go#75804: when a -fuzztime duration expires while a coordinator
# -> worker call is in flight, the coordinator can surface
# "context deadline exceeded" as a target failure although nothing failed
# — a propagation race between the timeout context and the worker stop
# path, which CI hits under CPU load. A real finding always leaves
# evidence: "Failing input written to testdata/fuzz/<target>/" for a new
# crasher, or "failure while testing seed corpus entry" for a seed. So a
# deadline error with none of that evidence is retried once and annotated
# ::warning::. A second consecutive occurrence fails the job — the race is
# rare, and a recurring identical signature would erode the gate silently.
# Every other failure (crasher, seed failure, panic, build error) fails
# immediately; this script only ever forgives the deadline race.
set -uo pipefail

FUZZTIME="${FUZZTIME:?set FUZZTIME (go -fuzztime value, e.g. 30s or 10m)}"
PARALLEL="${FUZZ_PARALLEL:-$(nproc)}"

cd "$(dirname "$0")/../.." || exit 1

SPECS=(
	"internal/sessionhost FuzzDomainMatch"
	"internal/ratelimit FuzzClientKey"
	"internal/gateway FuzzHostClassify"
	"internal/gateway FuzzLaunchRedeem"
	"internal/api FuzzCallbackParsing"
)

# deadline_flake <log> <corpus-files-before> <corpus-dir> reports whether
# the failed run left only the go.dev/issue/75804 signature.
deadline_flake() {
	local log="$1" before="$2" corpus="$3" after
	grep -q "context deadline exceeded" "$log" || return 1
	! grep -q "Failing input written to" "$log" || return 1
	! grep -q "failure while testing seed corpus entry" "$log" || return 1
	after="$(find "$corpus" -type f 2>/dev/null | sort)"
	[ "$after" = "$before" ]
}

for spec in "${SPECS[@]}"; do
	read -r pkg target <<<"$spec"
	corpus="$pkg/testdata/fuzz/$target"
	for attempt in 1 2; do
		before="$(find "$corpus" -type f 2>/dev/null | sort)"
		log="$(mktemp)"
		echo "== $target (-fuzztime=$FUZZTIME -parallel=$PARALLEL, attempt $attempt)"
		if go test -count=1 -run='^$' -fuzz="^${target}\$" \
			-fuzztime="$FUZZTIME" -parallel="$PARALLEL" "./$pkg" >"$log" 2>&1; then
			cat "$log"
			rm -f "$log"
			break
		fi
		cat "$log"
		if deadline_flake "$log" "$before" "$corpus"; then
			rm -f "$log"
			if [ "$attempt" -eq 1 ]; then
				echo "::warning::$target hit the go-fuzz coordinator deadline race with no crasher (go.dev/issue/75804); retrying once"
				continue
			fi
			echo "::error::$target hit the coordinator deadline race on both attempts with no crasher — persistent, not a flake"
			exit 1
		fi
		rm -f "$log"
		exit 1
	done
done
echo "all fuzz targets passed"
