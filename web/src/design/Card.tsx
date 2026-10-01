import type { HTMLAttributes, ReactNode } from "react";
import { cx } from "./cx";
import { useDomId } from "./useId";

export interface CardProps extends Omit<HTMLAttributes<HTMLElement>, "title" | "style"> {
  /** Heading text; becomes the card's accessible name. */
  title?: ReactNode;
  /** Heading level for `title` (default 2). */
  headingLevel?: 2 | 3 | 4;
  /** Secondary line under the title. */
  description?: ReactNode;
  /** Right-aligned header content (buttons, menu). */
  actions?: ReactNode;
  footer?: ReactNode;
  /** Remove body padding (for edge-to-edge tables/lists). */
  flush?: boolean;
  /** Hover/press affordance for clickable cards. */
  interactive?: boolean;
  as?: "section" | "article" | "div" | "li";
}

export function Card({
  title,
  headingLevel = 2,
  description,
  actions,
  footer,
  flush = false,
  interactive = false,
  as: Tag = "section",
  className,
  children,
  ...rest
}: CardProps) {
  const headingId = useDomId("card");
  const H = `h${headingLevel}` as "h2" | "h3" | "h4";
  const hasHeader = title !== undefined || actions !== undefined;
  return (
    <Tag
      className={cx("tc-card", interactive && "tc-card--interactive", className)}
      {...(title !== undefined && Tag !== "div" ? { "aria-labelledby": headingId } : {})}
      {...rest}
    >
      {hasHeader ? (
        <div className="tc-card__header">
          <div className="tc-card__heading">
            {title !== undefined ? (
              <H id={headingId} className="tc-card__title">
                {title}
              </H>
            ) : null}
            {description !== undefined ? <p className="tc-card__description">{description}</p> : null}
          </div>
          {actions !== undefined ? <div className="tc-card__actions">{actions}</div> : null}
        </div>
      ) : null}
      <div className={cx("tc-card__body", flush && "tc-card__body--flush")}>{children}</div>
      {footer !== undefined ? <div className="tc-card__footer">{footer}</div> : null}
    </Tag>
  );
}
