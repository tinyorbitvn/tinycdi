import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createApi, setCsrfToken } from "../../../src/api/client";
import { ApiProvider } from "../../../src/api/context";
import { MeProvider, type Me } from "../../../src/app/me";
import { SessionPage } from "../../../src/session/SessionPage";
import {
  SESSION_FRAME_ALLOW,
  SESSION_FRAME_SANDBOX,
  markSessionOwned,
  readSessionMarker,
  sessionFrameName,
  TICKET_FIELD,
} from "../../../src/session/launch";
import { createMockApi, CSRF_TOKEN_VALUE } from "../../mock-api/handler.ts";
import { loginCookies, stubFetch } from "../helpers";
import { readyWorkspace } from "../../mock-api/fixtures.ts";
import { leaseRefOf } from "../../mock-api/session.ts";

const SESSION_DOMAIN = "session.example.com";

// The principal /v1/me returns on the portal: identity plus the
// session-bound CSRF token and the session domain (P1/D17).
const ME: Me = {
  subject: "user-01J4ZDADA",
  displayName: "Ada Lovelace",
  tenant: "acme",
  roles: ["user"],
  csrfToken: CSRF_TOKEN_VALUE,
  sessionDomain: SESSION_DOMAIN,
};

function watchFormSubmits() {
  const submitted: HTMLFormElement[] = [];
  vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(function (
    this: HTMLFormElement,
  ) {
    submitted.push(this);
  });
  return submitted;
}

// The mock publishes https://session.example.com as its session origin so
// launch URLs come out as https://ws-<label>.session.example.com/v1/launch.
function newApi() {
  return createMockApi({ sessionOrigin: `https://${SESSION_DOMAIN}` });
}

function setupPage(workspaceOverrides: Parameters<typeof readyWorkspace>[0] = {}) {
  const ws = readyWorkspace(workspaceOverrides);
  const api = newApi();
  api.state.workspaces.set(ws.id, ws);
  loginCookies();
  const client = createApi(stubFetch(api));
  const utils = render(
    <ApiProvider client={client}>
      <MeProvider load={async () => ME}>
        <SessionPage workspaceId={ws.id} />
      </MeProvider>
    </ApiProvider>,
  );
  return { api, ws, ...utils };
}

afterEach(() => {
  setCsrfToken(undefined);
  vi.restoreAllMocks();
  sessionStorage.clear();
});

describe("SessionPage", () => {
  it("renders the session frame with the D13 sandbox and permissions", async () => {
    const submitted = watchFormSubmits();
    const { ws } = setupPage();

    const frame = await screen.findByTitle(`Desktop: ${ws.name}`);
    expect(frame.getAttribute("sandbox")).toBe(SESSION_FRAME_SANDBOX);
    expect(frame.getAttribute("allow")).toBe(SESSION_FRAME_ALLOW);
    const tokens = (frame.getAttribute("sandbox") ?? "").split(/\s+/);
    for (const forbidden of ["allow-top-navigation", "allow-popups", "allow-modals"]) {
      expect(tokens).not.toContain(forbidden);
    }

    // The ticket POST lands inside the frame, never in the top navigation.
    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0].target).toBe(sessionFrameName(ws.id));
    expect(submitted[0].action).toBe(
      `https://ws-${ws.id.replace("ws_", "").toLowerCase()}.${SESSION_DOMAIN}/v1/launch`,
    );
    const input = submitted[0].querySelector(
      `input[name="${TICKET_FIELD}"]`,
    ) as HTMLInputElement;
    expect(input.value).toMatch(/^tkt_/);
  });

  it("offers takeover when the workspace is already connected", async () => {
    const ws = readyWorkspace();
    const api = newApi();
    api.state.workspaces.set(ws.id, ws);
    api.state.leases.set(ws.id, "lease_existing");
    const submitted = watchFormSubmits();
    loginCookies();
    const client = createApi(stubFetch(api));
    render(
      <ApiProvider client={client}>
        <MeProvider load={async () => ME}>
          <SessionPage workspaceId={ws.id} />
        </MeProvider>
      </ApiProvider>,
    );

    await screen.findByRole("alertdialog");
    expect(submitted).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Take over session" }));
    await waitFor(() => expect(submitted).toHaveLength(1));
    const posts = api.state.requests.filter((r) => r.path.endsWith("/connections"));
    expect(JSON.parse(posts[1].rawBody)).toEqual({ takeover: true });
  });

  it("shows Reconnect and Open-in-new-tab fallbacks when the session drops", async () => {
    const submitted = watchFormSubmits();
    const { ws } = setupPage();
    await waitFor(() => expect(submitted).toHaveLength(1)); // auto-launch → connecting

    act(() => {
      window.dispatchEvent(new Event("offline"));
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Connection lost");

    // D15: the disconnected state offers a manual reconnect and the
    // open-in-new-tab fallback.
    fireEvent.click(within(alert).getByRole("button", { name: "Reconnect" }));
    await waitFor(() => expect(submitted).toHaveLength(2));
    expect(submitted[1].target).toBe(sessionFrameName(ws.id));

    // After the relaunch the overlay closes; the toolbar keeps the
    // new-tab fallback available.
    fireEvent.click(screen.getByRole("button", { name: "Open in new tab" }));
    await waitFor(() => expect(submitted).toHaveLength(3));
    expect(submitted[2].target).toBe("_blank");
    expect(submitted[2].rel).toBe("noopener");
  });
});

