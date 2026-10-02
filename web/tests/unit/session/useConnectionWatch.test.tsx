import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import {
  CONNECTION_POLL_MS,
  RECONNECT_BACKOFF_MS,
  useConnectionWatch,
  type ConnectionStatus,
  type WatchEvent,
} from "../../../src/session/useConnectionWatch";
import { sessionFrameName, type LaunchTicket } from "../../../src/session/launch";

const WS = "ws_0123456789abcdef";
const DOMAIN = "session.example.com";
const ORIGIN = `https://ws-0123456789abcdef.${DOMAIN}`;

function ticket(): LaunchTicket {
  return {
    workspaceId: WS,
    ticket: "tkt_relaunch",
    launchUrl: `${ORIGIN}/v1/launch`,
    expiresAt: "2026-10-02T00:01:00Z",
  };
}

function setup(overrides: Record<string, unknown> = {}) {
  const frame = document.createElement("iframe");
  const events: WatchEvent[] = [];
  const fetchStatus = vi.fn<(status?: void) => Promise<ConnectionStatus>>();
  const requestTicket = vi.fn(async () => ticket());
  const props = {
    workspaceId: WS,
    sessionDomain: DOMAIN,
    frameName: sessionFrameName(WS),
    frame: { current: frame },
    active: true,
    fetchStatus,
    requestTicket,
    onEvent: (e: WatchEvent) => events.push(e),
    ...overrides,
  };
  const view = renderHook((p: typeof props) => useConnectionWatch(p), { initialProps: props });
  return { frame, events, fetchStatus, requestTicket, view, props };
}

function watchFormSubmits() {
  const submitted: HTMLFormElement[] = [];
  vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(function (
    this: HTMLFormElement,
  ) {
    submitted.push(this);
  });
  return submitted;
}

