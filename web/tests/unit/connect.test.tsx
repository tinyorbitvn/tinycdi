import { describe, expect, it, vi, afterEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { ConnectButton } from "../../src/workspaces/ConnectButton";
import { MeProvider, type Me } from "../../src/app/me";
import { TICKET_FIELD } from "../../src/session/launch";
import {
  createMockApi,
  renderWithApi,
  loginCookies,
  CSRF_TOKEN_VALUE,
} from "./helpers";
import { readyWorkspace } from "../mock-api/fixtures.ts";
import type { ReactElement } from "react";

// v0.2 (D9/D17): the workspace's launch URL lives on its own session host
// (ws-<label>.<sessionDomain>), and the portal learns the domain — plus the
// CSRF token — from GET /v1/me, not from cookies.

const SESSION_DOMAIN = "session.example.com";

const ME: Me = {
  subject: "user-01J4ZDADA",
  displayName: "Ada Lovelace",
  tenant: "acme",
  roles: ["user"],
  csrfToken: CSRF_TOKEN_VALUE,
  sessionDomain: SESSION_DOMAIN,
};

function renderConnect(ui: ReactElement, api = createMockApi(), me: Me = ME) {
  const utils = renderWithApi(
    <MeProvider load={async () => me}>{ui}</MeProvider>,
    api,
  );
  return utils;
}

function watchFormSubmits() {
  const submitted: HTMLFormElement[] = [];
  const spy = vi
    .spyOn(HTMLFormElement.prototype, "submit")
    .mockImplementation(function (this: HTMLFormElement) {
      submitted.push(this);
    });
  return { submitted, spy };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("ConnectButton", () => {
  it("POSTs the launch ticket to the workspace's session host in a new tab", async () => {
    const ws = readyWorkspace();
    const { submitted } = watchFormSubmits();
    const api = createMockApi({ sessionOrigin: `https://${SESSION_DOMAIN}` });
    api.state.workspaces.set(ws.id, ws);
    loginCookies();
    renderConnect(<ConnectButton workspace={ws} />, api);

    const button = await screen.findByRole("button", { name: "Connect" });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);

    await waitFor(() => expect(submitted).toHaveLength(1));
    const form = submitted[0];
    expect(form.method).toBe("post");
    expect(form.action).toBe(
      `https://${ws.id.replace("ws_", "ws-").toLowerCase()}.${SESSION_DOMAIN}/v1/launch`,
    );
    expect(form.target).toBe("_blank");
    const ticketInput = form.querySelector(
      `input[name="${TICKET_FIELD}"]`,
    ) as HTMLInputElement;
    expect(ticketInput.value).toMatch(/^tkt_/);

    // The ticket must never reach a URL, history entry or web storage.
    expect(form.action).not.toContain(ticketInput.value);
    expect(JSON.stringify(localStorage)).not.toContain(ticketInput.value);
    expect(JSON.stringify(sessionStorage)).not.toContain(ticketInput.value);
    expect(window.location.href).not.toContain(ticketInput.value);
  });

  it("prompts for explicit takeover on CONNECTION_IN_USE", async () => {
    const ws = readyWorkspace();
    const api = createMockApi({ sessionOrigin: `https://${SESSION_DOMAIN}` });
    api.state.workspaces.set(ws.id, ws);
    api.state.leases.set(ws.id, "lease_existing");
    const { submitted } = watchFormSubmits();
    loginCookies();
    renderConnect(<ConnectButton workspace={ws} />, api);

    const button = await screen.findByRole("button", { name: "Connect" });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("already connected");
    expect(submitted).toHaveLength(0); // no ticket minted for the failed call

    fireEvent.click(screen.getByRole("button", { name: "Take over session" }));
    await waitFor(() => expect(submitted).toHaveLength(1));

    const posts = api.state.requests.filter((r) =>
      r.path.endsWith("/connections"),
    );
    expect(posts).toHaveLength(2);
    expect(JSON.parse(posts[0].rawBody)).toEqual({ takeover: false });
    expect(JSON.parse(posts[1].rawBody)).toEqual({ takeover: true });
  });

  // SEC-26: the ticket is bearer-equivalent — it may only be POSTed to the
  // workspace's own host under the session domain /v1/me published. A
  // launchUrl on any other host must be refused before the form submits.
  it("refuses to POST the ticket to a foreign session host", async () => {
    const ws = readyWorkspace();
    // The mock answers a launchUrl on a different domain than /v1/me
    // published — the client must pin the response to the configured domain.
    const api = createMockApi({ sessionDomain: "evil.example" });
    api.state.workspaces.set(ws.id, ws);
    const { submitted } = watchFormSubmits();
    loginCookies();
    renderConnect(<ConnectButton workspace={ws} />, api);

    const button = await screen.findByRole("button", { name: "Connect" });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);

    await screen.findByRole("alert");
    expect(submitted).toHaveLength(0);
    // The ticket must not reach the session origin or web storage either.
    expect(api.state.launchRequests).toHaveLength(0);
    expect(JSON.stringify(localStorage)).not.toContain("tkt_");
  });

  it("refuses to launch when /v1/me has no session domain", async () => {
    const ws = readyWorkspace();
    const api = createMockApi();
    api.state.workspaces.set(ws.id, ws);
    const { submitted } = watchFormSubmits();
    loginCookies();
    const meNoDomain: Me = {
      subject: ME.subject,
      displayName: ME.displayName,
      tenant: ME.tenant,
      roles: ME.roles,
    };
    renderConnect(<ConnectButton workspace={ws} />, api, meNoDomain);

    const button = await screen.findByRole("button", { name: "Connect" });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);

    await screen.findByRole("alert");
    expect(submitted).toHaveLength(0);
  });
});