// ---- FX-R3: resume, signed-out, relaunch timeout, print hint ----

type SessionPageProps = Parameters<typeof SessionPage>[0];

// The jsdom frame never fires a cross-origin load, so these tests reach
// "connected" through the resume path (own lease + the /connection poll).
// `connection` scripts the poll; `fail` turns matching requests into errors.
function setupScripted(
  opts: {
    marker?: boolean | { leaseRef: string; streamEpoch: number };
    lease?: boolean;
    props?: Partial<SessionPageProps>;
  } = {},
) {
  const ws = readyWorkspace();
  const api = newApi();
  api.state.workspaces.set(ws.id, ws);
  if (opts.lease ?? true) api.state.leases.set(ws.id, "lease_own");
  // The tab remembers the lease (and the stream epoch) it last saw connected.
  const marker = opts.marker ?? true;
  if (marker) {
    markSessionOwned(
      ws.id,
      marker === true ? { leaseRef: leaseRefOf("lease_own"), streamEpoch: 0 } : marker,
    );
  }
  loginCookies();
  const control = {
    connection: undefined as (() => unknown) | undefined,
    unauthenticated: (_method: string, _path: string): boolean => false,
    connectionGets: 0,
  };
  const base = stubFetch(api);
  const wrapped = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = input instanceof Request ? input : new Request(String(input), init);
    const path = new URL(req.url).pathname;
    if (control.unauthenticated(req.method, path)) {
      return new Response(
        JSON.stringify({ code: "UNAUTHENTICATED", message: "no session", retryable: false }),
        { status: 401, headers: { "content-type": "application/json" } },
      );
    }
    if (req.method === "GET" && path.endsWith("/connection")) {
      control.connectionGets += 1;
      const scripted = control.connection?.();
      if (scripted !== undefined) {
        return new Response(JSON.stringify(scripted), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }
    }
    return base(input as RequestInfo, init);
  }) as typeof fetch;
  const submitted = watchFormSubmits();
  const onSignIn = vi.fn();
  const utils = render(
    <ApiProvider client={createApi(wrapped)}>
      <MeProvider load={async () => ME}>
        <SessionPage workspaceId={ws.id} onSignIn={onSignIn} {...opts.props} />
      </MeProvider>
    </ApiProvider>,
  );
  const ticketPosts = () =>
    api.state.requests.filter((r) => r.method === "POST" && r.path.endsWith("/connections"));
  return { api, ws, control, submitted, onSignIn, ticketPosts, ...utils };
}

const originOf = (id: string) =>
  `https://ws-${id.replace("ws_", "").toLowerCase()}.${SESSION_DOMAIN}`;

describe("SessionPage resume (R3c)", () => {
  it("mount with an active lease loads the frame without a ticket request", async () => {
    const { ws, submitted, ticketPosts } = setupScripted({ props: { pollIntervalMs: 20 } });

    const frame = (await screen.findByTitle(`Desktop: ${ws.name}`)) as HTMLIFrameElement;
    await waitFor(() => expect(frame.getAttribute("src")).toBe(originOf(ws.id)));
    // The poll confirms the stream: the overlay goes away, badge says connected.
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(ticketPosts()).toHaveLength(0);
    expect(submitted).toHaveLength(0);
  });

  it("resume timeout asks for a ticket without forcing a takeover (R8b)", async () => {
    const { ws, control, submitted, ticketPosts } = setupScripted({
      props: { pollIntervalMs: 20, resumeTimeoutMs: 150 },
    });
    // The lease is alive but the stream never comes back.
    control.connection = () => ({
      state: "disconnected",
      leaseActive: true,
      leaseRef: leaseRefOf("lease_own"),
      streamEpoch: 1,
    });

    const frame = (await screen.findByTitle(`Desktop: ${ws.name}`)) as HTMLIFrameElement;
    await waitFor(() => expect(frame.getAttribute("src")).toBe(originOf(ws.id)));
    expect(ticketPosts()).toHaveLength(0);

    await waitFor(() => expect(ticketPosts()).toHaveLength(1));
    // Never a silent takeover: the lease is still held, so the user is asked.
    expect(JSON.parse(ticketPosts()[0].rawBody)).toEqual({ takeover: false });
    expect(await screen.findByRole("alertdialog")).toBeInTheDocument();
    expect(submitted).toHaveLength(0);
  });

  it("requests a ticket right away when there is no active lease", async () => {
    const { submitted, ticketPosts } = setupScripted({ lease: false });
    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(JSON.parse(ticketPosts()[0].rawBody)).toEqual({ takeover: false });
  });

  it("does not resume a lease this tab never redeemed (another browser's session)", async () => {
    const { submitted, ticketPosts } = setupScripted({ marker: false });
    await screen.findByRole("alertdialog");
    expect(submitted).toHaveLength(0);
    expect(JSON.parse(ticketPosts()[0].rawBody)).toEqual({ takeover: false });
  });
});