const navigated = (events: WatchEvent[]) => events.filter((e) => e.type === "frame-navigated");
const advanced = (ms: number) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)));

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("useConnectionWatch (D15)", () => {
  it("reloads the frame while the lease is active", async () => {
    const { frame, events, fetchStatus, requestTicket } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });

    // First poll finds the lease still alive: reload after backoff[0] = 1 s.
    await advanced(CONNECTION_POLL_MS);
    expect(fetchStatus).toHaveBeenCalledTimes(1);
    expect(frame.getAttribute("src")).toBeNull();
    await advanced(RECONNECT_BACKOFF_MS[0] - 1);
    expect(frame.getAttribute("src")).toBeNull();
    await advanced(1);
    expect(frame.getAttribute("src")).toBe(ORIGIN);
    expect(navigated(events)).toHaveLength(1);

    // Still disconnected on the next poll: backoff[1] = 2 s.
    await advanced(CONNECTION_POLL_MS);
    expect(navigated(events)).toHaveLength(1);
    await advanced(RECONNECT_BACKOFF_MS[1]);
    expect(navigated(events)).toHaveLength(2);
    // A live lease never mints a new ticket.
    expect(requestTicket).not.toHaveBeenCalled();
  });

  // R3a: the poll runs every 5 s, longer than the first backoff steps but
  // shorter than the last two. A poll must never cancel a pending reload.
  it("fires every backoff step exactly once under a 5 s poll, then reports exhausted", async () => {
    const { events, fetchStatus, requestTicket } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });

    // Poll cadence 5 s; run long enough for the 15 s step to land and one
    // more poll to find the budget spent.
    await advanced(CONNECTION_POLL_MS * 12);
    expect(navigated(events)).toHaveLength(RECONNECT_BACKOFF_MS.length);
    expect(events.filter((e) => e.type === "exhausted")).toHaveLength(1);
    // Exhaustion comes after the last reload, never before it.
    const kinds = events.map((e) => e.type).filter((k) => k === "frame-navigated" || k === "exhausted");
    expect(kinds).toEqual([
      ...RECONNECT_BACKOFF_MS.map(() => "frame-navigated"),
      "exhausted",
    ]);

    // Quiet afterwards: no further reloads, no repeated exhaustion, no tickets.
    await advanced(CONNECTION_POLL_MS * 6);
    expect(navigated(events)).toHaveLength(RECONNECT_BACKOFF_MS.length);
    expect(events.filter((e) => e.type === "exhausted")).toHaveLength(1);
    expect(requestTicket).not.toHaveBeenCalled();
  });

  it("does not re-arm a pending reload on every poll (8 s step survives a 5 s poll)", async () => {
    const { frame, events, fetchStatus } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });

    // Steps 0..2 (1 s, 2 s, 4 s) each fit between two polls.
    await advanced(CONNECTION_POLL_MS * 4); // t = 20 s: step 3 (8 s) armed now
    expect(navigated(events)).toHaveLength(3);
    const before = navigated(events).length;
    await advanced(CONNECTION_POLL_MS); // t = 25 s: a poll lands mid-wait
    expect(navigated(events)).toHaveLength(before);
    await advanced(3_000); // t = 28 s: the 8 s timer still fires
    expect(navigated(events)).toHaveLength(before + 1);
    expect(frame.getAttribute("src")).toBe(ORIGIN);
  });

  it("cancels a pending reload when the stream comes back and restarts the backoff", async () => {
    const { events, fetchStatus } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });
    await advanced(CONNECTION_POLL_MS * 4); // steps 0..2 ran; the 8 s step is pending
    expect(navigated(events)).toHaveLength(3);

    fetchStatus.mockResolvedValue({ state: "connected", leaseActive: true, streamEpoch: 0 });
    await advanced(CONNECTION_POLL_MS); // t = 25 s: connected again
    await advanced(RECONNECT_BACKOFF_MS[3]); // the cancelled reload must not fire
    expect(navigated(events)).toHaveLength(3);

    // The next outage starts again at the first step.
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });
    await advanced(CONNECTION_POLL_MS + RECONNECT_BACKOFF_MS[0]);
    expect(navigated(events)).toHaveLength(4);
  });

  // R3b: a poll that is already in flight when the watch is disabled or the
  // page unmounts must not act on its result.
  describe("a poll in flight when the watch stops", () => {
    function deferredStatus() {
      let resolve!: (s: ConnectionStatus) => void;
      const promise = new Promise<ConnectionStatus>((r) => (resolve = r));
      return { promise, resolve };
    }

    it("arms no reload timer after unmount", async () => {
      const { events, fetchStatus, view, frame } = setup();
      const d = deferredStatus();
      fetchStatus.mockReturnValue(d.promise);
      await advanced(CONNECTION_POLL_MS);
      expect(fetchStatus).toHaveBeenCalledTimes(1);
      view.unmount();
      await act(async () => {
        d.resolve({ state: "disconnected", leaseActive: true, streamEpoch: 0 });
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(vi.getTimerCount()).toBe(0);
      await advanced(RECONNECT_BACKOFF_MS[0] * 3);
      expect(navigated(events)).toHaveLength(0);
      expect(frame.getAttribute("src")).toBeNull();
    });

    it("mints no ticket and submits no form after unmount", async () => {
      const submitted = watchFormSubmits();
      const { events, fetchStatus, requestTicket, view } = setup();
      const d = deferredStatus();
      fetchStatus.mockReturnValue(d.promise);
      await advanced(CONNECTION_POLL_MS);
      view.unmount();
      await act(async () => {
        d.resolve({ state: "disconnected", leaseActive: false, streamEpoch: 0 });
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(requestTicket).not.toHaveBeenCalled();
      expect(submitted).toHaveLength(0);
      expect(events).toEqual([]);
    });

    it("ignores the result when the watch is disabled mid-poll", async () => {
      const submitted = watchFormSubmits();
      const { events, fetchStatus, requestTicket, view, props } = setup();
      const d = deferredStatus();
      fetchStatus.mockReturnValue(d.promise);
      await advanced(CONNECTION_POLL_MS);
      view.rerender({ ...props, active: false });
      await act(async () => {
        d.resolve({ state: "disconnected", leaseActive: false, streamEpoch: 0 });
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(requestTicket).not.toHaveBeenCalled();
      expect(submitted).toHaveLength(0);
      expect(vi.getTimerCount()).toBe(0);
      expect(events).toEqual([]);
    });

    it("submits no form when unmounted while the relaunch ticket is being minted", async () => {
      const submitted = watchFormSubmits();
      const { requestTicket, fetchStatus, view } = setup();
      let release!: (t: LaunchTicket) => void;
      requestTicket.mockReturnValue(new Promise<LaunchTicket>((r) => (release = r)));
      fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: false, streamEpoch: 0 });
      await advanced(CONNECTION_POLL_MS);
      expect(requestTicket).toHaveBeenCalledTimes(1);
      view.unmount();
      await act(async () => {
        release(ticket());
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(submitted).toHaveLength(0);
    });
  });

  it("relaunches with a fresh ticket when the lease is gone", async () => {
    const submitted = watchFormSubmits();
    const { events, fetchStatus, requestTicket } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: false, streamEpoch: 0 });

    await advanced(CONNECTION_POLL_MS);
    await vi.waitFor(() => expect(submitted).toHaveLength(1));

    expect(requestTicket).toHaveBeenCalledTimes(1);
    expect(submitted[0].method).toBe("post");
    expect(submitted[0].target).toBe(sessionFrameName(WS));
    expect(submitted[0].action).toBe(`${ORIGIN}/v1/launch`);
    expect(events).toContainEqual({ type: "relaunched" });
  });

  it("stops after MAX_AUTO_RELAUNCH within 5 minutes", async () => {
    const submitted = watchFormSubmits();
    const { events, fetchStatus, requestTicket } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: false, streamEpoch: 0 });

    await advanced(CONNECTION_POLL_MS);
    await vi.waitFor(() => expect(requestTicket).toHaveBeenCalledTimes(1));
    await advanced(CONNECTION_POLL_MS);
    await vi.waitFor(() => expect(requestTicket).toHaveBeenCalledTimes(2));

    // Third loss inside the window: give up, surface the disconnected state.
    await advanced(CONNECTION_POLL_MS);
    await vi.waitFor(() => expect(events).toContainEqual({ type: "exhausted" }));
    expect(requestTicket).toHaveBeenCalledTimes(2);
    expect(submitted).toHaveLength(2);

    // It stays quiet afterwards — no more tickets, no repeated exhaustion.
    await advanced(CONNECTION_POLL_MS * 2);
    expect(requestTicket).toHaveBeenCalledTimes(2);
    expect(events.filter((e) => e.type === "exhausted")).toHaveLength(1);
  });

  it("pauses polling while the page is hidden", async () => {
    vi.spyOn(Document.prototype, "visibilityState", "get").mockReturnValue("hidden");
    const { fetchStatus } = setup();
    fetchStatus.mockResolvedValue({ state: "connected", leaseActive: true, streamEpoch: 0 });

    await advanced(CONNECTION_POLL_MS * 3);
    expect(fetchStatus).not.toHaveBeenCalled();
  });

  it("does not poll while inactive", async () => {
    const { fetchStatus } = setup({ active: false });
    await advanced(CONNECTION_POLL_MS * 3);
    expect(fetchStatus).not.toHaveBeenCalled();
  });
});

