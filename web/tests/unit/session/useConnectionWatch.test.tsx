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
  return { frame, events, fetchStatus, requestTicket, view };
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
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: true });

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

  it("relaunches with a fresh ticket when the lease is gone", async () => {
    const submitted = watchFormSubmits();
    const { events, fetchStatus, requestTicket } = setup();
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: false });

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
    fetchStatus.mockResolvedValue({ state: "disconnected", leaseActive: false });

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
    fetchStatus.mockResolvedValue({ state: "connected", leaseActive: true });

    await advanced(CONNECTION_POLL_MS * 3);
    expect(fetchStatus).not.toHaveBeenCalled();
  });

  it("does not poll while inactive", async () => {
    const { fetchStatus } = setup({ active: false });
    await advanced(CONNECTION_POLL_MS * 3);
    expect(fetchStatus).not.toHaveBeenCalled();
  });
});
