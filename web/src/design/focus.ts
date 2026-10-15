import { useEffect, useRef, type RefObject } from "react";

const FOCUSABLE = [
  "a[href]",
  "area[href]",
  "button:not([disabled])",
  "input:not([disabled]):not([type='hidden'])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  "iframe",
  "[contenteditable='true']",
  "[tabindex]:not([tabindex='-1'])",
].join(",");

export function focusableIn(root: HTMLElement): HTMLElement[] {
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
    (el) => !el.hasAttribute("inert") && el.getAttribute("aria-hidden") !== "true",
  );
}

/**
 * While `active`: moves focus into `ref` (the `[data-autofocus]` element,
 * else the first focusable, else the container), keeps Tab/Shift+Tab inside
 * it, and restores focus to the previously focused element on deactivate.
 */
export function useFocusTrap(ref: RefObject<HTMLElement | null>, active: boolean): void {
  const restoreTo = useRef<HTMLElement | null>(null);

  useEffect(() => {
    const root = ref.current;
    if (!active || !root) return;
    restoreTo.current = document.activeElement as HTMLElement | null;

    const initial =
      root.querySelector<HTMLElement>("[data-autofocus]") ?? focusableIn(root)[0] ?? root;
    initial.focus();

    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Tab") return;
      const items = focusableIn(root);
      if (items.length === 0) {
        e.preventDefault();
        root.focus();
        return;
      }
      const first = items[0]!;
      const last = items[items.length - 1]!;
      const activeEl = document.activeElement;
      if (e.shiftKey && (activeEl === first || !root.contains(activeEl))) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && (activeEl === last || !root.contains(activeEl))) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKeyDown, true);
    return () => {
      document.removeEventListener("keydown", onKeyDown, true);
      const target = restoreTo.current;
      if (target && typeof target.focus === "function" && document.contains(target)) target.focus();
    };
  }, [ref, active]);
}