// FX-R8 R8d: the last reload deserves its whole interval before the watch
// gives up; otherwise it is judged by the very next poll, seconds later.
describe("useConnectionWatch last backoff step (FX-R8)", () => {
  it("waits the last step's full interval after the final reload before reporting exhausted", async () => {
    const stamps: { type: string; at: number }[] = [];
    const { fetchStatus } = setup({
      onEvent: (e: WatchEvent) => stamps.push({ type: e.type, at: Date.now() }),
    });
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });

    await advanced(CONNECTION_POLL_MS * 20);
    const lastReload = stamps.filter((s) => s.type === "frame-navigated").at(-1);
    const exhausted = stamps.find((s) => s.type === "exhausted");
    expect(lastReload).toBeDefined();
    expect(exhausted).toBeDefined();
    const last = RECONNECT_BACKOFF_MS[RECONNECT_BACKOFF_MS.length - 1];
    expect(exhausted!.at - lastReload!.at).toBeGreaterThanOrEqual(last);
  });

  it("a connected report during the last grace period cancels the exhaustion", async () => {
    const { events, fetchStatus } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });
    // Run until the last reload has happened (about 45 s with a 5 s poll).
    await advanced(CONNECTION_POLL_MS * 9 + 1000);
    expect(navigated(events)).toHaveLength(RECONNECT_BACKOFF_MS.length);

    fetchStatus.mockResolvedValue({ state: "connected", leaseActive: true, streamEpoch: 0 });
    await advanced(CONNECTION_POLL_MS * 6);
    expect(events.filter((e) => e.type === "exhausted")).toHaveLength(0);
  });
});

// FX-R8 R8a/R8c: every successful poll is also handed to the page, which
// owns the lease/epoch bookkeeping.
describe("useConnectionWatch observations (FX-R8)", () => {
  it("passes each polled status to onObserve before acting on it", async () => {
    const seen: ConnectionStatus[] = [];
    const { fetchStatus } = setup({ onObserve: (s: ConnectionStatus) => seen.push(s) });
    const status: ConnectionStatus = {
      state: "connected",
      leaseActive: true,
      leaseRef: "0123456789abcdef",
      streamEpoch: 3,
    };
    fetchStatus.mockResolvedValue(status);
    await advanced(CONNECTION_POLL_MS * 2);
    expect(seen).toEqual([status, status]);
  });
});
