import type { ReactNode } from "react";
import { cx } from "./cx";

export type Tone = "neutral" | "info" | "success" | "warning" | "danger" | "accent";

export interface BadgeProps {
  tone?: Tone;
  /** Leading dot in the tone color. */
  dot?: boolean;
  /** Animate the dot (transitional states). Disabled under reduced motion. */
  pulse?: boolean;
  icon?: ReactNode;
  className?: string;
  children: ReactNode;
}

export function Badge({ tone = "neutral", dot = false, pulse = false, icon, className, children }: BadgeProps) {
  return (
    <span className={cx("tc-badge", `tc-badge--${tone}`, className)}>
      {dot ? <span className={cx("tc-badge__dot", pulse && "tc-badge__dot--pulse")} aria-hidden="true" /> : null}
      {icon}
      <span>{children}</span>
    </span>
  );
}

/** Workspace lifecycle phases (mirrors WorkspacePhase in the OpenAPI contract). */
export type WorkspacePhase =
  | "Pending"
  | "Provisioning"
  | "Ready"
  | "Stopping"
  | "Stopped"
  | "Failed"
  | "Terminating";

const PHASE_META: Record<WorkspacePhase, { tone: Tone; label: string; transitional: boolean }> = {
  Pending: { tone: "info", label: "Pending", transitional: true },
  Provisioning: { tone: "info", label: "Starting", transitional: true },
  Ready: { tone: "success", label: "Ready", transitional: false },
  Stopping: { tone: "warning", label: "Stopping", transitional: true },
  Stopped: { tone: "neutral", label: "Stopped", transitional: false },
  Failed: { tone: "danger", label: "Failed", transitional: false },
  Terminating: { tone: "warning", label: "Deleting", transitional: true },
};

export function phaseTone(phase: WorkspacePhase): Tone {
  return PHASE_META[phase]?.tone ?? "neutral";
}

export function isTransitionalPhase(phase: WorkspacePhase): boolean {
  return PHASE_META[phase]?.transitional ?? false;
}

export interface StatusPillProps {
  phase: WorkspacePhase;
  /** Override the human label (defaults: Provisioning→"Starting", Terminating→"Deleting"). */
  label?: string;
  className?: string;
}

/** Workspace phase pill: tone + dot (pulsing while transitional) + label. */
export function StatusPill({ phase, label, className }: StatusPillProps) {
  const meta = PHASE_META[phase] ?? { tone: "neutral" as Tone, label: phase, transitional: false };
  return (
    <Badge
      tone={meta.tone}
      dot
      pulse={meta.transitional}
      className={cx("tc-status-pill", className)}
    >
      <span data-phase={phase}>{label ?? meta.label}</span>
    </Badge>
  );
}
