import { describe, expect, it } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { FileTransfer } from "../../../src/session/FileTransfer";
import { createMockApi } from "../../mock-api/handler.ts";
import { renderWithApi } from "../helpers";

const WS = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";

function mounted(policy: "Disabled" | "Upload" | "Download" | "Bidirectional", maxFileBytes = 1024) {
  const api = createMockApi();
  const { unmount } = renderWithApi(
    <FileTransfer workspaceId={WS} sessionDomain="session.example.test" policy={policy} maxFileBytes={maxFileBytes} />,
    api,
  );
  return { api, unmount };
}

const mints = (api: ReturnType<typeof createMockApi>) => api.state.requests.filter((r) => r.path.endsWith("/file-transfers"));

describe("FileTransfer", () => {
  it("renders nothing when the resolved policy is Disabled", () => {
    mounted("Disabled");
    expect(screen.queryByRole("region", { name: "File transfer" })).toBeNull();
  });

  it("offers only the allowed directions", () => {
    const up = mounted("Upload");
    expect(screen.getByLabelText("Choose a file to upload")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
    up.unmount();
    const down = mounted("Download");
    expect(screen.queryByLabelText("Choose a file to upload")).toBeNull();
    expect(screen.getByRole("button", { name: "Download" })).toBeTruthy();
    down.unmount();
    mounted("Bidirectional");
    expect(screen.getByLabelText("Choose a file to upload")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Download" })).toBeTruthy();
  });

  it("refuses an over-limit file before minting anything", async () => {
    const { api } = mounted("Upload", 4);
    fireEvent.change(screen.getByLabelText("Choose a file to upload"), {
      target: { files: [new File(["12345"], "big.bin")] },
    });
    expect(await screen.findByText("The file is larger than the transfer limit.")).toBeTruthy();
    expect(mints(api)).toEqual([]);
  });
});
