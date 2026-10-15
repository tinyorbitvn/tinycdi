import { describe, expect, it, vi } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, render, screen, within } from "@testing-library/react";
import { LifecycleProgress } from "../../../src/progress/LifecycleProgress";
import { makeWorkspace, type ConditionFixture } from "../../mock-api/fixtures.ts";
import type { WorkspaceView } from "../../../src/progress/derive";

const T0 = "2026-10-03T10:00:00Z";

function cond(
  type: ConditionFixture["type"],
  status: ConditionFixture["status"],
  reason: string,
): ConditionFixture {
  return { type, status, reason, lastTransitionTime: T0 };
}

function provisioning(reason: string, over: Partial<WorkspaceView> = {}): WorkspaceView {
  return makeWorkspace({
    name: "research-desktop",
    phase: "Provisioning",
    desiredState: "Running",
    createdAt: "2026-10-03T09:50:00Z",
    updatedAt: T0,
    conditions: [
      cond("Admitted", "True", "QuotaReserved"),
      cond("StorageReady", "True", "Ready"),
      cond("RuntimeReady", "False", reason),
    ],
    ...over,
  });
}

describe("LifecycleProgress", () => {
  it("renders a labelled region with ordered steps and shape+text marks", async () => {
    render(<LifecycleProgress workspace={provisioning("Unschedulable")} />);
    const region = await screen.findByRole("region", { name: "Starting research-desktop" });
    const steps = within(region).getAllByRole("listitem");
    expect(steps).toHaveLength(5); // Retain: accepted, disk, machine, desktop, connect
    // Marks are text characters, never colour alone.
    expect(within(steps[0]).getByText("✓")).toBeInTheDocument();
    expect(within(steps[2]).getByText("●")).toBeInTheDocument();
    expect(within(steps[3]).getByText("○")).toBeInTheDocument();
    // The active step carries aria-current and the slow reason copy.
    const active = steps.find((li) => li.getAttribute("aria-current") === "step");
    expect(active).toHaveTextContent("Finding a machine");
    expect(active).toHaveTextContent("No machine has room");
    expect(active).toHaveTextContent(/for 0:00/); // client-observed elapsed, aria-hidden
    expect(steps[0]).toHaveTextContent("done");
    expect(steps[3]).toHaveTextContent("waiting");
  });

  it("announces via one polite region that changes only on step change", async () => {
    const ws = provisioning("Provisioning");
    const { rerender } = render(<LifecycleProgress workspace={ws} />);
    const status = (await screen.findAllByRole("status")).find(
      (el) => el.textContent?.includes("step"),
    )!;
    const first = status.textContent;
    expect(first).toContain("step 3 of 5");
    expect(first).toContain("Finding a machine");

    // A re-render with identical view data must not re-announce.
    rerender(<LifecycleProgress workspace={provisioning("Provisioning")} />);
    expect(status.textContent).toBe(first);

    // A new step re-announces.
    rerender(<LifecycleProgress workspace={provisioning("PullingImage")} />);
    await screen.findByText(/step 4 of 5/);
    expect(status.textContent).toContain("Downloading the desktop image");
  });

  it("compact variant is one line with mark, title and step position", async () => {
    render(<LifecycleProgress workspace={provisioning("Provisioning")} variant="compact" />);
    const text = await screen.findByText(/step 3 of 5/, { selector: ".tc-progress__compactline" });
    expect(text).toHaveTextContent("Starting research-desktop");
    expect(text).toHaveTextContent("Finding a machine");
  });

  it("failed op marks the stalled step and raises an alert", async () => {
    const ws = provisioning("BootDeadlineExceeded", {
      phase: "Failed",
      failureReason: "BootDeadlineExceeded",
      conditions: [
        cond("StorageReady", "True", "Ready"),
        cond("RuntimeReady", "False", "BootDeadlineExceeded"),
        cond("ConnectionReady", "False", "ErrImagePull"),
      ],
    });
    render(<LifecycleProgress workspace={ws} />);
    const alert = await screen.findAllByRole("alert");
    expect(alert.some((a) => a.textContent?.includes("couldn't be downloaded"))).toBe(true);
    const failed = screen.getAllByRole("listitem").find((li) => li.dataset.state === "failed");
    expect(failed).toHaveTextContent("Downloading the desktop image");
    expect(within(failed!).getByText("!")).toBeInTheDocument();
  });

  it("shows 'Status delayed' for a stale observed view", async () => {
    const ws = provisioning("Provisioning", {
      conditions: [
        ...provisioning("Provisioning").conditions,
        cond("Degraded", "True", "StatusStale"),
      ],
    });
    render(<LifecycleProgress workspace={ws} />);
    expect(await screen.findByText(/Status delayed/)).toBeInTheDocument();
  });

  it("shows the stall note once the operation is older than the threshold", async () => {
    const ws = provisioning("Provisioning", {
      updatedAt: "2026-10-03T09:40:00Z",
    });
    render(<LifecycleProgress workspace={ws} />);
    expect(await screen.findByText(/Still waiting/)).toBeInTheDocument();
  });

  it("a refresh error is polite at first and gains a Retry after 3 failures", async () => {
    const ws = provisioning("Provisioning");
    const onRetry = vi.fn();
    const { rerender } = render(
      <LifecycleProgress workspace={ws} refreshError={new Error("e1")} onRetry={onRetry} />,
    );
    expect(await screen.findByText(/Can't refresh progress/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    rerender(<LifecycleProgress workspace={ws} refreshError={new Error("e2")} onRetry={onRetry} />);
    rerender(<LifecycleProgress workspace={ws} refreshError={new Error("e3")} onRetry={onRetry} />);
    const retry = await screen.findByRole("button", { name: "Retry" });
    retry.click();
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("steps a delete through its teardown states", async () => {
    const ws = makeWorkspace({
      name: "old-desktop",
      phase: "Terminating",
      desiredState: "Stopped",
      conditions: [cond("RuntimeReady", "False", "DrainingStreams")],
    });
    render(<LifecycleProgress workspace={ws} />);
    const region = await screen.findByRole("region", { name: "Deleting old-desktop" });
    const active = within(region)
      .getAllByRole("listitem")
      .find((li) => li.getAttribute("aria-current") === "step");
    expect(active).toHaveTextContent("Closing sessions");
  });

  it("reduced motion renders the active mark statically", () => {
    const css = readFileSync(join(process.cwd(), "src/progress/progress.css"), "utf8");
    expect(css).toContain("prefers-reduced-motion: reduce");
    expect(css).toContain("animation: none");
  });

  it("the elapsed counter ticks without touching the announcement", async () => {
    vi.useFakeTimers();
    try {
      const ws = provisioning("Provisioning");
      render(<LifecycleProgress workspace={ws} />);
      const status = screen
        .getAllByRole("status")
        .find((el) => el.textContent?.includes("step"))!;
      const before = status.textContent;
      await act(async () => {
        vi.advanceTimersByTime(5_000);
      });
      // The tick re-renders (elapsed text in the DOM) but the polite region
      // still carries the same announcement.
      expect(status.textContent).toBe(before);
      expect(screen.getByText(/for 0:0[0-9]/)).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});
