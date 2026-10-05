import { afterEach, describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import {
  ReasonText,
  REASON_TOKENS,
  formatReasonMessage,
  formatReasonParam,
  reasonMessageKey,
  reasonParamFormats,
  reasonText,
} from "../../../src/workspaces/reasons";
import { en } from "../../../src/i18n/en";
import { vi as viCatalog } from "../../../src/i18n/vi";
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
    expect(within(table).getByText("Capacity reserved: 4 vCPU, 8 GiB memory, 20 GiB storage.")).toBeInTheDocument();
    expect(within(table).getByText("The runtime is up.")).toBeInTheDocument();
  }, 20000);
});

// ---------------------------------------------------------------------------
// B3-PARAMS: structured params localize the interpolated values too.
// ---------------------------------------------------------------------------

describe("formatReasonMessage params", () => {
  it("interpolates token params through their catalog text", () => {
    expect(
      formatReasonMessage("TemplateUpdateSkipped", {
        revision: "7",
        skipReason: "runtime-changed",
      }),
    ).toBe("The workspace stayed on its recorded template revision 7 (runtime changed).");
    expect(formatReasonMessage("CleanupRetry", { step: "drain-streams" })).toBe(
      "Teardown step stream draining is blocked; retrying.",
    );
    expect(formatReasonMessage("DrainTimedOut", { budgetSeconds: "45" })).toBe(
      "Open sessions did not close inside the 45 s budget; teardown continues.",
    );
  });

  it("formats sizes and counts for the active locale", () => {
    expect(
      formatReasonMessage("QuotaReserved", {
        cpuMillicores: "8000",
        memoryMiB: "16384",
        storageGiB: "50",
      }),
    ).toBe("Capacity reserved: 8 vCPU, 16 GiB memory, 50 GiB storage.");
    setActiveLocale("vi");
    expect(
      formatReasonMessage("QuotaReserved", {
        cpuMillicores: "8000",
        memoryMiB: "16384",
        storageGiB: "50",
      }),
    ).toBe("Đã dành sẵn tài nguyên: 8 vCPU, 16 GiB bộ nhớ, 50 GiB đĩa.");
    expect(formatReasonMessage("CleanupRetry", { step: "drain-streams" })).toBe(
      "Bước gỡ bỏ dọn stream bị chặn; đang thử lại.",
    );
  });

  it("renders an unknown param value raw instead of failing", () => {
    expect(formatReasonMessage("CleanupRetry", { step: "mystery" })).toBe(
      "Teardown step mystery is blocked; retrying.",
    );
  });

  it("falls back to the server message when a placeholder param is missing", () => {
    const detail = "teardown step cleanup blocked; retrying — detail in the operator logs";
    expect(formatReasonMessage("CleanupRetry", { condition: "Degraded" }, detail)).toBe(detail);
    expect(formatReasonMessage("CleanupRetry", undefined, detail)).toBe(detail);
    // No server message: the raw token is the last-resort fallback.
    expect(formatReasonMessage("CleanupRetry", {})).toBe("CleanupRetry");
  });

  it("ignores params a template does not ask for", () => {
    expect(formatReasonMessage("Stopped", { step: "x" })).toBe("The workspace is stopped.");
  });
});

describe("formatReasonParam", () => {
  it("formats each kind, raw on unparseable", () => {
    expect(formatReasonParam("int", "12345")).toBe("12,345");
    expect(formatReasonParam("int", "abc")).toBe("abc");
    expect(formatReasonParam("durationSeconds", "45")).toBe("45 s");
    expect(formatReasonParam("durationSeconds", "3600")).toBe("1 h");
    expect(formatReasonParam("cpu", "8000")).toBe("8 vCPU");
    expect(formatReasonParam("cpu", "500")).toBe("0.5 vCPU");
    expect(formatReasonParam("mib", "16384")).toBe("16 GiB");
    expect(formatReasonParam("mib", "512")).toBe("512 MiB");
    expect(formatReasonParam("gib", "50")).toBe("50 GiB");
    expect(formatReasonParam("step", "drain-streams")).toBe("stream draining");
    expect(formatReasonParam("skipReason", "storage-smaller")).toBe("smaller storage");
    expect(formatReasonParam("raw", "<img>")).toBe("<img>");
  });
});

describe("params escaping", () => {
  it("substitutes values as text — markup in a param is not interpreted", () => {
    const evil = '<img src=x onerror=alert(1)>';
    const { container } = render(
      <ReasonText
        reason="TemplateUpdateSkipped"
        detail="stayed on recorded revision"
        params={{ revision: "7", skipReason: evil }}
      />,
    );
    expect(container.querySelector("img")).toBeNull();
    expect(container).toHaveTextContent(evil);
  });
});

// Catalog completeness (B3-PARAMS): every reason token has an en + vi
// template whose {placeholders} all resolve to a declared param for that
// token — a template can never ask for a value no emitter produces.
describe("reason catalog completeness", () => {
  const PLACEHOLDER = /\{([^{}]+)\}/g;
  const placeholders = (s: string) => [...s.matchAll(PLACEHOLDER)].map((m) => m[1]);

  it("every known token has en + vi templates with declared placeholders", () => {
    for (const token of REASON_TOKENS) {
      const key = reasonMessageKey(token);
      expect(key, token).toBeDefined();
      const declared = reasonParamFormats(token);
      for (const [locale, catalog] of [
        ["en", en],
        ["vi", viCatalog],
      ] as const) {
        const template = catalog[key!];
        expect(template, `${locale} ${token}`).toBeDefined();
        for (const name of placeholders(template)) {
          expect(declared[name], `${locale} ${token} placeholder {${name}}`).toBeDefined();
        }
      }
    }
  });
});
