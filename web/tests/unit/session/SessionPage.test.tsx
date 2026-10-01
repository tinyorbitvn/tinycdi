import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createApi, setCsrfToken } from "../../../src/api/client";
import { ApiProvider } from "../../../src/api/context";
import { MeProvider, type Me } from "../../../src/app/me";
import { SessionPage } from "../../../src/session/SessionPage";
import {
  SESSION_FRAME_ALLOW,
  SESSION_FRAME_SANDBOX,
  sessionFrameName,
  TICKET_FIELD,
} from "../../../src/session/launch";
import { createMockApi, CSRF_TOKEN_VALUE } from "../../mock-api/handler.ts";
import { loginCookies, stubFetch } from "../helpers";
import { readyWorkspace } from "../../mock-api/fixtures.ts";

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
