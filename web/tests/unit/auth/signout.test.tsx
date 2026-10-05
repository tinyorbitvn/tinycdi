import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApiProvider } from "../../../src/api/context";
import { createApi } from "../../../src/api/client";
import { App } from "../../../src/App";
import { SignedOut } from "../../../src/auth/SignedOut";
import { SIGN_IN_AGAIN_URL, SIGNED_OUT_PATH, signOut, signOutEverywhere } from "../../../src/auth/signOut";
import { AppShell, BrandingProvider } from "../../../src/app/shell";
import { MeProvider, type Me } from "../../../src/app/me";
import { ThemeProvider } from "../../../src/app/theme";
import { LocaleProvider } from "../../../src/app/locale";
import { DEFAULT_BRANDING } from "../../../src/app/branding";
import type { RouteArea } from "../../../src/app/routes";
import { ToastProvider } from "../../../src/design";
import { clearCookies, createMockApi, loginCookies, stubFetch } from "../helpers";

// FX-R21: sign-out. The user menu calls POST /v1/logout (CSRF header added
// by the API client) and leaves for the identity provider's end-session URL
// or the public signed-out page, which never starts a login on its own.

const IDP_LOGOUT = "https://idp.example/logout?client_id=tinycdi-portal";

function setEndSession(api: ReturnType<typeof createMockApi>, url: string | null) {
  const r = api.handle({
    method: "POST",
    path: "/_control/auth/endSession",
    query: new URLSearchParams(),
    headers: {},
    rawBody: "",
    ...(url ? { body: { url } } : {}),
  });
  expect(r.status).toBe(200);
}

describe("signOut", () => {
  beforeEach(() => loginCookies());
  afterEach(() => clearCookies());

  it("204 (no provider end-session): leaves for the signed-out page", async () => {
    const api = createMockApi();
    const assign = vi.fn();
    await signOut(createApi(stubFetch(api)), assign);
    expect(assign).toHaveBeenCalledExactlyOnceWith(SIGNED_OUT_PATH);
    // The mock enforces session + CSRF on POST: reaching 204 proves the
    // X-CSRF-Token header went out.
    expect(api.state.requests.map((r) => `${r.method} ${r.path}`)).toEqual(["POST /v1/logout"]);
  });

  it("200: continues at the end-session URL the backend returned", async () => {
    const api = createMockApi();
    setEndSession(api, IDP_LOGOUT);
    const assign = vi.fn();
    await signOut(createApi(stubFetch(api)), assign);
    expect(assign).toHaveBeenCalledExactlyOnceWith(IDP_LOGOUT);
  });

  it("refuses a non-http(s) end-session URL and falls back to the signed-out page", async () => {
    const api = createMockApi();
    setEndSession(api, "javascript:alert(1)");
    const assign = vi.fn();
    await signOut(createApi(stubFetch(api)), assign);
    expect(assign).toHaveBeenCalledExactlyOnceWith(SIGNED_OUT_PATH);
  });

  it("401 (session already gone): still ends on the signed-out page", async () => {
    clearCookies();
    const assign = vi.fn();
    await signOut(createApi(stubFetch(createMockApi())), assign);
    expect(assign).toHaveBeenCalledExactlyOnceWith(SIGNED_OUT_PATH);
  });

  it("rejects on any other failure and does not navigate", async () => {
    const assign = vi.fn();
    const failing = (async () =>
      new Response(JSON.stringify({ code: "INTERNAL", message: "boom", retryable: true }), {
        status: 500,
        headers: { "content-type": "application/json" },
      })) as unknown as typeof fetch;
    await expect(signOut(createApi(failing), assign)).rejects.toMatchObject({ httpStatus: 500 });
    expect(assign).not.toHaveBeenCalled();
  });
});

