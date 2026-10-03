import { cx, useDomId, VisuallyHidden } from "../design";
import { t, type MessageKey } from "../i18n";
import {
  formatElapsed,
  type ProgressModel,
  type ProgressStep,
  type WorkspaceView,
} from "./derive";
import { useProgress } from "./useProgress";
import "./progress.css";

// The lifecycle step panel (V3.27 PR-B): one component, three placements.
// Compact is a one-line summary inside a list row; full is the bordered
// panel on the detail page; overlay is the same panel centred on the
// session stage. Marks are shape + text, never colour alone; the polite
// status region only changes on step/state change, never per second.

const MARK: Record<ProgressStep["state"], string> = {
  done: "✓",
  active: "●",
  waiting: "○",
  failed: "!",
  skipped: "–",
};

const STATE_LABEL: Record<ProgressStep["state"], MessageKey> = {
  done: "progress.state.done",
  active: "progress.state.active",
  waiting: "progress.state.waiting",
  failed: "progress.state.failed",
  skipped: "progress.state.skipped",
};

function announcement(model: ProgressModel, name: string): string {
  const title = t(model.title, { name });
  const cur = model.steps[model.current];
  if (model.terminal === "failed") {
    return `${title} — ${t("progress.state.failed")}: ${cur ? t(cur.label) : t("progress.step.working")}`;
  }
  if (!cur) return title;
  return `${title} — ${t("progress.step.of", { current: model.current + 1, total: model.steps.length })}: ${t(cur.label)}`;
}

function StepNotice({ step }: { step: ProgressStep }) {
  if (!step.notice && !step.reason) return null;
  // Copy first; the raw token follows it only for unknown reasons or when
  // there is no copy at all (a known-but-quiet token is the line by itself).
  const text = step.notice ? t(step.notice, { reason: step.reason ?? "" }) : null;
  const tokenLine =
    step.reason && (step.unknownReason || !step.notice)
      ? t("progress.step.reason", { reason: step.reason })
      : null;
  const body = (
    <>
      {text}
      {text && tokenLine ? " " : ""}
      {tokenLine}
    </>
  );
  return step.state === "failed" ? (
    <p className="tc-progress__notice" role="alert">
      {body}
    </p>
  ) : (
    <p className="tc-progress__notice">{body}</p>
  );
}

function StepRow({
  step,
  elapsedMs,
}: {
  step: ProgressStep;
  elapsedMs: number | undefined;
}) {
  return (
    <li
      className="tc-progress__step"
      data-state={step.state}
      aria-current={step.state === "active" ? "step" : undefined}
    >
      <span className="tc-progress__mark" aria-hidden="true">
        {MARK[step.state]}
      </span>
      <span className="tc-progress__stepbody">
        <span className="tc-progress__stepline">
          <span className="tc-progress__label">{t(step.label)}</span>
          {step.state === "active" ? (
            <span className="tc-progress__meta" aria-hidden="true">
              {elapsedMs !== undefined
                ? t("progress.step.elapsed", { elapsed: formatElapsed(elapsedMs) })
                : t("progress.state.active")}
            </span>
          ) : (
            <span className="tc-progress__meta">{t(STATE_LABEL[step.state])}</span>
          )}
        </span>
        <StepNotice step={step} />
      </span>
    </li>
  );
}

export function LifecycleProgress({
  workspace,
  variant = "full",
  refreshError,
  onRetry,
  className,
}: {
  workspace: WorkspaceView;
  variant?: "compact" | "full" | "overlay";
  refreshError?: unknown;
  onRetry?: () => void;
  className?: string;
}) {
  const { model, refreshFailures } = useProgress(workspace, { refreshError });
  const titleId = useDomId("tc-progress");
  if (!model) return null;

  const cur = model.steps[model.current];
  const refreshNotice = refreshError ? (
    refreshFailures >= 3 ? (
      <p className="tc-progress__hint" data-tone="warn">
        {t("progress.refresh.retrying")}{" "}
        {onRetry ? (
          <button type="button" className="tc-progress__retry" onClick={onRetry}>
            {t("errors.banner.retry")}
          </button>
        ) : null}
      </p>
    ) : (
      <p className="tc-progress__hint">{t("progress.refresh.retrying")}</p>
    )
  ) : null;

  if (variant === "compact") {
    return (
      <span className={cx("tc-progress", "tc-progress--compact", className)}>
        <span className="tc-progress__mark" data-state={cur?.state ?? "done"} aria-hidden="true">
          {MARK[cur?.state ?? "done"]}
        </span>{" "}
        <span className="tc-progress__compactline">
          {t(model.title, { name: workspace.name })} ·{" "}
          {t("progress.step.of", { current: model.current + 1, total: model.steps.length })}
          {cur ? ` · ${t(cur.label)}` : ""}
        </span>{" "}
        <span className="tc-progress__elapsed" aria-hidden="true">
          {formatElapsed(model.elapsedMs)}
        </span>
        <VisuallyHidden>
          <span role="status">{announcement(model, workspace.name)}</span>
        </VisuallyHidden>
      </span>
    );
  }

  return (
    <section
      className={cx("tc-progress", `tc-progress--${variant}`, className)}
      aria-labelledby={titleId}
    >
      <div className="tc-progress__head">
        <h2 id={titleId} className="tc-progress__title">
          {t(model.title, { name: workspace.name })}
        </h2>
        <span className="tc-progress__elapsed" aria-hidden="true">
          {formatElapsed(model.elapsedMs)}
        </span>
      </div>
      <ol className="tc-progress__steps">
        {model.steps.map((s) => (
          <StepRow key={s.id} step={s} elapsedMs={model.stepElapsedMs} />
        ))}
      </ol>
      {model.terminal === "failed" ? (
        <p className="tc-progress__terminal" role="alert">
          {cur?.notice ? t(cur.notice, { reason: cur.reason ?? "" }) : t("progress.failed.generic")}
        </p>
      ) : null}
      {model.delayed ? <p className="tc-progress__hint">{t("progress.delayed")}</p> : null}
      {model.stalled ? (
        <p className="tc-progress__hint" data-tone="warn">
          {t("progress.stalled")}
        </p>
      ) : null}
      {refreshNotice}
      <VisuallyHidden>
        <span role="status">{announcement(model, workspace.name)}</span>
      </VisuallyHidden>
    </section>
  );
}

