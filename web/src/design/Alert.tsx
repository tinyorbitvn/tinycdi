import type { ReactNode } from "react";
import { cx } from "./cx";
import type { Tone } from "./Badge";
import { IconAlertCircle, IconAlertTriangle, IconCheckCircle, IconInfo, IconX } from "./icons";
import { t } from "../i18n";

export type AlertTone = Exclude<Tone, "neutral" | "accent">;

export interface AlertProps {
  tone?: AlertTone;
  title?: ReactNode;
  children?: ReactNode;
  /** Trailing actions (e.g. a Retry button). */
  actions?: ReactNode;
  /** Shows a close button. */
  onDismiss?: () => void;
  className?: string;
}

const ICONS = {
  info: IconInfo,
  success: IconCheckCircle,
  warning: IconAlertTriangle,
  danger: IconAlertCircle,
} as const;

/**
 * Inline callout. `danger` and `warning` use role="alert" (announced
 * immediately); `info` and `success` use role="status".
 */
export function Alert({ tone = "info", title, children, actions, onDismiss, className }: AlertProps) {
  const Icon = ICONS[tone];
  return (
    <div
      role={tone === "danger" || tone === "warning" ? "alert" : "status"}
      className={cx("tc-alert", `tc-alert--${tone}`, className)}
    >
      <Icon size={20} className="tc-alert__icon" />
      <div className="tc-alert__content">
        {title ? <p className="tc-alert__title">{title}</p> : null}
        {children ? <div className="tc-alert__body">{children}</div> : null}
      </div>
      {actions ? <div className="tc-alert__actions">{actions}</div> : null}
      {onDismiss ? (
        <button type="button" className="tc-alert__dismiss" aria-label={t("common.dismiss")} onClick={onDismiss}>
          <IconX size={16} />
        </button>
      ) : null}
    </div>
  );
}
