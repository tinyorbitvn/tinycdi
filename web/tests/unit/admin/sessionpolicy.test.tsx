import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, templatesStubArea } from "./helpers";
import { SessionPolicyPage, meetClipboard, meetFileTransfer } from "../../../src/admin/SessionPolicyPage";
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
      fileTransferPolicy: "Bidirectional",
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
      fileTransferPolicy: "Upload",
      networkProfile: "InternetOnly",
    } as TemplateFixture,
  );
  return api;
}

// The direction lattice the admin preview renders — the same meet the
// backend enforces (ADR 0009 §1): Disabled is bottom, Bidirectional top,
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

// The file-transfer channel runs the same lattice with Upload/Download
// as the incomparable directions (ADR 0009 §5).
describe("meetFileTransfer: direction lattice", () => {
  const cases: Array<[string, string, string]> = [
    ["Bidirectional", "FollowTemplate", "Bidirectional"],
    ["Upload", "Bidirectional", "Upload"],
    ["Bidirectional", "Upload", "Upload"],
    ["Bidirectional", "Download", "Download"],
    ["Upload", "Download", "Disabled"],
    ["Download", "Upload", "Disabled"],
    ["Upload", "Upload", "Upload"],
    ["Disabled", "Bidirectional", "Disabled"],
    ["Bidirectional", "Disabled", "Disabled"],
  ];
  for (const [template, ceiling, want] of cases) {
    it(`${template} ∩ ${ceiling} = ${want}`, () => {
      expect(meetFileTransfer(template, ceiling)).toBe(want);
    });
  }
});

describe("session policy page", () => {
  it("renders the five ceiling options, the live file-transfer controls and the meet preview", async () => {
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
    const ft = screen.getByLabelText("File transfer", { exact: true });
    expect(ft).toBeEnabled();
    expect(ft).toHaveValue("FollowTemplate");

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

  it("the file-transfer ceiling is live: mode and size ride the PUT", async () => {
    const api = setup();
    renderWithApi(<SessionPolicyPage />, api);
    const ft = await screen.findByLabelText("File transfer", { exact: true });
    fireEvent.change(ft, { target: { value: "Upload" } });
    fireEvent.change(screen.getByLabelText("Largest file (MiB)"), { target: { value: "10" } });
    fireEvent.click(screen.getByRole("button", { name: "Save ceiling" }));
    await waitFor(() =>
      expect(
        api.state.requests.some((r) => {
          if (r.method !== "PUT" || r.path !== "/v1/admin/tenants/acme/session-policy") return false;
          const body = JSON.parse(r.rawBody) as { fileTransfer?: { mode?: string; maxFileBytes?: number } };
          return body.fileTransfer?.mode === "Upload" && body.fileTransfer?.maxFileBytes === 10485760;
        }),
      ).toBe(true),
    );
  });
  it("lays the ceiling out as a two-column Form with Save as the submit action", async () => {
    renderWithApi(<SessionPolicyPage />, setup());
    const save = await screen.findByRole("button", { name: "Save ceiling" });
    expect(save).toHaveAttribute("type", "submit");
    const form = save.closest("form")!;
    expect(form).toHaveClass("tc-form", "tc-form--2col");
    expect(save.parentElement).toHaveClass("tc-form__actions");
    const info = form.querySelector(".tc-alert--info");
    expect(info).not.toBeNull();
    expect(info!.parentElement).toBe(form);
  });
});
