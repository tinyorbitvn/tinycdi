import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import type { AlertTone } from "./Alert";
import { cx } from "./cx";
import { IconAlertCircle, IconAlertTriangle, IconCheckCircle, IconInfo, IconX } from "./icons";

export interface ToastOptions {
  title: ReactNode;
  description?: ReactNode;
  tone?: AlertTone;
  /** Auto-dismiss after ms (default 5000; danger defaults to sticky). 0 = sticky. */
  duration?: number;
  /** Optional single action, e.g. { label: "Undo", onClick }. */
  action?: { label: string; onClick: () => void };
}

interface ToastEntry extends ToastOptions {
  id: number;
}

export interface ToastApi {
  /** Shows a toast; returns its id. */
  toast: (opts: ToastOptions) => number;
  dismiss: (id: number) => void;
}

const ToastContext = createContext<ToastApi | null>(null);

const ICONS = {
  info: IconInfo,
  success: IconCheckCircle,
  warning: IconAlertTriangle,
  danger: IconAlertCircle,
} as const;

function ToastItem({ entry, onDismiss }: { entry: ToastEntry; onDismiss: (id: number) => void }) {
  const tone = entry.tone ?? "info";
  const duration = entry.duration ?? (tone === "danger" ? 0 : 5000);
  const [paused, setPaused] = useState(false);
  const Icon = ICONS[tone];

  useEffect(() => {
    if (duration <= 0 || paused) return;
    const t = window.setTimeout(() => onDismiss(entry.id), duration);
    return () => window.clearTimeout(t);
  }, [duration, paused, entry.id, onDismiss]);

  return (
    <li
      className={cx("tc-toast", `tc-toast--${tone}`)}
      onMouseEnter={() => setPaused(true)}
      onMouseLeave={() => setPaused(false)}
      onFocus={() => setPaused(true)}
      onBlur={() => setPaused(false)}
    >
      <Icon size={20} className="tc-toast__icon" />
      <div className="tc-toast__content">
        <p className="tc-toast__title">{entry.title}</p>
        {entry.description ? <p className="tc-toast__description">{entry.description}</p> : null}
      </div>
      {entry.action ? (
        <button
          type="button"
          className="tc-toast__action"
          onClick={() => {
            entry.action!.onClick();
            onDismiss(entry.id);
          }}
        >
          {entry.action.label}
        </button>
      ) : null}
      <button type="button" className="tc-toast__close" aria-label="Dismiss notification" onClick={() => onDismiss(entry.id)}>
        <IconX size={16} />
      </button>
    </li>
  );
}

/**
 * Hosts toasts. Two live regions: polite (info/success/warning) and
 * assertive (danger), so screen readers announce errors immediately.
 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<ToastEntry[]>([]);
  const nextId = useRef(1);

  const dismiss = useCallback((id: number) => setToasts((ts) => ts.filter((t) => t.id !== id)), []);
  const toast = useCallback((opts: ToastOptions) => {
    const id = nextId.current++;
    setToasts((ts) => [...ts.slice(-4), { ...opts, id }]);
    return id;
  }, []);
  const api = useMemo(() => ({ toast, dismiss }), [toast, dismiss]);

  const polite = toasts.filter((t) => t.tone !== "danger");
  const assertive = toasts.filter((t) => t.tone === "danger");

  return (
    <ToastContext.Provider value={api}>
      {children}
      {createPortal(
        <div className="tc-toaster">
          <ol className="tc-toaster__list" aria-live="assertive" aria-label="Error notifications">
            {assertive.map((t) => (
              <ToastItem key={t.id} entry={t} onDismiss={dismiss} />
            ))}
          </ol>
          <ol className="tc-toaster__list" aria-live="polite" aria-label="Notifications">
            {polite.map((t) => (
              <ToastItem key={t.id} entry={t} onDismiss={dismiss} />
            ))}
          </ol>
        </div>,
        document.body,
      )}
    </ToastContext.Provider>
  );
}

export function useToast(): ToastApi {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error("useToast must be used inside <ToastProvider>");
  return ctx;
}
