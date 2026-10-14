import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { control, makeAdmin } from "./helpers";
import { OverviewPage } from "../../../src/admin/OverviewPage";
import { adminArea } from "../../mock-api/admin.ts";
import { dataArea } from "../../mock-api/data.ts";

function setup() {
  const api = createMockApi({ areas: [adminArea, dataArea] });
  loginCookies();
  makeAdmin(api);
  return api;
}

describe("overview: health cards", () => {
  it("shows live sessions, p95, sample count, the stuck-deletion warning and running per template", async () => {
    renderWithApi(<OverviewPage />, setup());
    const health = await screen.findByRole("region", { name: "Health" });
    expect(health).toHaveTextContent("12 s");
    expect(health).toHaveTextContent("7 starts measured");
    expect(health).toHaveTextContent("Stuck deletions");
    expect(health.querySelector(".tc-badge--warning")).not.toBeNull();
    expect(health).toHaveTextContent("chrome-r1 — 2");
  });

  it("says so when the Kubernetes watch is stale", async () => {
    const api = setup();
    control(api, "/_control/admin/overview", { fresh: false });
    renderWithApi(<OverviewPage />, api);
    expect(await screen.findByText(/Workspace status is out of date/)).toBeInTheDocument();
    expect(screen.getByText("Nothing running")).toBeInTheDocument();
  });
});
