// FX-R29 — the retained-data views follow a purge to completion: the
// list polls while a row is Purging and drops it when the record turns
// Purged (the API excludes Purged records); the detail page shows the
// Purged state once the record stops being returned.
import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { DataDetailPage } from "../../../src/data/DataDetailPage";
import { DataListPage } from "../../../src/data/DataListPage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { RETAINED_DISK } from "../../mock-api/fixtures.ts";

const listGets = (api: ReturnType<typeof createMockApi>): number =>
  api.state.requests.filter((r) => r.method === "GET" && r.path === "/v1/data").length;

async function confirmPurge() {
  const table = await screen.findByRole("table", { name: /retained/i });
  const row = within(table)
    .getAllByRole("row")
    .find((r) => r.textContent?.includes(RETAINED_DISK.id))!;
  fireEvent.click(within(row).getByRole("button", { name: /^purge$/i }));
  const dialog = await screen.findByRole("alertdialog");
  fireEvent.change(within(dialog).getByLabelText(/confirm/i), {
    target: { value: RETAINED_DISK.id },
  });
  const confirm = within(dialog).getByRole("button", { name: /purge permanently/i });
  await waitFor(() => expect(confirm).toBeEnabled());
  fireEvent.click(confirm);
  await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument());
}

describe("retained-data purge follow-through (FX-R29)", () => {
  it("starts polling after a purge is scheduled and drops the row on Purged", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataListPage pollIntervalMs={50} />, api);

    await confirmPurge();

    // No manual reload: the row flips to Purging on the post-purge
    // refresh and the list keeps polling while it stays there.
    await screen.findByText("Purging");
    const seen = listGets(api);
    await waitFor(() => expect(listGets(api)).toBeGreaterThan(seen));

    // The sweep completes: the record is Purged and excluded by the API,
    // so the row drops out and the poll stops.
    api.state.retained.get(RETAINED_DISK.id)!.state = "Purged";
    await waitFor(() =>
      expect(screen.queryByText(RETAINED_DISK.id)).not.toBeInTheDocument(),
    );
    const settled = listGets(api);
    await new Promise((r) => setTimeout(r, 300));
    expect(listGets(api)).toBe(settled);
  });

  it("polls a row that was already Purging when the page opened", async () => {
    const api = createMockApi();
    api.state.retained.get(RETAINED_DISK.id)!.state = "Purging";
    loginCookies();
    renderWithApi(<DataListPage pollIntervalMs={50} />, api);

    const table = await screen.findByRole("table", { name: /retained/i });
    await within(table).findByText("Purging");
    const seen = listGets(api);
    await waitFor(() => expect(listGets(api)).toBeGreaterThan(seen));
  });

  it("shows Purged with no actions once the record is gone (detail)", async () => {
    const api = createMockApi();
    api.state.retained.get(RETAINED_DISK.id)!.state = "Purging";
    loginCookies();
    renderWithApi(
      <DataDetailPage dataId={RETAINED_DISK.id} pollIntervalMs={50} />,
      api,
    );

    await screen.findByText("Purging");

    // The sweep completes: the backend stops returning the record — a
    // Purged record is 404 on the single-get endpoint.
    api.state.retained.delete(RETAINED_DISK.id);

    await screen.findByText("Purged");
    expect(screen.queryByRole("button", { name: /^attach$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^purge$/i })).not.toBeInTheDocument();
  });
});
