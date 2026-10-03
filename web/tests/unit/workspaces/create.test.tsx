import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { CreateWorkspacePage } from "../../../src/workspaces/CreateWorkspacePage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { TEMPLATE_LINUX, TEMPLATE_BROWSER } from "../../mock-api/fixtures.ts";

interface CreateCall {
  headers: Record<string, string>;
  rawBody: string;
}

// Intercepts POST /v1/workspaces so the test controls failures while still
// recording every attempt (including ones rejected before the mock's own
// request log).
function interceptCreates(
  api: ReturnType<typeof createMockApi>,
  failFirst: { status: number; body: object } | null,
) {
  const calls: CreateCall[] = [];
  const origHandle = api.handle;
  api.handle = (req) => {
    if (req.method === "POST" && req.path === "/v1/workspaces") {
      calls.push({ headers: req.headers, rawBody: req.rawBody ?? "" });
      if (failFirst) {
        const resp = failFirst;
        failFirst = null;
        return {
          status: resp.status,
          headers: { "content-type": "application/json" },
          body: resp.body,
        };
      }
    }
    return origHandle(req);
  };
  return calls;
}

async function fillForm(templateId: string) {
  const nameInput = await screen.findByLabelText(/Name/);
  fireEvent.change(nameInput, { target: { value: "my-box" } });
  const select = await screen.findByLabelText(/Template/);
  await waitFor(() => expect(select.querySelectorAll("option").length).toBeGreaterThan(1));
  fireEvent.change(select, { target: { value: templateId } });
}

describe("CreateWorkspacePage", () => {
  it("create: idempotency key is reused when retrying a failed submit", async () => {
    const api = createMockApi();
    loginCookies();
    const calls = interceptCreates(api, {
      status: 500,
      body: { code: "INTERNAL", message: "boom", retryable: true, requestId: "r1" },
        });
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm(TEMPLATE_LINUX.id);

    const submit = screen.getByRole("button", { name: "Create workspace" });
    fireEvent.click(submit);
    await screen.findByRole("alert");

    fireEvent.click(await screen.findByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(calls).toHaveLength(2));

    expect(calls[0]!.headers["idempotency-key"]).toBeTruthy();
    expect(calls[1]!.headers["idempotency-key"]).toBe(calls[0]!.headers["idempotency-key"]);
    expect(calls[1]!.rawBody).toBe(calls[0]!.rawBody);
  }, 20000);

  it("create: ephemeral warning appears before submit", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm(TEMPLATE_BROWSER.id); // dataPolicyDefault: Ephemeral

    const notice = await screen.findByText("Data is not kept");
    expect(notice).toBeVisible();
    expect(screen.getByText(/destroys the disk/)).toBeVisible();
    // Shown before submit: no create request has gone out yet.
    expect(
      api.state.requests.filter((r) => r.method === "POST" && r.path === "/v1/workspaces"),
    ).toHaveLength(0);
  }, 20000);

  it("create: no ephemeral warning for a Retain template", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm(TEMPLATE_LINUX.id); // dataPolicyDefault: Retain

    await waitFor(() => expect(screen.getByLabelText(/Template/)).toHaveValue(TEMPLATE_LINUX.id));
    expect(screen.queryByText("Data is not kept")).not.toBeInTheDocument();
  }, 20000);

  it("create: successful create navigates to the new workspace", async () => {
    const api = createMockApi();
    loginCookies();
    const calls = interceptCreates(api, null);
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm(TEMPLATE_LINUX.id);

    fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(calls).toHaveLength(1));
    await waitFor(() => expect(window.location.pathname).toMatch(/^\/workspaces\/ws_/));
  }, 20000);

  // FX-R17: the two quota refusals read differently — real exhaustion tells
  // the user to free resources; a tenant with no quota at all tells them to
  // ask an administrator to set one.
  it.each([
    [
      "QUOTA_EXHAUSTED",
      "Quota exhausted — delete an unused workspace or ask an administrator for more quota.",
    ],
    [
      "QUOTA_NOT_CONFIGURED",
      "No quota is configured for your tenant. Ask an administrator to set one.",
    ],
  ])("create: %s shows its own guidance", async (code, message) => {
    const api = createMockApi();
    loginCookies();
    interceptCreates(api, {
      status: 409,
      body: { code, message: "server detail", retryable: false, requestId: "r-quota" },
    });
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm(TEMPLATE_LINUX.id);

    fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(code);
    expect(alert).toHaveTextContent(message);
    // Neither refusal is retryable: no Retry button.
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  }, 20000);
});
