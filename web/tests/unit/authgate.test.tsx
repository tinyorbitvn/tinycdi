import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { AuthGate, loginUrl } from "../../src/auth/AuthGate";
import {
  createMockApi,
  renderWithApi,
  loginCookies,
  clearCookies,
} from "./helpers";

describe("AuthGate", () => {
  it("computes the /v1 login redirect with a returnTo", () => {
    expect(loginUrl("/workspaces/ws_abc?x=1")).toBe(
      "/v1/login?returnTo=%2Fworkspaces%2Fws_abc%3Fx%3D1",
    );
  });

  it("redirects to /v1/login when the API answers UNAUTHENTICATED", async () => {
    clearCookies();
    const onUnauthenticated = vi.fn();
    renderWithApi(
      <AuthGate onUnauthenticated={onUnauthenticated}>
        <div>secret content</div>
      </AuthGate>,
      createMockApi(),
    );

    await waitFor(() => expect(onUnauthenticated).toHaveBeenCalledOnce());
    expect(screen.queryByText("secret content")).not.toBeInTheDocument();
  });

  // The probe never answers 401, so a signed-out load issues no failed request
  // (Chrome logs every failed fetch to the console, whatever the page does).
  function recordingApi() {
    const seen: string[] = [];
    const base = createMockApi();
    const api = {
      ...base,
      handle: (req: Parameters<typeof base.handle>[0]) => {
        seen.push(`${req.method} ${req.path}`);
        return base.handle(req);
      },
    };
    return { api, seen };
  }

  it("probes /v1/session first and, when signed out, never calls /v1/me", async () => {
    clearCookies();
    const { api, seen } = recordingApi();
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const rejections: unknown[] = [];
    const onRejection = (e: unknown) => rejections.push(e);
    process.on("unhandledRejection", onRejection);
    const onUnauthenticated = vi.fn();
    renderWithApi(
      <AuthGate onUnauthenticated={onUnauthenticated}>
        <div>secret content</div>
      </AuthGate>,
      api,
    );

    await waitFor(() => expect(onUnauthenticated).toHaveBeenCalledOnce());
    expect(seen).toEqual(["GET /v1/session"]);
    expect(screen.queryByText("secret content")).not.toBeInTheDocument();
    await new Promise((r) => setTimeout(r, 0));
    expect(errors).not.toHaveBeenCalled();
    expect(rejections).toEqual([]);
    process.off("unhandledRejection", onRejection);
    errors.mockRestore();
  });

  it("calls /v1/me only after the probe says authenticated", async () => {
    loginCookies();
    const { api, seen } = recordingApi();
    renderWithApi(
      <AuthGate onUnauthenticated={vi.fn()}>
        <div>secret content</div>
      </AuthGate>,
      api,
    );
    expect(await screen.findByText("secret content")).toBeInTheDocument();
    expect(seen.slice(0, 2)).toEqual(["GET /v1/session", "GET /v1/me"]);
    expect(seen).not.toContain("GET /v1/workspaces");
  });

  it("redirects when the session dies between the probe and /v1/me", async () => {
    loginCookies();
    const base = createMockApi();
    const api = {
      ...base,
      handle: (req: Parameters<typeof base.handle>[0]) =>
        req.path === "/v1/me"
          ? { status: 401, headers: { "content-type": "application/json" }, body: { code: "UNAUTHENTICATED", message: "expired", retryable: false } }
          : base.handle(req),
    };
    const onUnauthenticated = vi.fn();
    renderWithApi(
      <AuthGate onUnauthenticated={onUnauthenticated}>
        <div>secret content</div>
      </AuthGate>,
      api,
    );
    await waitFor(() => expect(onUnauthenticated).toHaveBeenCalledOnce());
    expect(screen.queryByText("secret content")).not.toBeInTheDocument();
  });

  it("shows the unreachable alert when the probe fails", async () => {
    clearCookies();
    const base = createMockApi();
    const api = {
      ...base,
      handle: (req: Parameters<typeof base.handle>[0]) =>
        req.path === "/v1/session"
          ? { status: 503, headers: { "content-type": "application/json" }, body: { code: "UNAVAILABLE", message: "down", retryable: true } }
          : base.handle(req),
    };
    const onUnauthenticated = vi.fn();
    renderWithApi(
      <AuthGate onUnauthenticated={onUnauthenticated}>
        <div>secret content</div>
      </AuthGate>,
      api,
    );
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(onUnauthenticated).not.toHaveBeenCalled();
    expect(screen.queryByText("secret content")).not.toBeInTheDocument();
  });

  it.each([404, 501])(
    "does not open the gate on a %i from /v1/me (every backend ships /v1/me)",
    async (status) => {
      loginCookies();
      const base = createMockApi();
      const api = {
        ...base,
        handle: (req: Parameters<typeof base.handle>[0]) =>
          req.path === "/v1/me"
            ? { status, headers: { "content-type": "application/json" }, body: { code: "NOT_FOUND", message: "no route", retryable: false } }
            : base.handle(req),
      };
      renderWithApi(
        <AuthGate onUnauthenticated={vi.fn()}>
          <div>secret content</div>
        </AuthGate>,
        api,
      );
      expect(await screen.findByRole("alert")).toBeInTheDocument();
      expect(screen.queryByText("secret content")).not.toBeInTheDocument();
    },
  );

  it("renders children once the session check passes", async () => {
    loginCookies();
    const onUnauthenticated = vi.fn();
    renderWithApi(
      <AuthGate onUnauthenticated={onUnauthenticated}>
        <div>secret content</div>
      </AuthGate>,
      createMockApi(),
    );
    expect(await screen.findByText("secret content")).toBeInTheDocument();
    expect(onUnauthenticated).not.toHaveBeenCalled();
  });
});
