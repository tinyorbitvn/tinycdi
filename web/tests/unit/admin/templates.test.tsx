import { describe, expect, it } from "vitest";
import { screen, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant, templatesStubArea } from "./helpers";
import { TemplatesPage } from "../../../src/admin/TemplatesPage";
import { adminArea } from "../../mock-api/admin.ts";
import {
  TEMPLATE_BROWSER,
  TEMPLATE_LINUX,
  type TemplateFixture,
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
