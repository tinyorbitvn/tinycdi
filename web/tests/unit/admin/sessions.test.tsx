import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { clearCookies, createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin } from "./helpers";
import { UserSessionsSection } from "../../../src/admin/UserSessionsSection";
import { SIGNED_OUT_PATH } from "../../../src/auth/signOut";
import { TENANT } from "../../mock-api/admin.ts";

// Answers the admin "sign out everywhere" POST; endsOwnSession models the
// admin revoking their own principal, which kills this browser's portal
// session as well.
function interceptRevoke(api: ReturnType<typeof createMockApi>, endsOwnSession: boolean) {
  const origHandle = api.handle;
  api.handle = (req) => {
    if (req.method === "POST" && req.path.endsWith("/sessions:revoke-all")) {
      if (endsOwnSession) clearCookies();
      return {
        status: 200,
        headers: { "content-type": "application/json" },
        body: { sessions: 2, tickets: 0, leases: 1 },
      };
    }
    return origHandle(req);
  };
}

async function signOutEverywhere() {
  fireEvent.click(await screen.findByRole("button", { name: "Sign out everywhere" }));
  const dialog = await screen.findByRole("alertdialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Sign out everywhere" }));
}

describe("UserSessionsSection sign out everywhere", () => {
  it("on the admin's own sessions: success, no UNAUTHENTICATED alert, then the signed-out page", async () => {
    const api = createMockApi();
    makeAdmin(api);
    loginCookies();
    interceptRevoke(api, true);
    const leave = vi.fn();
    renderWithApi(
      <UserSessionsSection tenant={TENANT} ownerRef="iss|me" displayName="Me" leave={leave} />,
      api,
    );
    await signOutEverywhere();

    const status = await screen.findByRole("status");
    expect(status).toHaveTextContent("Signed out: 2 portal sessions, 1 desktop sessions.");
    expect(status).toHaveTextContent("Your own portal session was signed out too.");
    await waitFor(() => expect(leave).toHaveBeenCalledExactlyOnceWith(SIGNED_OUT_PATH), { timeout: 5000 });
    expect(screen.queryByText(/UNAUTHENTICATED/)).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  }, 20000);

  it("on another user's sessions: the admin stays signed in and the list reloads", async () => {
    const api = createMockApi();
    makeAdmin(api);
    loginCookies();
    interceptRevoke(api, false);
    const leave = vi.fn();
    renderWithApi(
      <UserSessionsSection tenant={TENANT} ownerRef="iss|grace" displayName="Grace" leave={leave} />,
      api,
    );
    await signOutEverywhere();

    const status = await screen.findByRole("status");
    expect(status).toHaveTextContent("Signed out: 2 portal sessions, 1 desktop sessions.");
    expect(status).not.toHaveTextContent("Your own portal session");
    await new Promise((r) => setTimeout(r, 50));
    expect(leave).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  }, 20000);
});
