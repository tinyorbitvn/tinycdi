import { cx } from "./cx";

export interface SpinnerProps {
  size?: "sm" | "md" | "lg";
  /** Screen-reader text; ignored when `decorative`. */
  label?: string;
  /** Hide from assistive tech (e.g. inside a button that already says aria-busy). */
  decorative?: boolean;
  className?: string;
}

export function Spinner({ size = "md", label = "Loading", decorative = false, className }: SpinnerProps) {
  if (decorative) {
    return <span className={cx("tc-spinner", `tc-spinner--${size}`, className)} aria-hidden="true" />;
  }
  return (
    <span role="status" className={cx("tc-spinner-wrap", className)}>
      <span className={cx("tc-spinner", `tc-spinner--${size}`)} aria-hidden="true" />
      <span className="tc-sr-only">{label}</span>
    </span>
  );
}

export type SkeletonWidth = "25" | "50" | "75" | "100";

export interface SkeletonProps {
  variant?: "text" | "block" | "circle";
  /** Number of text lines (variant="text"). The last line is shorter. */
  lines?: number;
  /** Width preset in percent (no inline styles under CSP). */
  width?: SkeletonWidth;
  /** Height preset for variant="block". */
  height?: "sm" | "md" | "lg";
  className?: string;
}

/** Loading placeholder. Decorative: pair with aria-busy on the region it fills. */
export function Skeleton({ variant = "text", lines = 1, width = "100", height = "md", className }: SkeletonProps) {
  if (variant === "text") {
    return (
      <span className={cx("tc-skeleton-lines", className)} aria-hidden="true">
        {Array.from({ length: lines }, (_, i) => (
          <span
            key={i}
            className={cx(
              "tc-skeleton tc-skeleton--text",
              `tc-w-${lines > 1 && i === lines - 1 ? "50" : width}`,
            )}
          />
        ))}
      </span>
    );
  }
  return (
    <span
      aria-hidden="true"
      className={cx(
        "tc-skeleton",
        `tc-skeleton--${variant}`,
        variant === "block" && `tc-skeleton--h-${height}`,
        variant === "block" && `tc-w-${width}`,
        className,
      )}
    />
  );
}