describe("signOutEverywhere (ADR 0007)", () => {
  beforeEach(() => loginCookies());
  afterEach(() => clearCookies());

  it("posts the revoke-all endpoint and leaves for the signed-out page on 204", async () => {
    const api = createMockApi();
    const assign = vi.fn();
    await signOutEverywhere(createApi(stubFetch(api)), assign);
    expect(assign).toHaveBeenCalledExactlyOnceWith(SIGNED_OUT_PATH);
    expect(api.state.requests.map((r) => `${r.method} ${r.path}`)).toEqual([
      "POST /v1/me/sessions:revoke-all",
    ]);
  });

  it("200: continues at the provider end-session URL like plain sign-out", async () => {
    const api = createMockApi();
    setEndSession(api, IDP_LOGOUT);
    const assign = vi.fn();
    await signOutEverywhere(createApi(stubFetch(api)), assign);
    expect(assign).toHaveBeenCalledExactlyOnceWith(IDP_LOGOUT);
  });

  it("rejects on failure — the caller stays signed in", async () => {
    const assign = vi.fn();
    const failing = (async () =>
      new Response(JSON.stringify({ code: "INTERNAL", message: "boom", retryable: true }), {
        status: 500,
        headers: { "content-type": "application/json" },
      })) as unknown as typeof fetch;
    await expect(signOutEverywhere(createApi(failing), assign)).rejects.toMatchObject({ httpStatus: 500 });
    expect(assign).not.toHaveBeenCalled();
  });
});

describe("signed-out page", () => {
  afterEach(() => window.history.pushState(null, "", "/"));

  it("offers 'Sign in again' as a plain link to an explicit login", () => {
    render(<SignedOut />);
    expect(screen.getByRole("heading", { name: "You have signed out" })).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "Sign in again" });
    expect(link).toHaveAttribute("href", SIGN_IN_AGAIN_URL);
    expect(SIGN_IN_AGAIN_URL).toBe("/v1/login?returnTo=%2F");
  });

  it("at /signed-out the app makes no API request — no probe, no /v1/login", async () => {
    clearCookies();
    window.history.pushState(null, "", SIGNED_OUT_PATH);
    const seen: string[] = [];
    const base = stubFetch(createMockApi());
    const spy = (async (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push(String(input instanceof Request ? input.url : input));
      return base(input, init);
    }) as typeof fetch;
    const assign = vi.fn();
    const loc = window.location;
    Object.defineProperty(window, "location", { value: { ...loc, assign, pathname: loc.pathname }, configurable: true });
    try {
      render(
        <ApiProvider client={createApi(spy)}>
          <App />
        </ApiProvider>,
      );
      expect(await screen.findByRole("link", { name: "Sign in again" })).toBeInTheDocument();
      await new Promise((r) => setTimeout(r, 20));
    } finally {
      Object.defineProperty(window, "location", { value: loc, configurable: true });
    }
    expect(seen.filter((u) => !u.includes("/branding/"))).toEqual([]);
    expect(assign).not.toHaveBeenCalled();
  });
});

