import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { DataDetailPage } from "../../../src/data/DataDetailPage";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { RETAINED_DISK } from "../../mock-api/fixtures.ts";

describe("DataDetailPage", () => {
  it("titles the page with the source workspace and copies the full id", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    try {
      const api = createMockApi();
      loginCookies();
      renderWithApi(<DataDetailPage dataId={RETAINED_DISK.id} pollIntervalMs={60_000} />, api);
      expect(await screen.findByRole("heading", { level: 1, name: "old-desktop" })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: "Back to retained data" })).toHaveClass("tc-backlink");
      expect(screen.getByText(RETAINED_DISK.id, { selector: "code" })).toBeInTheDocument();
      fireEvent.click(screen.getByRole("button", { name: "Copy ID" }));
      await waitFor(() => expect(writeText).toHaveBeenCalledWith(RETAINED_DISK.id));
      expect(await screen.findByText("ID copied")).toBeInTheDocument();
    } finally {
      delete (navigator as { clipboard?: unknown }).clipboard;
    }
  });

  it("an unknown disk is titled with the shortened id", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataDetailPage dataId="rd_01J4ZZZZZZZZZZZZZZZZZZZZ0B" pollIntervalMs={60_000} />, api);
    expect(
      await screen.findByRole("heading", { level: 1, name: "Retained disk rd_01J4…0B" }),
    ).toBeInTheDocument();
  });
});
