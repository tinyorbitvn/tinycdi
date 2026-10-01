import { useEffect, useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Button } from "./Button";
import { cx } from "./cx";
import { useFocusTrap } from "./focus";
import { IconX } from "./icons";
import { useDomId } from "./useId";

let openCount = 0;

function useScrollLock(active: boolean) {
  useEffect(() => {
    if (!active) return;
    openCount += 1;
    document.documentElement.classList.add("tc-scroll-locked");
    return () => {
      openCount -= 1;
      if (openCount === 0) document.documentElement.classList.remove("tc-scroll-locked");
    };
  }, [active]);
}

interface OverlayProps {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  /** Footer actions, right-aligned. */
  footer?: ReactNode;
  /** Close on backdrop click (default true). Escape always closes unless `dismissible` is false. */
  closeOnBackdrop?: boolean;
  /** When false, Escape/backdrop/close button are disabled (e.g. while a request runs). */
  dismissible?: boolean;
  /** `alertdialog` for destructive confirmations. */
  role?: "dialog" | "alertdialog";
  className?: string;
}

function Overlay({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  closeOnBackdrop = true,
  dismissible = true,
  role = "dialog",
  kind,
  className,
}: OverlayProps & { kind: string }) {
  const panelRef = useRef<HTMLDivElement>(null);
  const titleId = useDomId("dlg-title");
  const descId = useDomId("dlg-desc");
  useFocusTrap(panelRef, open);
  useScrollLock(open);

  useEffect(() => {
    if (!open || !dismissible) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, dismissible, onClose]);

  if (!open) return null;
  return createPortal(
    <div className={cx("tc-overlay", `tc-overlay--${kind}`)}>
      <div
        className="tc-overlay__backdrop"
        aria-hidden="true"
        onClick={dismissible && closeOnBackdrop ? onClose : undefined}
      />
      <div
        ref={panelRef}
        role={role}
        aria-modal="true"
        aria-labelledby={titleId}
        {...(description ? { "aria-describedby": descId } : {})}
        tabIndex={-1}
        className={cx("tc-overlay__panel", `tc-${kind}`, className)}
      >
        <div className="tc-overlay__header">
          <div>
            <h2 id={titleId} className="tc-overlay__title">
              {title}
            </h2>
            {description ? (
              <p id={descId} className="tc-overlay__description">
                {description}
              </p>
            ) : null}
          </div>
          {dismissible ? (
            <button type="button" className="tc-overlay__close" aria-label="Close" onClick={onClose}>
              <IconX size={20} />
            </button>
          ) : null}
        </div>
        {children !== undefined ? <div className="tc-overlay__body">{children}</div> : null}
        {footer ? <div className="tc-overlay__footer">{footer}</div> : null}
      </div>
    </div>,
    document.body,
  );
}

export interface DialogProps extends OverlayProps {
  size?: "sm" | "md" | "lg";
}

/** Modal dialog: portal to body, focus-trapped, Escape/backdrop close, focus restored. */
export function Dialog({ size = "md", className, ...rest }: DialogProps) {
  return <Overlay kind="dialog" className={cx(`tc-dialog--${size}`, className)} {...rest} />;
}

export interface DrawerProps extends OverlayProps {
  side?: "right" | "left";
  size?: "sm" | "md" | "lg";
}

/** Side sheet with the same behaviour as Dialog. */
export function Drawer({ side = "right", size = "md", className, ...rest }: DrawerProps) {
  return <Overlay kind="drawer" className={cx(`tc-drawer--${side}`, `tc-drawer--${size}`, className)} {...rest} />;
}

export interface ConfirmDialogProps {
  open: boolean;
  title: ReactNode;
  /** Explanation of the consequence. */
  children?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** Danger styling + role="alertdialog" (default true). */
  destructive?: boolean;
  /** Shows a spinner on the confirm button and blocks dismissal. */
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

/** Confirm/cancel prompt; the cancel button gets initial focus. */
export function ConfirmDialog({
  open,
  title,
  children,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  destructive = true,
  busy = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  return (
    <Dialog
      open={open}
      onClose={onCancel}
      title={title}
      size="sm"
      role={destructive ? "alertdialog" : "dialog"}
      dismissible={!busy}
      footer={
        <>
          <Button onClick={onCancel} disabled={busy} data-autofocus>
            {cancelLabel}
          </Button>
          <Button variant={destructive ? "danger" : "primary"} loading={busy} onClick={onConfirm}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      {children}
    </Dialog>
  );
}
