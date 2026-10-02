import { describe, expect, it } from "vitest";
import { screen, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant, templatesStubArea } from "./helpers";
import { TemplatesPage, catalogRows } from "../../../src/admin/TemplatesPage";
import type { AdminTemplateView, ScopedWorkspace } from "../../../src/admin/api";
import { adminArea } from "../../mock-api/admin.ts";
import {
  TEMPLATE_BROWSER,
  TEMPLATE_LINUX,
  type TemplateFixture,
  type WorkspaceFixture,
} from "../../mock-api/fixtures.ts";

describe("templates: stale image column", () => {
  it("shows the build date and stale badge; unknown age is labelled Unknown", async () => {
    const api = createMockApi({ areas: [adminArea, templatesStubArea] });
    loginCookies();
    makeAdmin(api);
    seedTenant(api);
    api.state.templates.push(
      {
        ...structuredClone(TEMPLATE_LINUX),
        imageBuiltAt: "2026-09-10T00:00:00Z",
        imageStale: true,
      } as TemplateFixture,
      structuredClone(TEMPLATE_BROWSER),
    );
    renderWithApi(<TemplatesPage />, api);

    await screen.findByRole("heading", { name: "Template catalog" });
    const table = await screen.findByRole("table", { name: "Template catalog" });
    expect(within(table).getByRole("columnheader", { name: "Image" })).toBeInTheDocument();

    const linuxRow = within(table)
      .getByRole("rowheader", { name: /linux-firefox-desktop/ })
      .closest("tr")!;
    // The row has two <time> elements (image build date and published date);
    // the image column's carries the build timestamp.
    expect(linuxRow.querySelector('time[dateTime="2026-09-10T00:00:00Z"]')).not.toBeNull();
    expect(within(linuxRow).getByText("Stale")).toBeInTheDocument();

    const browserRow = within(table)
      .getByRole("rowheader", { name: /linux-chromium-browser/ })
      .closest("tr")!;
    expect(within(browserRow).getByText("Unknown")).toBeInTheDocument();
  });
});

describe("templates: usage join", () => {
  // A chart upgrade publishes a new revision object (new id) and removes the
  // old one; workspaces created on the old revision keep its id but share the
  // family.
  const FAMILY = "linux-firefox-desktop";
  const current = {
    ...structuredClone(TEMPLATE_LINUX),
    id: "tpl_REV8",
    revision: 8,
    family: FAMILY,
  } as AdminTemplateView;
  const other = {
    ...structuredClone(TEMPLATE_BROWSER),
    family: "linux-chromium-browser",
  } as AdminTemplateView;
  const ws = (id: string, tplId: string, revision: number, family: string, desiredState = "Running") =>
    ({
      id,
      name: id,
      phase: "Ready",
      desiredState,
      template: { id: tplId, name: family, family, revision, runtime: "LinuxContainer", experience: "Desktop" },
    }) as unknown as ScopedWorkspace;

  it("counts workspaces pinned to a superseded revision of the same family", () => {
    const rows = catalogRows(
      [current, other],
      [
        ws("a", "tpl_REV7", 7, FAMILY),
        ws("b", "tpl_REV8", 8, FAMILY, "Stopped"),
        ws("c", "tpl_REV6", 6, FAMILY),
      ],
    );
    const row = rows.find((r) => r.template.id === "tpl_REV8")!;
    expect(row.inUse).toBe(3);
    expect(row.running).toBe(2);
    expect(rows.find((r) => r.template.id === other.id)!.inUse).toBe(0);
  });

  it("renders the in-use count for a superseded revision in the table", async () => {
    const api = createMockApi({ areas: [adminArea, templatesStubArea] });
    loginCookies();
    makeAdmin(api);
    api.state.templates.push(current as TemplateFixture);
    api.state.workspaces.set("ws_OLD", ws("ws_OLD", "tpl_REV7", 7, FAMILY) as unknown as WorkspaceFixture);
    renderWithApi(<TemplatesPage />, api);

    const table = await screen.findByRole("table", { name: "Template catalog" });
    const row = within(table).getByRole("rowheader", { name: /linux-firefox-desktop/ }).closest("tr")!;
    await within(row).findByLabelText("1 workspaces, 1 running");
  });
});
