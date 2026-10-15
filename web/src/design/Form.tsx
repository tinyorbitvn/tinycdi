import { forwardRef, type FormHTMLAttributes, type ReactNode } from "react";
import { cx } from "./cx";

export interface FormProps extends Omit<FormHTMLAttributes<HTMLFormElement>, "style"> {
  /** 2 = two equal columns from 768px; alerts, fieldsets and FormActions span both. */
  columns?: 1 | 2;
}

/** A <form> laid out as a grid with --tc-space-6 between fields. */
export const Form = forwardRef<HTMLFormElement, FormProps>(function Form({ columns = 1, className, ...rest }, ref) {
  return <form ref={ref} className={cx("tc-form", columns === 2 && "tc-form--2col", className)} {...rest} />;
});

export interface FormActionsProps {
  children: ReactNode;
  className?: string;
}

/** The submit row: primary action first; wraps on narrow screens. */
export function FormActions({ children, className }: FormActionsProps) {
  return <div className={cx("tc-form__actions", className)}>{children}</div>;
}
