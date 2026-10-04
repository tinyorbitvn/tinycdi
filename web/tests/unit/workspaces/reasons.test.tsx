import { afterEach, describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import {
  ReasonText,
  reasonMessageKey,
  reasonText,
} from "../../../src/workspaces/reasons";
import { setActiveLocale } from "../../../src/i18n";
import { WorkspaceDetailPage } from "../../../src/workspaces/WorkspaceDetailPage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { readyWorkspace } from "../../mock-api/fixtures.ts";
import type { WorkspaceEventFixture } from "../../mock-api/fixtures.ts";

// Reason-token localization (V3.14b): a known reason renders catalog text
// and keeps the server message as secondary detail; an unknown token falls
// back to the server message, never dropped.

const WS_ID = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";

afterEach(() => {
  setActiveLocale("en");
});

describe("reason mapping", () => {
  it("maps every curated event and condition token", () => {
    const tokens = [
      "Created", "StartRequested", "StopRequested", "MaxDurationReached",
      "DeleteRequested", "TemplateUpdateSkipped", "TemplateResolved",
      "TemplateNotFound", "TemplateRejected", "TemplateSnapshotInvalid",
      "IntentApplied", "Provisioning", "WaitingForDisk", "Ready", "Stopped",
      "Terminating", "NameConflict", "BootDeadlineExceeded", "BackendError",
      "Nominal", "RetainedClaimMissing", "IdleTimeout", "DisconnectTimeout",
      "MaxDuration", "StreamDraining", "DrainTimedOut", "RetentionPending",
      "CleanupRetry", "FailedCleanup", "MissingWorkspaceID",
      "BlockingConnects", "RevokingLeases", "DrainingStreams",
      "StoppingRuntime", "ApplyingRetention", "CleaningUp", "Unschedulable",
      "PreparingPod", "PullingImage", "ContainerCreating", "PodInitializing",
      "NotReady", "ImagePullBackOff", "ErrImagePull", "CrashLoopBackOff",
      "CreateContainerConfigError", "PodFailed", "PodExited", "StatusStale",
      "Unknown",
    ];
    for (const token of tokens) {
      expect(reasonMessageKey(token), token).toBeDefined();
    }
  });

  it("returns undefined for unknown tokens", () => {
    expect(reasonMessageKey("BackOff")).toBeUndefined();
    expect(reasonMessageKey("")).toBeUndefined();
  });

  it("reasonText localizes known tokens, keeps unknown ones verbatim", () => {
    expect(reasonText("Stopped")).toBe("The workspace is stopped.");
    expect(reasonText("WhatTheOperator")).toBe("WhatTheOperator");
  });

  it("reasonText follows the active locale", () => {
    setActiveLocale("vi");
    expect(reasonText("Stopped")).toBe("Workspace đã dừng.");
    expect(reasonText("WhatTheOperator")).toBe("WhatTheOperator");
  });
});

describe("ReasonText", () => {
  it("renders catalog text plus the server message as secondary detail", () => {
    const { container } = render(
      <ReasonText reason="Stopped" detail="Runtime stopped; disk retained" />,
    );
    expect(container).toHaveTextContent("The workspace is stopped.");
    const hint = container.querySelector("[title]");
    expect(hint).toHaveAttribute("title", "Runtime stopped; disk retained");
    expect(hint).toHaveTextContent("Runtime stopped; disk retained");
  });

  it("suppresses detail identical to the catalog text", () => {
    const { container } = render(
      <ReasonText reason="Created" detail="The workspace was created." />,
    );
    expect(container).toHaveTextContent("The workspace was created.");
    expect(container.querySelector("[title]")).toBeNull();
  });

  it("falls back to the server message for unknown reasons", () => {
    const { container } = render(
      <ReasonText reason="BackOff" detail="kubelet backoff detail" />,
    );
    expect(container).toHaveTextContent("kubelet backoff detail");
    expect(container.querySelector("[title]")).toBeNull();
  });

  it("falls back to the raw token when there is no message", () => {
    const { container } = render(<ReasonText reason="BackOff" />);
    expect(container).toHaveTextContent("BackOff");
  });
});

describe("WorkspaceDetailPage reason localization", () => {
  it("events table shows localized text with the server message kept as detail", async () => {
    const api = createMockApi();
    const ws = readyWorkspace({ id: WS_ID });
    api.state.workspaces.set(WS_ID, ws);
    api.state.events.set(WS_ID, [
      { type: "Normal", reason: "Stopped", message: "Runtime stopped; disk retained", lastTimestamp: "2026-09-30T10:05:00Z" },
      { type: "Normal", reason: "BackOff", message: "free-form detail stays primary", lastTimestamp: "2026-09-30T10:00:00Z" },
    ] satisfies WorkspaceEventFixture[]);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    const table = await screen.findByRole("table", { name: "Events" });
    expect(within(table).getByText("The workspace is stopped.")).toBeInTheDocument();
    expect(within(table).getByText("Runtime stopped; disk retained")).toBeInTheDocument();
    expect(within(table).getByText("free-form detail stays primary")).toBeInTheDocument();
  }, 20000);

  it("conditions table shows localized text from the condition reason", async () => {
    const api = createMockApi();
    const ws = readyWorkspace({ id: WS_ID });
    api.state.workspaces.set(WS_ID, ws);
    api.state.events.set(WS_ID, []);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    const table = await screen.findByRole("table", { name: "conditions" });
    expect(within(table).getByText("Capacity was reserved for the workspace.")).toBeInTheDocument();
    expect(within(table).getByText("The runtime is up.")).toBeInTheDocument();
  }, 20000);
});
