import type { HTMLAttributes, ReactNode } from "react";
import { cx } from "./cx";
import { useDomId } from "./useId";

// Page structure. The app shell owns <main>; pages render <Page> inside it.

export interface PageProps {
  title: ReactNode;
  description?: ReactNode;
  /** Right-aligned header actions (primary button etc.). */
  actions?: ReactNode;
  /** Small content above the title (e.g. a back link / breadcrumbs). */
  eyebrow?: ReactNode;
  /** `narrow` caps the content at a readable width (forms). */
  width?: "default" | "narrow" | "full";
  children?: ReactNode;
  className?: string;
}

export function Page({ title, description, actions, eyebrow, width = "default", children, className }: PageProps) {
  return (
    <div className={cx("tc-page", `tc-page--${width}`, className)}>
      <header className="tc-page__header">
        <div className="tc-page__heading">
          {eyebrow ? <div className="tc-page__eyebrow">{eyebrow}</div> : null}
          <h1 className="tc-page__title">{title}</h1>
          {description ? <p className="tc-page__description">{description}</p> : null}
        </div>
        {actions ? <div className="tc-page__actions">{actions}</div> : null}
      </header>
      <div className="tc-page__body">{children}</div>
    </div>
  );
}

export interface SectionProps {
  title?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  /** Heading level (default 2). */
  headingLevel?: 2 | 3 | 4;
  children?: ReactNode;
  className?: string;
}

export function Section({ title, description, actions, headingLevel = 2, children, className }: SectionProps) {
  const id = useDomId("section");
  const H = `h${headingLevel}` as "h2" | "h3" | "h4";
  return (
    <section className={cx("tc-section", className)} {...(title ? { "aria-labelledby": id } : {})}>
      {title || actions ? (
        <div className="tc-section__header">
          <div>
            {title ? (
              <H id={id} className="tc-section__title">
                {title}
              </H>
            ) : null}
            {description ? <p className="tc-section__description">{description}</p> : null}
          </div>
          {actions ? <div className="tc-section__actions">{actions}</div> : null}
        </div>
      ) : null}
      {children}
    </section>
  );
}

export type Gap = 0 | 1 | 2 | 3 | 4 | 5 | 6 | 8;

export interface StackProps extends Omit<HTMLAttributes<HTMLDivElement>, "style"> {
  gap?: Gap;
  /** Cross-axis alignment. */
  align?: "start" | "center" | "end" | "stretch";
}

/** Vertical flow with a token gap. */
export function Stack({ gap = 4, align = "stretch", className, ...rest }: StackProps) {
  return <div className={cx("tc-stack", `tc-gap-${gap}`, `tc-align-${align}`, className)} {...rest} />;
}

export interface ClusterProps extends Omit<HTMLAttributes<HTMLDivElement>, "style"> {
  gap?: Gap;
  align?: "start" | "center" | "end" | "baseline" | "stretch";
  justify?: "start" | "center" | "end" | "between";
  /** Allow wrapping (default true). */
  wrap?: boolean;
}

/** Horizontal row of items with a token gap; wraps on small screens. */
export function Cluster({
  gap = 2,
  align = "center",
  justify = "start",
  wrap = true,
  className,
  ...rest
}: ClusterProps) {
  return (
    <div
      className={cx(
        "tc-cluster",
        `tc-gap-${gap}`,
        `tc-align-${align}`,
        `tc-justify-${justify}`,
        !wrap && "tc-nowrap",
        className,
      )}
      {...rest}
    />
  );
}

export interface GridProps extends Omit<HTMLAttributes<HTMLDivElement>, "style"> {
  gap?: Gap;
  /** Minimum column width before wrapping (auto-fill grid). */
  min?: "xs" | "sm" | "md" | "lg";
}

/** Responsive auto-fill grid (cards, stat tiles). */
export function Grid({ gap = 4, min = "md", className, ...rest }: GridProps) {
  return <div className={cx("tc-grid", `tc-gap-${gap}`, `tc-grid--${min}`, className)} {...rest} />;
}

export interface DescriptionListProps {
  items: Array<{ term: ReactNode; detail: ReactNode; key?: string }>;
  /** `columns` = term beside detail (default); `stacked` = term above detail. */
  layout?: "columns" | "stacked";
  className?: string;
}

export function DescriptionList({ items, layout = "columns", className }: DescriptionListProps) {
  return (
    <dl className={cx("tc-dl", `tc-dl--${layout}`, className)}>
      {items.map((it, i) => (
        <div className="tc-dl__row" key={it.key ?? i}>
          <dt>{it.term}</dt>
          <dd>{it.detail}</dd>
        </div>
      ))}
    </dl>
  );
}

export function VisuallyHidden({ children }: { children: ReactNode }) {
  return <span className="tc-sr-only">{children}</span>;
}

export interface MeterProps {
  /** Accessible label, e.g. "CPU". */
  label: ReactNode;
  value: number;
  max: number;
  /** Text shown on the right (default "value / max"). */
  valueText?: ReactNode;
  /** Fraction (0–1) at which the bar turns warning / danger. */
  warnAt?: number;
  dangerAt?: number;
  className?: string;
}

/** Quota/usage bar on a native <meter> (styled; no inline widths). */
export function Meter({ label, value, max, valueText, warnAt = 0.8, dangerAt = 0.95, className }: MeterProps) {
  const id = useDomId("meter");
  const ratio = max > 0 ? value / max : 0;
  const tone = ratio >= dangerAt ? "danger" : ratio >= warnAt ? "warning" : "ok";
  return (
    <div className={cx("tc-meter", `tc-meter--${tone}`, className)}>
      <div className="tc-meter__header">
        <label htmlFor={id} className="tc-meter__label">
          {label}
        </label>
        <span className="tc-meter__value">{valueText ?? `${value} / ${max}`}</span>
      </div>
      <meter
        id={id}
        className="tc-meter__bar"
        min={0}
        max={max > 0 ? max : 1}
        value={Math.min(value, max > 0 ? max : 1)}
        low={max * warnAt}
        high={max * dangerAt}
        optimum={0}
      />
    </div>
  );
}