describe("user menu", () => {
  const AREAS: RouteArea[] = [
    {
      name: "workspaces",
      owns: () => true,
      load: async () => ({ routes: [{ path: "/*", title: "Home", render: () => <div>Page content</div> }] }),
    },
  ];
  const ME: Me = { subject: "u1", displayName: "Ada Admin", tenant: "acme", roles: [] };

  function renderShell(api = createMockApi()) {
    const logout = vi.spyOn(api, "handle");
    render(
      <ApiProvider client={createApi(stubFetch(api))}>
        <ThemeProvider>
          <BrandingProvider load={async () => DEFAULT_BRANDING}>
            <LocaleProvider>
              <ToastProvider>
                <MeProvider load={async () => ME}>
                  <AppShell areas={AREAS} />
                </MeProvider>
              </ToastProvider>
            </LocaleProvider>
          </BrandingProvider>
        </ThemeProvider>
      </ApiProvider>,
    );
    return { api, logout };
  }

  beforeEach(() => loginCookies());
  afterEach(() => clearCookies());

  it("is a labelled menu button on the account name", async () => {
    renderShell();
    const trigger = await screen.findByRole("button", { name: "Ada Admin" });
    expect(trigger).toHaveAttribute("aria-haspopup", "menu");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(trigger);
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    const menu = screen.getByRole("menu", { name: "Account menu for Ada Admin" });
    expect(menu).toHaveTextContent("Signed in as Ada Admin");
    expect(menu).toHaveTextContent("Tenant acme");
    expect(screen.getByRole("menuitem", { name: "Sign out" })).toBeInTheDocument();
  });

  it("is keyboard reachable: ArrowDown opens it on 'Sign out', Escape closes and restores focus", async () => {
    renderShell();
    const trigger = await screen.findByRole("button", { name: "Ada Admin" });
    trigger.focus();
    fireEvent.keyDown(trigger, { key: "ArrowDown" });
    const item = await screen.findByRole("menuitem", { name: "Sign out" });
    await waitFor(() => expect(item).toHaveFocus());
    fireEvent.keyDown(item, { key: "Escape" });
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it("choosing 'Sign out' posts /v1/logout and, with no session left, ends on the signed-out page", async () => {
    clearCookies(); // no session: the mock answers 401 → signed-out page
    const { logout } = renderShell();
    const trigger = await screen.findByRole("button", { name: "Ada Admin" });
    fireEvent.click(trigger);
    const loc = window.location;
    const assign = vi.fn();
    Object.defineProperty(window, "location", { value: { ...loc, assign, pathname: loc.pathname }, configurable: true });
    try {
      fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
      await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGNED_OUT_PATH));
    } finally {
      Object.defineProperty(window, "location", { value: loc, configurable: true });
    }
    expect(logout.mock.calls.some(([r]) => r.method === "POST" && r.path === "/v1/logout")).toBe(true);
  });

  it("'Sign out everywhere' confirms, naming the tenant, then posts revoke-all", async () => {
    const { api } = renderShell();
    const trigger = await screen.findByRole("button", { name: "Ada Admin" });
    fireEvent.click(trigger);
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out everywhere" }));

    // The confirm dialog names the tenant scope before anything is sent.
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toHaveTextContent("tenant acme");
    expect(dialog).toHaveTextContent("including this one");
    expect(api.state.requests.some((r) => r.path.includes("revoke-all"))).toBe(false);

    const loc = window.location;
    const assign = vi.fn();
    Object.defineProperty(window, "location", { value: { ...loc, assign, pathname: loc.pathname }, configurable: true });
    try {
      fireEvent.click(screen.getByRole("button", { name: "Sign out everywhere" }));
      await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGNED_OUT_PATH));
    } finally {
      Object.defineProperty(window, "location", { value: loc, configurable: true });
    }
    expect(api.state.requests.map((r) => `${r.method} ${r.path}`)).toContain("POST /v1/me/sessions:revoke-all");
  });

  it("the revoke-all confirm can be cancelled without any request", async () => {
    const { api } = renderShell();
    fireEvent.click(await screen.findByRole("button", { name: "Ada Admin" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out everywhere" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(api.state.requests.some((r) => r.path.includes("revoke-all"))).toBe(false);
  });

  it("a failed sign-out stays on the page and says so", async () => {
    const api = createMockApi();
    const base = api.handle;
    api.handle = (req) =>
      req.path === "/v1/logout"
        ? { status: 500, headers: { "content-type": "application/json" }, body: { code: "INTERNAL", message: "boom", retryable: true } }
        : base(req);
    renderShell(api);
    fireEvent.click(await screen.findByRole("button", { name: "Ada Admin" }));
    const loc = window.location;
    const assign = vi.fn();
    Object.defineProperty(window, "location", { value: { ...loc, assign, pathname: loc.pathname }, configurable: true });
    try {
      fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
      expect(await screen.findByText("Could not sign out")).toBeInTheDocument();
      expect(assign).not.toHaveBeenCalled();
    } finally {
      Object.defineProperty(window, "location", { value: loc, configurable: true });
    }
  });
});
