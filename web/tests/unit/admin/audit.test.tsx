import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin } from "./helpers";
import { AuditPage } from "../../../src/admin/AuditPage";
import { adminArea, TENANT } from "../../mock-api/admin.ts";

const AUDIT_PATH = `/v1/admin/tenants/${TENANT}/audit`;

function setup() {
  const api = createMockApi({ areas: [adminArea] });
  loginCookies();
  makeAdmin(api);
  renderWithApi(<AuditPage />, api);
  return api;
}

function auditRequests(api: ReturnType<typeof createMockApi>) {
  return api.state.requests.filter((r) => r.path === AUDIT_PATH);
}

describe("AuditPage", () => {
  it("lists the newest page and the denied outcome", async () => {
    setup();
    const table = await screen.findByRole("table", { name: "Audit events" });
    expect((await within(table).findAllByText("Denied")).length).toBeGreaterThan(0);
    await waitFor(() =>
      expect(within(table).getAllByRole("row")[1]).toHaveTextContent("workspace.start"),
    );
  });

  it("Apply sends the action filter", async () => {
    const api = setup();
    await screen.findByRole("table", { name: "Audit events" });
    fireEvent.change(screen.getByLabelText("Action"), { target: { value: "workspace.start" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(
        auditRequests(api).some(
          (r) => new URLSearchParams(r.query).get("action") === "workspace.start",
        ),
      ).toBe(true),
    );
    for (const r of auditRequests(api)) {
      expect(new URLSearchParams(r.query).get("sort")).toBe("-createdAt");
    }
    const table = screen.getByRole("table", { name: "Audit events" });
    await waitFor(() => {
      for (const row of within(table).getAllByRole("row").slice(1)) {
        expect(row).toHaveTextContent("workspace.start");
      }
    });
  });

  it("Clear drops the filter", async () => {
    const api = setup();
    await screen.findByRole("table", { name: "Audit events" });
    fireEvent.change(screen.getByLabelText("Action"), { target: { value: "workspace.start" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(
        auditRequests(api).some(
          (r) => new URLSearchParams(r.query).get("action") === "workspace.start",
        ),
      ).toBe(true),
    );
    const before = api.state.requests.length;
    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    await waitFor(() =>
      expect(
        api.state.requests
          .slice(before)
          .some(
            (r) => r.path === AUDIT_PATH && new URLSearchParams(r.query).get("action") === null,
          ),
      ).toBe(true),
    );
  });
  it("filters sit in a two-column Form with Apply/Clear in FormActions and a local-time hint", async () => {
    setup();
    const apply = await screen.findByRole("button", { name: "Apply" });
    expect(apply.closest("form")).toHaveClass("tc-form", "tc-form--2col");
    expect(apply.parentElement).toHaveClass("tc-form__actions");
    expect(screen.getByRole("button", { name: "Clear" }).parentElement).toBe(apply.parentElement);
    expect(screen.getAllByText("Local time")).toHaveLength(2);
  });

  it("denied outcomes use the danger tone", async () => {
    setup();
    const denied = (await screen.findAllByText("Denied"))[0]!;
    expect(denied.closest(".tc-badge")).toHaveClass("tc-badge--danger");
  });
});
