import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import { cx } from "./cx";
import { Spinner } from "./Spinner";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";
export type ButtonSize = "sm" | "md" | "lg";

/** Class list for a button look — use on links (`<Link className={buttonClass(...)}>`). */
export function buttonClass(
  variant: ButtonVariant = "secondary",
  size: ButtonSize = "md",
  extra?: string,
): string {
  return cx("tc-button", `tc-button--${variant}`, `tc-button--${size}`, extra);
}

export interface ButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "style"> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  /** Shows a spinner, sets aria-busy and disables the button. */
  loading?: boolean;
  /** Icon rendered before the label. */
  icon?: ReactNode;
  /** Icon rendered after the label. */
  iconEnd?: ReactNode;
  /** Stretch to the container width. */
  block?: boolean;
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  {
    variant = "secondary",
    size = "md",
    loading = false,
    icon,
    iconEnd,
    block = false,
    type = "button",
    disabled,
    className,
    children,
    ...rest
  },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      className={buttonClass(variant, size, cx(block && "tc-button--block", className))}
      {...rest}
    >
      {loading ? <Spinner size="sm" decorative /> : icon}
      {children !== undefined && children !== null ? (
        <span className="tc-button__label">{children}</span>
      ) : null}
      {iconEnd}
    </button>
  );
});

export interface IconButtonProps
  extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "style" | "children" | "aria-label"> {
  /** Accessible name (required: the button has no visible text). */
  label: string;
  icon: ReactNode;
  variant?: Exclude<ButtonVariant, "primary"> | "primary";
  size?: ButtonSize;
  loading?: boolean;
}

export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton(
  { label, icon, variant = "ghost", size = "md", loading = false, type = "button", disabled, className, ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      aria-label={label}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      className={buttonClass(variant, size, cx("tc-button--icon", className))}
      {...rest}
    >
      {loading ? <Spinner size="sm" decorative /> : icon}
    </button>
  );
});
