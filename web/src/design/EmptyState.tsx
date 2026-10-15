import type { ReactNode } from "react";
import { cx } from "./cx";

export interface EmptyStateProps {
  icon?: ReactNode;
  title: ReactNode;
  description?: ReactNode;
  /** Primary call to action (usually a Button). */
  action?: ReactNode;
  /** Heading level for the title (default 2). */
  headingLevel?: 2 | 3 | 4;
  compact?: boolean;
  className?: string;
}

export function EmptyState({
  icon,
  title,
  description,
  action,
  headingLevel = 2,
  compact = false,
  className,
}: EmptyStateProps) {
  const H = `h${headingLevel}` as "h2" | "h3" | "h4";
  return (
    <div className={cx("tc-empty", compact && "tc-empty--compact", className)}>
      {icon ? (
        <div className="tc-empty__icon" aria-hidden="true">
          {icon}
        </div>
      ) : null}
      <H className="tc-empty__title">{title}</H>
      {description ? <p className="tc-empty__description">{description}</p> : null}
      {action ? <div className="tc-empty__action">{action}</div> : null}
    </div>
  );
}
