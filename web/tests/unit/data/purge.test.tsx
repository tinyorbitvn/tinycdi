import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { DataListPage } from "../../../src/data/DataListPage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { RETAINED_DISK } from "../../mock-api/fixtures.ts";

async function openPurge() {
  const table = await screen.findByRole("table", { name: /retained/i });
  const link = await within(table).findByRole("link", {
    name: new RegExp(RETAINED_DISK.id),
  });
  const row = link.closest("tr")!;
  fireEvent.click(within(row).getByRole("button", { name: /^purge$/i }));
  return screen.findByRole("alertdialog");
}

describe("data purge", () => {
  it("keeps the confirm button disabled until the data ID is typed", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataListPage />, api);

    const dialog = await openPurge();
    const confirm = within(dialog).getByRole("button", {
      name: /purge permanently/i,
    });
    expect(confirm).toBeDisabled();

    const input = within(dialog).getByLabelText(/confirm/i);
    fireEvent.change(input, { target: { value: "rd_wrong" } });
    await waitFor(() => expect(confirm).toBeDisabled());

    fireEvent.change(input, { target: { value: RETAINED_DISK.id } });
    await waitFor(() => expect(confirm).toBeEnabled());
  });

  it("purges with the fresh nonce and reports Purging", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataListPage />, api);

    const dialog = await openPurge();
    const input = within(dialog).getByLabelText(/confirm/i);
    fireEvent.change(input, { target: { value: RETAINED_DISK.id } });
    const confirm = within(dialog).getByRole("button", {
      name: /purge permanently/i,
    });
    await waitFor(() => expect(confirm).toBeEnabled());
    fireEvent.click(confirm);

    await waitFor(() =>
      expect(api.state.retained.get(RETAINED_DISK.id)?.state).toBe("Purging"),
    );
    const req = api.state.requests.find((r) => r.path.endsWith(":purge"));
    expect(JSON.parse(req!.rawBody).confirmationNonce).toMatch(/^nonce-/);
    await waitFor(() =>
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument(),
    );
  });

  it("a 409 shows the being-attached message", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataListPage />, api);

    const dialog = await openPurge();
    const input = within(dialog).getByLabelText(/confirm/i);
    fireEvent.change(input, { target: { value: RETAINED_DISK.id } });
    const confirm = within(dialog).getByRole("button", {
      name: /purge permanently/i,
    });
    await waitFor(() => expect(confirm).toBeEnabled());

    // A concurrent attach won the race before our purge landed.
    api.state.retained.get(RETAINED_DISK.id)!.state = "Attaching";
    fireEvent.click(confirm);

    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent(
      "This data is being attached. Try again after it finishes.",
    );
    expect(api.state.retained.get(RETAINED_DISK.id)?.state).toBe("Attaching");
    expect(dialog).toBeInTheDocument();
  });

  it("the purge dialog body does not repeat the id from its title", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataListPage />, api);
    const dialog = await openPurge();
    expect(
      within(dialog).getByText("20 GiB from old-desktop. Purging destroys the disk permanently. This cannot be undone."),
    ).toBeInTheDocument();
  });

  it("an unmapped error code leads with stable guidance, not the raw server message", async () => {
    const api = createMockApi();
    const handle = api.handle.bind(api);
    api.handle = (req) =>
      req.path.endsWith(":purge")
        ? {
            status: 500,
            headers: { "content-type": "application/json" },
            body: { error: { code: "BRAND_NEW_CODE", message: "raw server text", retryable: true, requestId: "req_x" } },
          }
        : handle(req);
    loginCookies();
    renderWithApi(<DataListPage />, api);

    const dialog = await openPurge();
    fireEvent.change(within(dialog).getByLabelText(/confirm/i), {
      target: { value: RETAINED_DISK.id },
    });
    const confirm = within(dialog).getByRole("button", { name: /purge permanently/i });
    await waitFor(() => expect(confirm).toBeEnabled());
    fireEvent.click(confirm);

    const alert = await within(dialog).findByRole("alert");
    const title = alert.querySelector(".tc-alert__title");
    expect(title).toHaveTextContent(
      "The service could not complete the request; it is safe to retry.",
    );
    expect(title).not.toHaveTextContent("raw server text");
    // The server message stays visible as secondary detail.
    expect(alert).toHaveTextContent("raw server text");
  });
});
