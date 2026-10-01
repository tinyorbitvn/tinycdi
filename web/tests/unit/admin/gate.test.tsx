import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { failNextGet, makeAdmin } from "./helpers";
import { routes } from "../../../src/admin/routes";
import { QuotaPage } from "../../../src/admin/QuotaPage";
import { adminArea } from "../../mock-api/admin.ts";

describe("admin: hidden for users", () => {
  it("marks every admin route as requiring the tenant-admin role", () => {
    // The app shell reads `requires` to hide the nav entry and gate the
    // route for regular users; all admin routes must carry it.
    expect(routes.length).toBeGreaterThan(0);
    for (const route of routes) {
      expect(route.requires, `route ${route.path}`).toBe("tenant-admin");
      expect(route.path.startsWith("/admin"), `route ${route.path}`).toBe(true);
    }
  });

  it("redirects a non-admin from an admin page to /workspaces", async () => {
    const api = createMockApi({ areas: [adminArea] });
    loginCookies(); // default principal has roles: ["user"]
    renderWithApi(<QuotaPage />, api);

    // /v1/me confirms the role is absent; the layout redirects rather than
    // rendering any tenant data.
    await screen.findByText("Tenant administrators only");
    expect(window.location.pathname).toBe("/workspaces");
  });

  it("renders the page for a tenant admin", async () => {
    const api = createMockApi({ areas: [adminArea] });
    loginCookies();
    makeAdmin(api);
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    expect(window.location.pathname).not.toBe("/workspaces");
  });
});

describe("admin: 403 handled", () => {
  it("shows the not-allowed state when a tenant-scope request is forbidden", async () => {
    const api = createMockApi({ areas: [adminArea] });
    loginCookies();
    makeAdmin(api);
    // Role revoked mid-session: the next tenant read answers 403.
    failNextGet(api, "/v1/quota", 403, "FORBIDDEN");
    renderWithApi(<QuotaPage />, api);

    // The page chrome and an actionable error render — never a blank page.
    await screen.findByRole("heading", { name: "Quota" });
    await screen.findByText(/tenant-admin role/);
    expect(screen.getByRole("navigation", { name: "Admin sections" })).toBeInTheDocument();
  });
});
