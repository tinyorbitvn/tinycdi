import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import {
  CONNECTION_POLL_MS,
  IN_FRAME_RETRY_MS,
  RECONNECT_BACKOFF_MS,
  useConnectionWatch,
  type ConnectionStatus,
  type WatchEvent,
} from "../../../src/session/useConnectionWatch";
import { sessionFrameName, sessionFrameUrl, sessionTabId, type LaunchTicket } from "../../../src/session/launch";

const WS = "ws_0123456789abcdef";
const DOMAIN = "session.example.com";
const ORIGIN = `https://ws-0123456789abcdef.${DOMAIN}`;
// Frame navigations load the desktop client with the embedded-parity
// settings (FX-R18 resize=remote + V3.24); no clipboard policy here.
// The frame URL includes the tab-id path setting (FX-R31); the id is
// module-scoped, minted once per test file, so compare against a same-test
// call rather than a module constant.
const frameUrl = () => sessionFrameUrl(WS, DOMAIN);
const OTHER_TAB = "fedcba9876543210fedcba9876543210";

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
  // FX-R32: with a live lease the frame's own KasmVNC retry gets the whole
  // in-frame window first — the watch never touches el.src inside it.
  it("waits out the in-frame retry window before touching the frame", async () => {
    const { frame, events, fetchStatus, requestTicket } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });

    // First poll finds the stream down but the lease alive: recovery is
    // announced, nothing navigates.
    await advanced(CONNECTION_POLL_MS);
    expect(fetchStatus).toHaveBeenCalledTimes(1);
    expect(events).toContainEqual({ type: "recovering" });
    expect(frame.getAttribute("src")).toBeNull();

    // The window still has time left: polls report the retry as pending,
    // no reload is armed.
    await advanced(IN_FRAME_RETRY_MS - 1);
    expect(frame.getAttribute("src")).toBeNull();
    expect(navigated(events)).toHaveLength(0);
    expect(requestTicket).not.toHaveBeenCalled();
  });

  it("recovers without any navigation when the frame's retry lands", async () => {
    const { frame, events, fetchStatus } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });
    await advanced(CONNECTION_POLL_MS);
    expect(events).toContainEqual({ type: "recovering" });

    // The in-frame retry re-claimed the stream inside the window: the poll
    // sees connected again and the frame is never re-navigated.
    fetchStatus.mockResolvedValue({ state: "connected", leaseActive: true, streamEpoch: 1 });
    await advanced(CONNECTION_POLL_MS * 4);
    expect(frame.getAttribute("src")).toBeNull();
    expect(navigated(events)).toHaveLength(0);
    expect(events.filter((e) => e.type === "exhausted")).toHaveLength(0);
  });

  it("re-navigates exactly once when the in-frame window runs out", async () => {
    const { frame, events, fetchStatus } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });

    // Poll 1 opens the window; poll 2 (past it) drives the one reload.
    await advanced(CONNECTION_POLL_MS);
    expect(navigated(events)).toHaveLength(0);
    await advanced(CONNECTION_POLL_MS);
    expect(navigated(events)).toHaveLength(1);
    expect(frame.getAttribute("src")).toBe(frameUrl());

    // Bounded backoff: the next poll arms step 0 (10 s); the second
    // re-navigation cannot fire inside it.
    await advanced(CONNECTION_POLL_MS);
    await advanced(RECONNECT_BACKOFF_MS[0] - 1);
    expect(navigated(events)).toHaveLength(1);
    await advanced(1); // the step boundary itself fires the second reload
    expect(navigated(events)).toHaveLength(2);
    await advanced(CONNECTION_POLL_MS * 2);
    expect(navigated(events)).toHaveLength(2);
  });

  it("never navigates a frame whose stream was fenced by another owner", async () => {
    const { frame, events, fetchStatus, requestTicket } = setup();
    fetchStatus.mockResolvedValue({
      state: "disconnected",
      leaseActive: true,
      leaseRef: "0123456789abcdef",
      streamEpoch: 3,
      streamOwnerTab: OTHER_TAB,
    });

    // Another tab holds the stream: 'elsewhere' is the page's verdict —
    // the watch neither navigates into it nor mints a ticket.
    await advanced(CONNECTION_POLL_MS * 8);
    expect(frame.getAttribute("src")).toBeNull();
    expect(navigated(events)).toHaveLength(0);
    expect(requestTicket).not.toHaveBeenCalled();
    expect(events.filter((e) => e.type === "exhausted")).toHaveLength(0);
  });

  it("navigates normally when the stream owner is this tab itself", async () => {
    const { events, fetchStatus } = setup();
    fetchStatus.mockResolvedValue({
      state: "disconnected",
      leaseActive: true,
      leaseRef: "0123456789abcdef",
      streamEpoch: 3,
      streamOwnerTab: sessionTabId(),
    });
    await advanced(CONNECTION_POLL_MS * 3);
    expect(navigated(events)).toHaveLength(1);
  });

  // The backoff ladder runs only after the in-frame window, on the
  // [10,15,20] spacing, then reports exhausted.
  it("fires every backoff step exactly once under a 5 s poll, then reports exhausted", async () => {
    const { events, fetchStatus, requestTicket } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });

    // Poll cadence 5 s; run long enough for the 20 s step to land and one
    // more poll to find the budget spent.
    await advanced(CONNECTION_POLL_MS * 20);
    expect(navigated(events)).toHaveLength(RECONNECT_BACKOFF_MS.length);
    expect(events.filter((e) => e.type === "exhausted")).toHaveLength(1);
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

  it("does not re-arm a pending reload on every poll (10 s step survives a 5 s poll)", async () => {
    const { frame, events, fetchStatus } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });

    // t=5 s: window opens. t=10 s: reload 1. t=15 s: step 0 (10 s) armed.
    await advanced(CONNECTION_POLL_MS * 3);
    expect(navigated(events)).toHaveLength(1);
    await advanced(CONNECTION_POLL_MS); // t = 20 s: a poll lands mid-wait
    expect(navigated(events)).toHaveLength(1);
    await advanced(5_000); // t = 25 s: the 10 s timer still fires
    expect(navigated(events)).toHaveLength(2);
    expect(frame.getAttribute("src")).toBe(frameUrl());
  });

  it("cancels a pending reload when the stream comes back and restarts the recovery", async () => {
    const { events, fetchStatus } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });
    await advanced(CONNECTION_POLL_MS * 3); // nav 1 done; step 0 armed
    expect(navigated(events)).toHaveLength(1);

    fetchStatus.mockResolvedValue({ state: "connected", leaseActive: true, streamEpoch: 1 });
    await advanced(CONNECTION_POLL_MS); // t = 20 s: connected again
    await advanced(RECONNECT_BACKOFF_MS[0]); // the cancelled reload must not fire
    expect(navigated(events)).toHaveLength(1);

    // The next outage starts again at the in-frame window, not mid-ladder.
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 1 });
    await advanced(CONNECTION_POLL_MS * 2);
    expect(navigated(events)).toHaveLength(2);
  });

  // A short, injectable in-frame window: useful for fast tests, and proves
  // the window is measured, not just the first backoff step.
  it("honours an injected in-frame retry window", async () => {
    const { events, fetchStatus } = setup({ inFrameRetryMs: 50 });
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true, streamEpoch: 0 });
    await advanced(CONNECTION_POLL_MS);
    expect(navigated(events)).toHaveLength(0);
    await advanced(60);
    await advanced(CONNECTION_POLL_MS);
    expect(navigated(events)).toHaveLength(1);
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
      await advanced(IN_FRAME_RETRY_MS + RECONNECT_BACKOFF_MS[0] * 3);
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
    expect(submitted[0].action.startsWith(`${ORIGIN}/v1/launch?`)).toBe(true);
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

    await advanced(CONNECTION_POLL_MS * 30);
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
    // Run until the last reload has happened (~40 s in: window 5 s, then
    // the +10 s and +15 s steps) but before its 20 s grace expires at ~60 s.
    await advanced(CONNECTION_POLL_MS * 11);
    expect(navigated(events)).toHaveLength(RECONNECT_BACKOFF_MS.length);

    fetchStatus.mockResolvedValue({ state: "connected", leaseActive: true, streamEpoch: 1 });
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
