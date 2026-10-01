import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { DataPage } from "../../src/workspaces/DataPage";
import {
  createMockApi,
  renderWithApi,
  loginCookies,
  SESSION_COOKIE,
  SESSION_PRINCIPAL,
  CSRF_COOKIE,
  CSRF_TOKEN_VALUE,
} from "./helpers";
import { RETAINED_DISK } from "../mock-api/fixtures.ts";

describe("Purge confirmation", () => {
  it("requires typing the source workspace name and sends the fresh nonce", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataPage />, api);

    await screen.findByRole("table", { name: "retained data" });
    fireEvent.click(screen.getByRole("button", { name: "Purge" }));

    const dialog = await screen.findByRole("dialog");
    const confirmBtn = screen.getByRole("button", { name: "Purge permanently" });
    expect(confirmBtn).toBeDisabled();

    const input = screen.getByLabelText("Confirmation");
    fireEvent.change(input, { target: { value: "not-the-name" } });
    expect(confirmBtn).toBeDisabled();
    fireEvent.change(input, { target: { value: "old-desktop" } });
    await waitFor(() => expect(confirmBtn).toBeEnabled());

    fireEvent.click(confirmBtn);
    await waitFor(() =>
      expect(api.state.retained.get(RETAINED_DISK.id)?.state).toBe("Purging"),
    );
    const purgeReq = api.state.requests.find((r) =>
      r.path.endsWith("/purge"),
    );
    expect(purgeReq).toBeTruthy();
    expect(JSON.parse(purgeReq!.rawBody).confirmationNonce).toMatch(/^nonce-/);
    expect(dialog.textContent ?? "").not.toContain("Purge permanently");
  });

  it("rejects a stale nonce and keeps the record Retained", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataPage />, api);
    await screen.findByRole("table", { name: "retained data" });
    fireEvent.click(screen.getByRole("button", { name: "Purge" }));
    const dialog = await screen.findByRole("dialog");
    const input = screen.getByLabelText("Confirmation");
    fireEvent.change(input, { target: { value: "old-desktop" } });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Purge permanently" })).toBeEnabled(),
    );

    // Another read of the record mints a fresh nonce, invalidating the one
    // the dialog holds — the purge must fail with INVALID_REQUEST.
    api.handle({
      method: "GET",
      path: "/v1/data",
      query: new URLSearchParams(),
      headers: {
        cookie: `${SESSION_COOKIE}=${SESSION_PRINCIPAL}; ${CSRF_COOKIE}=${CSRF_TOKEN_VALUE}`,
      },
    });

    fireEvent.click(screen.getByRole("button", { name: "Purge permanently" }));
    const banner = await screen.findByRole("alert");
    expect(banner).toHaveTextContent("INVALID_REQUEST");
    expect(api.state.retained.get(RETAINED_DISK.id)?.state).toBe("Retained");
    expect(dialog).toBeInTheDocument();
  });
});
