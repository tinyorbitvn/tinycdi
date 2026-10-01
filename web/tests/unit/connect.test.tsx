import { describe, expect, it, vi, afterEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { ConnectButton } from "../../src/workspaces/ConnectButton";
import {
  createMockApi,
  renderWithApi,
  loginCookies,
  SESSION_COOKIE,
  SESSION_PRINCIPAL,
  CSRF_COOKIE,
  CSRF_TOKEN_VALUE,
} from "./helpers";
import { readyWorkspace } from "../mock-api/fixtures.ts";

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
  it("POSTs the launch ticket to the session origin in a new tab", async () => {
    const ws = readyWorkspace();
    const { submitted } = watchFormSubmits();
    const { api } = (() => {
      const a = createMockApi();
      a.state.workspaces.set(ws.id, ws);
      return { api: a };
    })();
    loginCookies();
    renderWithApi(<ConnectButton workspace={ws} />, api);

    fireEvent.click(screen.getByRole("button", { name: "Connect" }));

    await waitFor(() => expect(submitted).toHaveLength(1));
    const form = submitted[0];
    expect(form.method).toBe("post");
    expect(form.action).toBe("http://127.0.0.1:4311/v1/launch");
    expect(form.target).toBe("_blank");
    const ticketInput = form.querySelector(
      'input[name="ticket"]',
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
    const api = createMockApi();
    api.state.workspaces.set(ws.id, ws);
    api.state.leases.set(ws.id, "lease_existing");
    const { submitted } = watchFormSubmits();
    loginCookies();
    renderWithApi(<ConnectButton workspace={ws} />, api);

    fireEvent.click(screen.getByRole("button", { name: "Connect" }));
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
  // session origin the API published at login. A launchUrl on any other
  // origin must be refused before the form ever submits.
  it("refuses to POST the ticket to a foreign launchUrl origin", async () => {
    const ws = readyWorkspace();
    const api = createMockApi({ sessionOrigin: "https://evil.example" });
    api.state.workspaces.set(ws.id, ws);
    const { submitted } = watchFormSubmits();
    loginCookies(); // configured session origin stays http://127.0.0.1:4311
    renderWithApi(<ConnectButton workspace={ws} />, api);

    fireEvent.click(screen.getByRole("button", { name: "Connect" }));

    await screen.findByRole("alert");
    expect(submitted).toHaveLength(0);
    // The ticket must not reach the session origin or web storage either.
    expect(api.state.launchRequests).toHaveLength(0);
    expect(JSON.stringify(localStorage)).not.toContain("tkt_");
  });

  it("refuses to launch when no session origin is configured", async () => {
    const ws = readyWorkspace();
    const api = createMockApi();
    api.state.workspaces.set(ws.id, ws);
    const { submitted } = watchFormSubmits();
    // Session + CSRF cookies only: the session-origin cookie is absent.
    document.cookie = `${SESSION_COOKIE}=${SESSION_PRINCIPAL}; path=/`;
    document.cookie = `${CSRF_COOKIE}=${CSRF_TOKEN_VALUE}; path=/`;
    renderWithApi(<ConnectButton workspace={ws} />, api);

    fireEvent.click(screen.getByRole("button", { name: "Connect" }));

    await screen.findByRole("alert");
    expect(submitted).toHaveLength(0);
  });
});
