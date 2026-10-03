import { it, expect } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { CreateWorkspacePage } from "../../../src/workspaces/CreateWorkspacePage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { TEMPLATE_BROWSER } from "../../mock-api/fixtures.ts";

it("dbg", async () => {
  const api = createMockApi();
  loginCookies();
  const orig = api.handle;
  api.handle = (req) => {
    if (req.method === "POST" && req.path === "/v1/workspaces") {
      return { status: 409, headers: {"content-type":"application/json"}, body: { code: "IMAGE_STALE", message: 'template "linux-chromium-browser" runtime image is 60 days old (limit 45 days)', retryable: false, requestId: "r-stale", details: { templateName: "linux-chromium-browser", ageDays: 60, limitDays: 45, pinned: true } } };
    }
    return orig(req);
  };
  renderWithApi(<CreateWorkspacePage />, api);
  const nameInput = await screen.findByLabelText(/Name/);
  fireEvent.change(nameInput, { target: { value: "my-box" } });
  const select = await screen.findByLabelText(/Template/);
  await waitFor(() => expect(select.querySelectorAll("option").length).toBeGreaterThan(1));
  fireEvent.change(select, { target: { value: TEMPLATE_BROWSER.id } });
  fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
  await new Promise((r) => setTimeout(r, 1500));
  console.log("ALERTS:", screen.getAllByRole("alert").map(a => a.textContent));
  console.log("REQS:", api.state.requests.filter((r)=>r.method==="POST").map((r)=>r.path));
});