describe("SessionPage signed out (R3d)", () => {
  it("a 401 from the poll shows the signed-out state and stops polling", async () => {
    const { control, onSignIn } = setupScripted({ props: { pollIntervalMs: 20 } });
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));

    control.unauthenticated = (m, p) => m === "GET" && p.endsWith("/connection");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("signed out");
    expect(screen.getByRole("status")).toHaveTextContent("Signed out");

    const polls = control.connectionGets;
    await new Promise((r) => setTimeout(r, 120));
    expect(control.connectionGets).toBe(polls);

    fireEvent.click(within(alert).getByRole("button", { name: "Sign in again" }));
    expect(onSignIn).toHaveBeenCalledTimes(1);
  });

  it("a 401 from the ticket request shows the signed-out state", async () => {
    const { control, submitted } = setupScripted({ lease: false, marker: false });
    control.unauthenticated = (m, p) => m === "POST" && p.endsWith("/connections");
    // The first request may already be in flight; the page must still land
    // in signed-out rather than the generic error overlay.
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("signed out");
    expect(within(alert).getByRole("button", { name: "Sign in again" })).toBeInTheDocument();
    expect(submitted).toHaveLength(0);
  });
});

describe("SessionPage relaunch timeout (R3e)", () => {
  it("a frame that never loads after a watch-driven relaunch ends in blocked/timeout", async () => {
    const { api, ws, control, submitted } = setupScripted({
      props: { pollIntervalMs: 20, loadTimeoutMs: 120 },
    });
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));

    // The lease disappears: the watch mints a ticket and submits it into the
    // frame. jsdom never fires the frame's load, like a blocked embed.
    api.state.leases.delete(ws.id);
    control.connection = () => ({ state: "none", leaseActive: false });
    await waitFor(() => expect(submitted).toHaveLength(1));
    // The progress overlay is a status region too; the badge is the one in the toolbar.
    expect(document.querySelector(".tc-session__status")).toHaveTextContent("Connecting");

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("didn't load in time");
    expect(screen.getByRole("status")).toHaveTextContent("Blocked");
  });
});

describe("SessionPage toolbar (R3f)", () => {
  it("explains that printing and downloads need a new tab", async () => {
    setupPage();
    expect(
      await screen.findByText('Printing and downloads need "Open in new tab".'),
    ).toBeInTheDocument();
  });
});

// ---- FX-R8: lease-aware resume and the duplicate-tab guard ----

const OWN_REF = leaseRefOf("lease_own");
// The progress overlay is a status region too; the badge is the toolbar one.
const badge = () => document.querySelector(".tc-session__status");
const OTHER_REF = leaseRefOf("lease_taken_over");

