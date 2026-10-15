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
  /**
   * The session page was showing its "open in another tab" view at this
   * observation. The harness opens exactly one tab per session, so every
   * such observation is a false elsewhere transition (V3.10).
   */
  elsewhere?: boolean;
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
 * How far after the reload a non-connected observation is still attributed
 * to it. Anything later is a different event (a drill, a relaunch) and must
 * not be reported as the reload's reconnect.
 */
export const RELOAD_RECONNECT_WINDOW_MS = 60_000;

export interface ReloadRecovery {
  /**
   * From the first non-connected observation within the window after the
   * reload to the next "connected" one. 0 for a seamless reload; null when
   * the session was lost within the window and not seen connected again,
   * or when nothing was observed inside the window at all.
   */
  reconnectMs: number | null;
  /**
   * True only when the window held at least one observation and none of
   * them was non-connected: a reload with zero observations proves nothing
   * and is never seamless (backlog 4).
   */
  seamless: boolean;
  /**
   * Whether the 60 s window after the reload held at least one
   * observation. "none" is the explicit no-evidence verdict — the session
   * may have stopped reporting after the reload — and report gates count
   * it as a failed reload check, never a pass (backlog 4).
   */
  evidence: "observed" | "none";
}

/**
 * What the mid-run reload cost. Only a non-connected observation in
 * [reloadedAt, reloadedAt + RELOAD_RECONNECT_WINDOW_MS] counts as the
 * reload's reconnect; a reload observed inside that window without one
 * resumed seamlessly (0 ms), so a gap that opens 20 minutes later is never
 * attributed to it.
 */
export function reloadRecovery(observations: Observation[], reloadedAt: number): ReloadRecovery {
  const after = [...observations].sort((a, b) => a.at - b.at).filter((o) => o.at >= reloadedAt);
  const inWindow = after.filter((o) => o.at - reloadedAt <= RELOAD_RECONNECT_WINDOW_MS);
  if (inWindow.length === 0) return { reconnectMs: null, seamless: false, evidence: "none" };
  const lost = inWindow.find((o) => o.state !== "connected");
  if (!lost) return { reconnectMs: 0, seamless: true, evidence: "observed" };
  const back = after.find((o) => o.at > lost.at && o.state === "connected");
  return { reconnectMs: back ? back.at - lost.at : null, seamless: false, evidence: "observed" };
}

/**
 * How many times a session's observations flipped from not-elsewhere to
 * elsewhere ("open in another tab"). The harness owns the only tab, so
 * each transition is a false positive worth counting (V3.10 watch item).
 */
export function elsewhereTransitions(observations: Observation[]): number {
  let count = 0;
  let prev = false;
  for (const obs of [...observations].sort((a, b) => a.at - b.at)) {
    const cur = obs.elsewhere === true;
    if (cur && !prev) count++;
    prev = cur;
  }
  return count;
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
