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