describe("SessionPage resume marker (R8a)", () => {
  it("skips the resume when /connection reports another lease, and asks before taking over", async () => {
    const { ws, submitted, ticketPosts } = setupScripted({
      marker: { leaseRef: OTHER_REF, streamEpoch: 4 },
      props: { pollIntervalMs: 20 },
    });

    // Someone took the session over: the marker is stale, so the normal
    // ticket request runs without takeover and the dialog shows.
    await screen.findByRole("alertdialog");
    const frame = screen.getByTitle(`Desktop: ${ws.name}`) as HTMLIFrameElement;
    expect(frame.getAttribute("src")).toBeNull();
    expect(ticketPosts()).toHaveLength(1);
    expect(JSON.parse(ticketPosts()[0].rawBody)).toEqual({ takeover: false });
    expect(submitted).toHaveLength(0);
    expect(readSessionMarker(ws.id)).toBeNull();
    expect(screen.getByRole("status")).not.toHaveTextContent("Connected");
  });

  it("a Reconnect after the skipped resume still asks (no inherited ownership)", async () => {
    const { ticketPosts } = setupScripted({
      marker: { leaseRef: OTHER_REF, streamEpoch: 4 },
      props: { pollIntervalMs: 20 },
    });
    await screen.findByRole("alertdialog");
    fireEvent.click(screen.getByRole("button", { name: "Reconnect" }));
    await waitFor(() => expect(ticketPosts()).toHaveLength(2));
    expect(JSON.parse(ticketPosts()[1].rawBody)).toEqual({ takeover: false });
  });

  it("stores the lease and stream epoch this tab saw connected", async () => {
    const { ws } = setupScripted({ props: { pollIntervalMs: 20 } });
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));
    expect(readSessionMarker(ws.id)).toEqual({ leaseRef: OWN_REF, streamEpoch: 1 });
  });

  it("does not take a stream at or below the remembered epoch for the resumed one", async () => {
    const { control } = setupScripted({
      marker: { leaseRef: OWN_REF, streamEpoch: 3 },
      props: { pollIntervalMs: 20, resumeTimeoutMs: 5_000 },
    });
    // The previous page's stream (epoch 3) lingers: not our stream yet.
    control.connection = () => ({
      state: "connected",
      leaseActive: true,
      leaseRef: OWN_REF,
      streamEpoch: 3,
    });
    await waitFor(() => expect(badge()).toHaveTextContent("Connecting"));
    await new Promise((r) => setTimeout(r, 150));
    expect(badge()).toHaveTextContent("Connecting");

    // The reloaded frame opened its own stream.
    control.connection = () => ({
      state: "connected",
      leaseActive: true,
      leaseRef: OWN_REF,
      streamEpoch: 4,
    });
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));
  });
});

describe("SessionPage duplicate tab (R8c)", () => {
  const status = (streamEpoch: number, leaseRef = OWN_REF) => ({
    state: "connected",
    leaseActive: true,
    leaseRef,
    streamEpoch,
  });

  it("a higher stream epoch on the same lease shows 'open in another tab' and does not reload", async () => {
    const { ws, control, submitted, ticketPosts } = setupScripted({
      props: { pollIntervalMs: 20 },
    });
    control.connection = () => status(1);
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));
    const frame = screen.getByTitle(`Desktop: ${ws.name}`) as HTMLIFrameElement;
    const srcBefore = frame.getAttribute("src");

    // Another tab with the same cookie opened a stream on our lease.
    control.connection = () => status(2);
    expect(await screen.findByText("This session is open in another tab")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Use here" })).toBeInTheDocument();

    // No ping-pong: nothing reloads the frame or mints a ticket, even over
    // several more polls.
    const polls = control.connectionGets;
    await new Promise((r) => setTimeout(r, 200));
    expect(frame.getAttribute("src")).toBe(srcBefore);
    expect(submitted).toHaveLength(0);
    expect(ticketPosts()).toHaveLength(0);
    expect(screen.getByText("This session is open in another tab")).toBeInTheDocument();
    // The watch is idle in this state: it does not poll.
    expect(control.connectionGets).toBe(polls);
  });

  it("'Use here' brings the desktop back into this tab and does not flag our own new stream", async () => {
    const { control, ticketPosts } = setupScripted({ props: { pollIntervalMs: 20 } });
    control.connection = () => status(1);
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));
    control.connection = () => status(2);
    await screen.findByText("This session is open in another tab");

    fireEvent.click(screen.getByRole("button", { name: "Use here" }));
    // The reloaded frame opens its own stream (epoch 3), which is ours.
    control.connection = () => status(3);
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));
    await new Promise((r) => setTimeout(r, 200));
    expect(screen.queryByText("This session is open in another tab")).toBeNull();
    expect(screen.getByRole("status")).toHaveTextContent("Connected");
    expect(ticketPosts()).toHaveLength(0);
  });

  it("a stream epoch advance caused by this tab's own frame reload is not 'another tab'", async () => {
    const { control } = setupScripted({ props: { pollIntervalMs: 20 } });
    control.connection = () => status(1);
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Connected"));

    // The stream drops with the lease alive: the watch reloads the frame
    // after its first backoff step, which opens stream 2.
    control.connection = () => ({
      state: "disconnected",
      leaseActive: true,
      leaseRef: OWN_REF,
      streamEpoch: 1,
    });
    await new Promise((r) => setTimeout(r, 1_300));
    control.connection = () => status(2);
    await new Promise((r) => setTimeout(r, 300));
    expect(screen.queryByText("This session is open in another tab")).toBeNull();
    expect(screen.getByRole("status")).toHaveTextContent("Connected");
  });
});
