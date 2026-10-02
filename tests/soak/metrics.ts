// Pure metrics for the soak harness: percentiles, connection-gap analysis
// and duration parsing. Kept dependency-free so `node --test` covers them
// without a browser or a cluster. Erasable-syntax TypeScript only (Node
// type stripping runs this file directly, same as web/tests/mock-api).

/** One connection-state observation of a single session. */
export interface Observation {
  /** Epoch milliseconds. */
  at: number;
  /** ConnectionStatus.state: "none" | "connected" | "disconnected" | "stale". */
  state: string;
  /** Where the state came from: the API endpoint or the session probe. */
  source?: string;
}

/** A contiguous span of observations in one state. */
export interface StateSpan {
  state: string;
  /** Where the first observation of the span came from ("api" | "probe"). */
  source?: string;
  /** ISO 8601 UTC. */
  startedAt: string;
  /** ISO 8601 UTC; absent when the span was still open at run end. */
  endedAt?: string;
  durationMs: number;
}

/**
 * Nearest-rank percentile: percentile(values, 95) is the smallest value v
 * such that at least 95% of samples are <= v. Returns null for an empty
 * input so reports can distinguish "no samples" from 0.
 */
export function percentile(values: number[], p: number): number | null {
  if (values.length === 0) return null;
  if (p <= 0 || p > 100) throw new RangeError(`percentile out of range: ${p}`);
  const sorted = [...values].sort((a, b) => a - b);
  const rank = Math.ceil((p / 100) * sorted.length);
  return sorted[Math.max(0, rank - 1)];
}

/**
 * Collapse a session's observation timeline into per-state spans. Every
 * state other than "connected" shows up here — that is what the report's
 * nonConnectedStates list is built from.
 */
export function stateSpans(observations: Observation[]): StateSpan[] {
  const spans: StateSpan[] = [];
  for (const obs of [...observations].sort((a, b) => a.at - b.at)) {
    const last = spans[spans.length - 1];
    if (last && last.state === obs.state) {
      // Same state again: the span is still open; stretch its observed
      // duration to this observation.
      last.durationMs = obs.at - new Date(last.startedAt).getTime();
    } else {
      // A new state closes the previous span at this observation's time.
      if (last) {
        last.endedAt = new Date(obs.at).toISOString();
        last.durationMs = obs.at - new Date(last.startedAt).getTime();
      }
      spans.push({
        state: obs.state,
        ...(obs.source !== undefined ? { source: obs.source } : {}),
        startedAt: new Date(obs.at).toISOString(),
        durationMs: 0,
      });
    }
  }
  return spans;
}

/**
 * The longest uninterrupted period the session was observed in any state
 * other than "connected". Spans before the session first reached
 * "connected" are excluded — that stretch is already reported as
 * connectMs (time from launch click to first connected observation).
 * A span still open at `endAt` counts up to `endAt`.
 */
export function longestDisconnectedGapMs(observations: Observation[], endAt: number): number {
  const sorted = [...observations].sort((a, b) => a.at - b.at);
  const firstConnected = sorted.find((o) => o.state === "connected");
  if (!firstConnected) return 0;
  let longest = 0;
  let gapStart: number | null = null;
  for (const obs of sorted) {
    if (obs.at <= firstConnected.at) continue;
    if (obs.state === "connected") {
      if (gapStart !== null) {
        longest = Math.max(longest, obs.at - gapStart);
        gapStart = null;
      }
    } else if (gapStart === null) {
      gapStart = obs.at;
    }
  }
  if (gapStart !== null) longest = Math.max(longest, endAt - gapStart);
  return longest;
}

/**
 * Time to recover after the mid-run reload: from the first non-connected
 * observation at or after `reloadedAt` to the next "connected" one. A reload
 * that resumes seamlessly never shows a non-connected state and yields null
 * (nothing to reconnect) rather than the polling latency; null also covers a
 * session still not connected at run end.
 */
export function reconnectMs(observations: Observation[], reloadedAt: number): number | null {
  const after = [...observations].sort((a, b) => a.at - b.at).filter((o) => o.at >= reloadedAt);
  const lost = after.find((o) => o.state !== "connected");
  if (!lost) return null;
  const back = after.find((o) => o.at > lost.at && o.state === "connected");
  return back ? back.at - lost.at : null;
}

/**
 * Parse a duration like "60m", "90s", "500ms", "1h" or a bare number of
 * seconds into milliseconds.
 */
export function parseDurationMs(raw: string): number {
  const m = /^(\d+(?:\.\d+)?)(ms|s|m|h)?$/.exec(raw.trim());
  if (!m) throw new Error(`bad duration "${raw}" (use e.g. 30s, 60m, 1h)`);
  const n = Number(m[1]);
  const unit = m[2] ?? "s";
  const factor = unit === "ms" ? 1 : unit === "s" ? 1_000 : unit === "m" ? 60_000 : 3_600_000;
  return Math.round(n * factor);
}
