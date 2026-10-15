import type { ReactNode } from "react";
import { cx } from "./cx";
import { useDomId } from "./useId";

export type StatTone = "neutral" | "success" | "warning" | "danger";

export interface StatProps {
  /** What the figure is; also the group's accessible name. */
  label: ReactNode;
  value: ReactNode;
  hint?: ReactNode;
  tone?: StatTone;
  /** A link or small button under the value (e.g. "Review"). */
  action?: ReactNode;
  className?: string;
}

/** One figure, label on top. A `role="group"` div named by its label. */
export function Stat({ label, value, hint, tone = "neutral", action, className }: StatProps) {
  const id = useDomId("stat");
  return (
    <div role="group" aria-labelledby={id} className={cx("tc-stat", `tc-stat--${tone}`, className)}>
      <p id={id} className="tc-stat__label">
        {label}
      </p>
      <p className="tc-stat__value">{value}</p>
      {hint ? <p className="tc-stat__hint">{hint}</p> : null}
      {action ? <div className="tc-stat__action">{action}</div> : null}
    </div>
  );
}

export interface StatListItem {
  key: string;
  term: ReactNode;
  value: ReactNode;
}

export interface StatListProps {
  label: ReactNode;
  items: StatListItem[];
  /** Shown instead of the rows when `items` is empty. */
  empty?: ReactNode;
  className?: string;
}

/** A Stat card holding key/value rows (e.g. running workspaces per template). */
export function StatList({ label, items, empty, className }: StatListProps) {
  const id = useDomId("stat");
  return (
    <div role="group" aria-labelledby={id} className={cx("tc-stat", "tc-stat--list", className)}>
      <p id={id} className="tc-stat__label">
        {label}
      </p>
      {items.length === 0 ? (
        empty ? (
          <p className="tc-stat__hint">{empty}</p>
        ) : null
      ) : (
        <dl className="tc-statlist">
          {items.map((it) => (
            <div key={it.key} className="tc-statlist__row">
              <dt>{it.term}</dt>
              <dd>{it.value}</dd>
            </div>
          ))}
        </dl>
      )}
    </div>
  );
}
