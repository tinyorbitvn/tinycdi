import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { DataListPage } from "../../../src/data/DataListPage";
import { DataDetailPage } from "../../../src/data/DataDetailPage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { RETAINED_DISK } from "../../mock-api/fixtures.ts";

function retainedRow(): HTMLElement {
  const table = screen.getByRole("table", { name: /retained/i });
  return within(table)
    .getAllByRole("row")
    .find((r) => r.textContent?.includes(RETAINED_DISK.id))!;
}

describe("data attach", () => {
  it("posts to /v1/data/{id}/attach with an idempotency key and navigates to the new workspace", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataListPage />, api);

    await screen.findByRole("table", { name: /retained/i });
    fireEvent.click(within(retainedRow()).getByRole("button", { name: /^attach$/i }));

    const dialog = await screen.findByRole("dialog");
    const nameInput = within(dialog).getByLabelText(/workspace name/i);
    fireEvent.change(nameInput, { target: { value: "restored-desktop" } });

    fireEvent.click(
      within(dialog).getByRole("button", { name: /attach disk/i }),
    );

    await waitFor(() =>
      expect(window.location.pathname).toMatch(/^\/workspaces\/ws_/),
    );

    const attachReq = api.state.requests.find((r) =>
      r.path.endsWith(`/data/${RETAINED_DISK.id}/attach`),
    );
    expect(attachReq).toBeTruthy();
    expect(attachReq!.headers["idempotency-key"]).toBeTruthy();
    const body = JSON.parse(attachReq!.rawBody);
    expect(body.name).toBe("restored-desktop");
    expect(body.templateRef).toMatch(/^tpl_/);

    // The record is now Attaching, bound to the new workspace.
    const rec = api.state.retained.get(RETAINED_DISK.id)!;
    expect(rec.state).toBe("Attaching");
    expect(rec.consumingWorkspaceId).toBe(
      window.location.pathname.split("/").pop(),
    );
  });

  it("reuses the same Idempotency-Key when the submit is retried", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataListPage />, api);

    await screen.findByRole("table", { name: /retained/i });
    fireEvent.click(within(retainedRow()).getByRole("button", { name: /^attach$/i }));
    const dialog = await screen.findByRole("dialog");

    // First attempt loses a simulated race: the record is no longer
    // Retained, so the mock answers 409 INVALID_STATE.
    api.state.retained.get(RETAINED_DISK.id)!.state = "Attaching";
    fireEvent.click(within(dialog).getByRole("button", { name: /attach disk/i }));
    await screen.findByRole("alert");

    api.state.retained.get(RETAINED_DISK.id)!.state = "Retained";
    fireEvent.click(within(dialog).getByRole("button", { name: /attach disk/i }));

    await waitFor(() =>
      expect(window.location.pathname).toMatch(/^\/workspaces\/ws_/),
    );
    const attachReqs = api.state.requests.filter((r) =>
      r.path.endsWith(`/data/${RETAINED_DISK.id}/attach`),
    );
    expect(attachReqs).toHaveLength(2);
    expect(attachReqs[0]!.headers["idempotency-key"]).toBe(
      attachReqs[1]!.headers["idempotency-key"],
    );
  });

  it("attaches from the detail page and links the consuming workspace", async () => {
    const api = createMockApi();
    const rec = api.state.retained.get(RETAINED_DISK.id)!;
    rec.state = "Attached";
    rec.consumingWorkspaceId = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";
    loginCookies();
    renderWithApi(<DataDetailPage dataId={RETAINED_DISK.id} />, api);

    const link = await screen.findByRole("link", {
      name: "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E",
    });
    expect(link).toHaveAttribute(
      "href",
      "/workspaces/ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E",
    );
    // An attached disk offers no attach/purge actions.
    expect(
      screen.queryByRole("button", { name: /^attach$/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /^purge$/i }),
    ).not.toBeInTheDocument();
  });
});
