import { useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { cx } from "./cx";
import { useDomId } from "./useId";

export interface TabItem {
  id: string;
  label: ReactNode;
  /** Panel content; omit when the parent renders content from `value`. */
  content?: ReactNode;
  disabled?: boolean;
  /** Trailing count/badge. */
  badge?: ReactNode;
}

export interface TabsProps {
  tabs: TabItem[];
  /** Accessible name of the tab list. */
  label: string;
  /** Controlled selected id. */
  value?: string;
  defaultValue?: string;
  onChange?: (id: string) => void;
  className?: string;
}

/** WAI-ARIA tabs: arrow keys/Home/End move and activate, roving tabindex. */
export function Tabs({ tabs, label, value, defaultValue, onChange, className }: TabsProps) {
  const base = useDomId("tabs");
  const [internal, setInternal] = useState(defaultValue ?? tabs.find((t) => !t.disabled)?.id ?? "");
  const selected = value ?? internal;
  const refs = useRef<Record<string, HTMLButtonElement | null>>({});

  const select = (id: string) => {
    if (value === undefined) setInternal(id);
    onChange?.(id);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const enabled = tabs.filter((t) => !t.disabled);
    const idx = enabled.findIndex((t) => t.id === selected);
    let next: TabItem | undefined;
    if (e.key === "ArrowRight") next = enabled[(idx + 1) % enabled.length];
    else if (e.key === "ArrowLeft") next = enabled[(idx - 1 + enabled.length) % enabled.length];
    else if (e.key === "Home") next = enabled[0];
    else if (e.key === "End") next = enabled[enabled.length - 1];
    if (!next) return;
    e.preventDefault();
    select(next.id);
    refs.current[next.id]?.focus();
  };

  const current = tabs.find((t) => t.id === selected);
  return (
    <div className={cx("tc-tabs", className)}>
      <div role="tablist" aria-label={label} className="tc-tabs__list" onKeyDown={onKeyDown}>
        {tabs.map((t) => {
          const isSel = t.id === selected;
          return (
            <button
              key={t.id}
              ref={(el) => {
                refs.current[t.id] = el;
              }}
              type="button"
              role="tab"
              id={`${base}-tab-${t.id}`}
              aria-selected={isSel}
              aria-controls={`${base}-panel-${t.id}`}
              tabIndex={isSel ? 0 : -1}
              disabled={t.disabled}
              className={cx("tc-tabs__tab", isSel && "tc-tabs__tab--selected")}
              onClick={() => select(t.id)}
            >
              {t.label}
              {t.badge !== undefined ? <span className="tc-tabs__badge">{t.badge}</span> : null}
            </button>
          );
        })}
      </div>
      {current && current.content !== undefined ? (
        <div
          role="tabpanel"
          id={`${base}-panel-${current.id}`}
          aria-labelledby={`${base}-tab-${current.id}`}
          tabIndex={0}
          className="tc-tabs__panel"
        >
          {current.content}
        </div>
      ) : null}
    </div>
  );
}
