import type { components } from "../api/generated/schema";

type ConnectionStatus = components["schemas"]["ConnectionStatus"];

// Cold first connect (F2). A freshly Ready workspace can load the session
// frame before its stream comes up. Launch tickets are single-use (the
// gateway consumes one on the first redeem attempt), so the portal recovers
// by minting a NEW ticket, with backoff, inside a 60 s budget, and never by
// reloading the old one.

/** How long after the frame loads the stream must be confirmed. */
export const COLD_CONFIRM_MS = 8_000;
/** Delay before each re-mint; the last value repeats. */
export const COLD_BACKOFF_MS: readonly number[] = [1_000, 2_000, 4_000];
/** Total time from the first ticket to giving up. */
export const COLD_BUDGET_MS = 60_000;

export interface ColdAttempt {
  startedAt: number;
  relaunches: number;
}

export type ColdStep = { kind: "relaunch"; delayMs: number } | { kind: "give-up" };

export function startCold(now: number): ColdAttempt {
  return { startedAt: now, relaunches: 0 };
}

/**
 * The /connection report proves a stream came up: `connected` is "an open
 * stream on a live lease" (broker connection_state.go), and a
 * `streamOwnerTab` on a live lease means a stream claim landed on it (the
 * broker records the claiming tab with the claim) even if that stream has
 * dropped since. Such a drop is not a cold connect: the connection watch
 * owns it (in-frame retry, then one reload — FX-R32), and a cold re-mint
 * would replace the frame under it. The stream epoch is not consulted: a
 * backend without a session directory reports epoch 0 for every stream,
 * and requiring epoch > 0 would re-mint forever there.
 */
export function streamConfirmed(
  s: Pick<ConnectionStatus, "state" | "leaseActive" | "streamOwnerTab">,
): boolean {
  if (!s.leaseActive) return false;
  return s.state === "connected" || (typeof s.streamOwnerTab === "string" && s.streamOwnerTab !== "");
}

/** What to do after a confirm window ended without a confirmed stream. */
export function nextColdStep(a: ColdAttempt, now: number, confirmMs: number = COLD_CONFIRM_MS): ColdStep {
  const delayMs = COLD_BACKOFF_MS[Math.min(a.relaunches, COLD_BACKOFF_MS.length - 1)];
  if (now + delayMs + confirmMs > a.startedAt + COLD_BUDGET_MS) return { kind: "give-up" };
  return { kind: "relaunch", delayMs };
}
