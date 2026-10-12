import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, templatesStubArea } from "./helpers";
import { SessionPolicyPage, meetClipboard } from "../../../src/admin/SessionPolicyPage";
import { adminArea } from "../../mock-api/admin.ts";
import type { TemplateFixture } from "../../mock-api/fixtures.ts";

function setup() {
  const api = createMockApi({ areas: [adminArea, templatesStubArea] });
  loginCookies();
  makeAdmin(api);
  api.state.templates.push(
    {
      id: "tpl_bidi",
      name: "bidi-template",
      family: "bidi-template",
      description: "",
      revision: 1,
      runtime: "LinuxContainer",
      experience: "Desktop",
      resources: { cpuMillicores: 1000, memoryMib: 1024, storageGib: 10 },
      lifecycleDefaults: {},
      dataPolicyDefault: "Ephemeral",
      clipboardPolicy: "Bidirectional",
      networkProfile: "InternetOnly",
    } as TemplateFixture,
    {
      id: "tpl_send",
      name: "send-template",
      family: "send-template",
      description: "",
      revision: 1,
      runtime: "LinuxContainer",
      experience: "Desktop",
      resources: { cpuMillicores: 1000, memoryMib: 1024, storageGib: 10 },
      lifecycleDefaults: {},
      dataPolicyDefault: "Ephemeral",
      clipboardPolicy: "Send",
      networkProfile: "InternetOnly",
    } as TemplateFixture,
  );
  return api;
}

// The direction lattice the admin preview renders — the same meet the
// backend enforces (ADR 0010 §1): Disabled is bottom, Bidirectional top,
// Send ∩ Receive = Disabled, a ceiling can only narrow.
describe("meetClipboard: direction lattice", () => {
  const cases: Array<[string, string, string]> = [
    // FollowTemplate / Bidirectional ceiling narrows nothing.
    ["Bidirectional", "FollowTemplate", "Bidirectional"],
    ["Send", "FollowTemplate", "Send"],
    ["Disabled", "FollowTemplate", "Disabled"],
    ["Send", "Bidirectional", "Send"],
    // Disabled ceiling disables everything.
    ["Bidirectional", "Disabled", "Disabled"],
    ["Send", "Disabled", "Disabled"],
    // One-way ceiling over Bidirectional narrows to the direction.
    ["Bidirectional", "Send", "Send"],
    ["Bidirectional", "Receive", "Receive"],
    // Cross directions meet at Disabled.
    ["Send", "Receive", "Disabled"],
    ["Receive", "Send", "Disabled"],
    ["Disabled", "Send", "Disabled"],
    // Same direction over same direction stays.
    ["Send", "Send", "Send"],
    ["Receive", "Receive", "Receive"],
    // A wider ceiling never widens a narrower template.
    ["Disabled", "Bidirectional", "Disabled"],
  ];
  for (const [template, ceiling, want] of cases) {
    it(`${template} ∩ ${ceiling} = ${want}`, () => {
      expect(meetClipboard(template, ceiling)).toBe(want);
    });
  }
});

describe("session policy page", () => {
  it("renders the five ceiling options, the inert file-transfer control and the meet preview", async () => {
    const api = setup();
    renderWithApi(<SessionPolicyPage />, api);

    const clipboard = await screen.findByLabelText("Clipboard", { exact: true });
    const values = within(clipboard)
      .getAllByRole("option")
      .map((o) => (o as HTMLOptionElement).value);
    expect(values).toEqual([
      "FollowTemplate",
      "Disabled",
      "Send",
      "Receive",
      "Bidirectional",
    ]);
    expect(clipboard).toHaveValue("FollowTemplate");
    // The inert v1.3 channel is present but not editable.
    expect(screen.getByLabelText("File transfer", { exact: true })).toBeDisabled();

    const preview = screen.getByRole("table", {
      name: "Effective clipboard policy by template",
    });
    const bidiRow = within(preview).getByRole("row", { name: /bidi-template/ });
    // FollowTemplate ceiling: effective = the template's own policy.
    expect(within(bidiRow).getAllByRole("cell").map((c) => c.textContent)).toContain(
      "Both directions",
    );
  });

  it("saving a ceiling PUTs with If-Match and FollowTemplate deletes the row", async () => {
    const api = setup();
    renderWithApi(<SessionPolicyPage />, api);

    const clipboard = await screen.findByLabelText("Clipboard", { exact: true });
    // No row yet: If-Match "*" creates it.
    fireEvent.change(clipboard, { target: { value: "Disabled" } });
    fireEvent.click(screen.getByRole("button", { name: "Save ceiling" }));
    await waitFor(() =>
      expect(
        api.state.requests.some(
          (r) =>
            r.method === "PUT" &&
            r.path === "/v1/admin/tenants/acme/session-policy" &&
            r.headers["if-match"] === "*",
        ),
      ).toBe(true),
    );
    // The save's reload has landed once the row source renders.
    await screen.findByText("A ceiling is set for this tenant.");
    expect(clipboard).toHaveValue("Disabled");

    // Back to Follow template: the PUT deletes the row.
    fireEvent.change(clipboard, { target: { value: "FollowTemplate" } });
    fireEvent.click(screen.getByRole("button", { name: "Save ceiling" }));
    await waitFor(() =>
      expect(
        api.state.requests.filter(
          (r) => r.method === "PUT" && r.path === "/v1/admin/tenants/acme/session-policy",
        ).length,
      ).toBe(2),
    );
    await waitFor(() =>
      expect(screen.getByText(/No ceiling is set/)).toBeInTheDocument(),
    );
  });
});
