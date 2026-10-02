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

  it("probes /v1/me first and treats its 401 as signed out, without logging", async () => {
    clearCookies();
    const seen: string[] = [];
    const base = createMockApi();
    const api = {
      ...base,
      handle: (req: Parameters<typeof base.handle>[0]) => {
        seen.push(`${req.method} ${req.path}`);
        return base.handle(req);
      },
    };
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
    expect(seen[0]).toBe("GET /v1/me");
    expect(seen).not.toContain("GET /v1/workspaces");
    await new Promise((r) => setTimeout(r, 0));
    expect(errors).not.toHaveBeenCalled();
    expect(rejections).toEqual([]);
    process.off("unhandledRejection", onRejection);
    errors.mockRestore();
  });

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
