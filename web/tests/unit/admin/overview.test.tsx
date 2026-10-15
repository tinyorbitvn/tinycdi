import { describe, expect, it } from "vitest";
import { screen, within } from "@testing-library/react";
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
    expect(within(health).getByRole("group", { name: "Start time (p95, 24 h)" })).toHaveTextContent("12 s");
    expect(health).toHaveTextContent("7 starts measured");
    const stuck = within(health).getByRole("group", { name: "Stuck deletions" });
    expect(stuck).toHaveClass("tc-stat--warning");
    expect(within(stuck).getByRole("link", { name: "Review stuck deletions" })).toHaveAttribute(
      "href",
      "/admin/workspaces",
    );
    const running = within(health).getByRole("group", { name: "Running by template" });
    expect(within(running).getByText("chrome-r1").nextElementSibling).toHaveTextContent("2");
  });

  it("says so when the Kubernetes watch is stale", async () => {
    const api = setup();
    control(api, "/_control/admin/overview", { fresh: false });
    renderWithApi(<OverviewPage />, api);
    expect(await screen.findByText(/Workspace status is out of date/)).toBeInTheDocument();
    expect(screen.getByText("Nothing running")).toBeInTheDocument();
  });
});

describe("overview: capacity and retained data", () => {
  it("lays the meters out in the auto-fit grid and shows retained disks as a Stat", async () => {
    renderWithApi(<OverviewPage />, setup());
    const capacity = await screen.findByRole("region", { name: "Capacity" });
    expect(capacity.querySelector(".tc-meter-grid")).not.toBeNull();
    expect(await screen.findByRole("group", { name: "Retained disks" })).toHaveClass("tc-stat");
  });
});
