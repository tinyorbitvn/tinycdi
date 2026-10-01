import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { CreateWorkspacePage } from "../../src/workspaces/CreateWorkspacePage";
import {
  createMockApi,
  renderWithApi,
  loginCookies,
} from "./helpers";
import { TEMPLATE_LINUX } from "../mock-api/fixtures.ts";

async function fillForm(templateId = TEMPLATE_LINUX.id) {
  const nameInput = await screen.findByLabelText(/Name/);
  fireEvent.change(nameInput, { target: { value: "my-box" } });
  const select = await screen.findByLabelText(/Template/);
  await waitFor(() =>
    expect(select.querySelectorAll("option").length).toBeGreaterThan(1),
  );
  fireEvent.change(select, { target: { value: templateId } });
  return { select };
}

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

describe("CreateWorkspacePage", () => {
  it("reuses the same Idempotency-Key when retrying a failed create", async () => {
    const api = createMockApi();
    loginCookies();
    const calls = interceptCreates(api, {
      status: 500,
      body: { code: "INTERNAL", message: "boom", retryable: true, requestId: "r1" },
    });
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm();

    fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await screen.findByRole("alert");
    fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(calls).toHaveLength(2));

    expect(calls[0].headers["idempotency-key"]).toBeTruthy();
    expect(calls[1].headers["idempotency-key"]).toBe(
      calls[0].headers["idempotency-key"],
    );
  });

  it("mints a fresh Idempotency-Key after IDEMPOTENCY_CONFLICT", async () => {
    const api = createMockApi();
    loginCookies();
    const calls = interceptCreates(api, {
      status: 409,
      body: {
        code: "IDEMPOTENCY_CONFLICT",
        message: "key reused",
        retryable: false,
        requestId: "r2",
      },
    });
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm();

    fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await screen.findByText("IDEMPOTENCY_CONFLICT");
    fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    await waitFor(() => expect(calls).toHaveLength(2));

    expect(calls[1].headers["idempotency-key"]).toBeTruthy();
    expect(calls[1].headers["idempotency-key"]).not.toBe(
      calls[0].headers["idempotency-key"],
    );
  });

  it("shows the QUOTA_EXHAUSTED code and guidance", async () => {
    const api = createMockApi();
    api.state.quotaExhausted = true;
    loginCookies();
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm();
    fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    const banner = await screen.findByRole("alert");
    expect(banner).toHaveTextContent("QUOTA_EXHAUSTED");
    expect(banner).toHaveTextContent("Quota exhausted");
  });

  it("shows INVALID_TEMPLATE for a rejected templateRef", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<CreateWorkspacePage />, api);
    await fillForm();
    api.state.invalidTemplates.add(TEMPLATE_LINUX.id);
    fireEvent.click(screen.getByRole("button", { name: "Create workspace" }));
    const banner = await screen.findByRole("alert");
    expect(banner).toHaveTextContent("INVALID_TEMPLATE");
  });
});
