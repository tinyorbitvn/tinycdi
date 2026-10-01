import {
  cloneElement,
  isValidElement,
  useEffect,
  useRef,
  useState,
  type ReactElement,
  type ReactNode,
} from "react";
import { cx } from "./cx";
import { useDomId } from "./useId";

export interface TooltipProps {
  content: ReactNode;
  /** A single focusable element (button, link). It gets aria-describedby. */
  children: ReactElement<Record<string, unknown>>;
  placement?: "top" | "bottom";
  /** Delay before showing on hover (ms). Focus shows immediately. */
  delay?: number;
  className?: string;
}

/**
 * Supplementary hint on hover/focus (never the only label — IconButton
 * already has aria-label). Escape hides it. Positioned with CSS only.
 */
export function Tooltip({ content, children, placement = "top", delay = 300, className }: TooltipProps) {
  const id = useDomId("tip");
  const [open, setOpen] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open]);

  const show = (immediate: boolean) => {
    window.clearTimeout(timer.current);
    if (immediate) setOpen(true);
    else timer.current = window.setTimeout(() => setOpen(true), delay);
  };
  const hide = () => {
    window.clearTimeout(timer.current);
    setOpen(false);
  };

  if (!isValidElement(children)) return <>{children}</>;
  const existing = children.props["aria-describedby"];
  const trigger = cloneElement(children, {
    "aria-describedby": cx(typeof existing === "string" ? existing : undefined, id),
  });

  return (
    <span
      className={cx("tc-tooltip", className)}
      onMouseEnter={() => show(false)}
      onMouseLeave={hide}
      onFocus={() => show(true)}
      onBlur={hide}
    >
      {trigger}
      <span
        role="tooltip"
        id={id}
        className={cx("tc-tooltip__bubble", `tc-tooltip__bubble--${placement}`, open && "tc-tooltip__bubble--open")}
      >
        {content}
      </span>
    </span>
  );
}
