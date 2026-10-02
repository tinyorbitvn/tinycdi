import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { cx } from "./cx";
import { useDomId } from "./useId";

export interface MenuItem {
  /** Stable id. */
  id: string;
  label: ReactNode;
  icon?: ReactNode;
  /** Called when chosen (click / Enter / Space). Ignored when `href` is set. */
  onSelect?: () => void;
  /** Render as a link (e.g. "Sign out" to a server route). */
  href?: string;
  danger?: boolean;
  disabled?: boolean;
  /** Draw a divider above this item. */
  separatorBefore?: boolean;
}

export interface MenuTriggerProps {
  ref: (el: HTMLButtonElement | null) => void;
  id: string;
  "aria-haspopup": "menu";
  "aria-expanded": boolean;
  "aria-controls": string;
  onClick: () => void;
  onKeyDown: (e: KeyboardEvent<HTMLButtonElement>) => void;
}

export interface MenuProps {
  /** Render the trigger; spread the props onto a <button> (or Button/IconButton). */
  trigger: (props: MenuTriggerProps) => ReactNode;
  items: MenuItem[];
  /** Accessible name for the menu (defaults to the trigger via aria-labelledby). */
  label?: string;
  /** Horizontal alignment of the popup relative to the trigger. */
  align?: "start" | "end";
  /** Optional non-interactive header (e.g. signed-in user). */
  header?: ReactNode;
  className?: string;
}

/** WAI-ARIA menu button: arrows/Home/End/typeahead-free, Escape/outside-click close. */
export function Menu({ trigger, items, label, align = "end", header, className }: MenuProps) {
  const id = useDomId("menu");
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const pendingFocus = useRef<"first" | "last">("first");

  const itemEls = () =>
    Array.from(listRef.current?.querySelectorAll<HTMLElement>("[role='menuitem']:not([aria-disabled='true'])") ?? []);

  const close = useCallback((restore = true) => {
    setOpen(false);
    if (restore) triggerRef.current?.focus();
  }, []);

  useEffect(() => {
    if (!open) return;
    const els = itemEls();
    (pendingFocus.current === "last" ? els[els.length - 1] : els[0])?.focus();
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) close(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open, close]);

  const openWith = (focus: "first" | "last") => {
    pendingFocus.current = focus;
    setOpen(true);
  };

  const onTriggerKey = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (e.key === "ArrowDown" || e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      openWith("first");
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      openWith("last");
    }
  };

  const onListKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const els = itemEls();
    const idx = els.indexOf(document.activeElement as HTMLElement);
    if (e.key === "ArrowDown") {
      e.preventDefault();
      els[(idx + 1) % els.length]?.focus();
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      els[(idx - 1 + els.length) % els.length]?.focus();
    } else if (e.key === "Home") {
      e.preventDefault();
      els[0]?.focus();
    } else if (e.key === "End") {
      e.preventDefault();
      els[els.length - 1]?.focus();
    } else if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      close();
    } else if (e.key === "Tab") {
      close(false);
    }
  };

  const choose = (item: MenuItem) => {
    if (item.disabled) return;
    close();
    item.onSelect?.();
  };

  return (
    <div ref={rootRef} className={cx("tc-menu", className)}>
      {trigger({
        ref: (el) => {
          triggerRef.current = el;
        },
        id: `${id}-trigger`,
        "aria-haspopup": "menu",
        "aria-expanded": open,
        "aria-controls": `${id}-list`,
        onClick: () => (open ? close(false) : openWith("first")),
        onKeyDown: onTriggerKey,
      })}
      {open ? (
        <div
          ref={listRef}
          id={`${id}-list`}
          role="menu"
          {...(label ? { "aria-label": label } : { "aria-labelledby": `${id}-trigger` })}
          className={cx("tc-menu__popup", `tc-menu__popup--${align}`)}
          onKeyDown={onListKey}
        >
          {header ? <div className="tc-menu__header">{header}</div> : null}
          {items.map((item) => {
            const content = (
              <>
                {item.icon ? <span className="tc-menu__icon">{item.icon}</span> : null}
                <span className="tc-menu__label">{item.label}</span>
              </>
            );
            const cls = cx("tc-menu__item", item.danger && "tc-menu__item--danger");
            return (
              <div key={item.id} role="none">
                {item.separatorBefore ? <div role="separator" className="tc-menu__separator" /> : null}
                {item.href && !item.disabled ? (
                  <a role="menuitem" tabIndex={-1} href={item.href} className={cls} onClick={() => setOpen(false)}>
                    {content}
                  </a>
                ) : (
                  <button
                    type="button"
                    role="menuitem"
                    tabIndex={-1}
                    className={cls}
                    {...(item.disabled ? { "aria-disabled": true } : {})}
                    onClick={() => choose(item)}
                  >
                    {content}
                  </button>
                )}
              </div>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}
