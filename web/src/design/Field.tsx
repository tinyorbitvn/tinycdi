import {
  forwardRef,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";
import { cx } from "./cx";
import { IconChevronDown } from "./icons";
import { useDomId } from "./useId";

/** Ids/ARIA wiring handed to a custom control rendered inside <Field>. */
export interface FieldControlProps {
  id: string;
  "aria-describedby"?: string;
  "aria-invalid"?: true;
  "aria-required"?: true;
}

export interface FieldProps {
  label: ReactNode;
  /** Help text under the control. */
  hint?: ReactNode;
  /** Error message; marks the control aria-invalid. */
  error?: ReactNode;
  required?: boolean;
  /** Visually hide the label (still announced). */
  hideLabel?: boolean;
  /** Use a given id instead of a generated one. */
  id?: string;
  className?: string;
  children: (control: FieldControlProps) => ReactNode;
}

/** Label + hint + error wrapper for any control (render-prop gets the ids). */
export function Field({ label, hint, error, required, hideLabel, id, className, children }: FieldProps) {
  const generated = useDomId("field");
  const controlId = id ?? generated;
  const hintId = hint ? `${controlId}-hint` : undefined;
  const errorId = error ? `${controlId}-error` : undefined;
  const describedBy = [errorId, hintId].filter(Boolean).join(" ") || undefined;
  return (
    <div className={cx("tc-field", error ? "tc-field--invalid" : undefined, className)}>
      <label htmlFor={controlId} className={cx("tc-field__label", hideLabel && "tc-sr-only")}>
        {label}
        {required ? (
          <span className="tc-field__required" aria-hidden="true">
            {" "}
            *
          </span>
        ) : null}
      </label>
      {children({
        id: controlId,
        ...(describedBy ? { "aria-describedby": describedBy } : {}),
        ...(error ? { "aria-invalid": true as const } : {}),
        ...(required ? { "aria-required": true as const } : {}),
      })}
      {hint ? (
        <p id={hintId} className="tc-field__hint">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={errorId} className="tc-field__error">
          {error}
        </p>
      ) : null}
    </div>
  );
}

interface CommonFieldProps {
  label: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  hideLabel?: boolean;
  /** Class for the outer field wrapper (the control gets `className`). */
  fieldClassName?: string;
}

export interface InputProps extends CommonFieldProps, Omit<InputHTMLAttributes<HTMLInputElement>, "style"> {}

export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  { label, hint, error, hideLabel, fieldClassName, required, id, className, ...rest },
  ref,
) {
  return (
    <Field
      label={label}
      hint={hint}
      error={error}
      required={required ?? false}
      hideLabel={hideLabel ?? false}
      {...(id ? { id } : {})}
      {...(fieldClassName ? { className: fieldClassName } : {})}
    >
      {(c) => <input ref={ref} required={required} className={cx("tc-input", className)} {...c} {...rest} />}
    </Field>
  );
});

export interface SelectProps extends CommonFieldProps, Omit<SelectHTMLAttributes<HTMLSelectElement>, "style"> {
  /** Convenience: options rendered before `children`. */
  options?: Array<{ value: string; label: ReactNode; disabled?: boolean }>;
}

export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  { label, hint, error, hideLabel, fieldClassName, required, id, className, options, children, ...rest },
  ref,
) {
  return (
    <Field
      label={label}
      hint={hint}
      error={error}
      required={required ?? false}
      hideLabel={hideLabel ?? false}
      {...(id ? { id } : {})}
      {...(fieldClassName ? { className: fieldClassName } : {})}
    >
      {(c) => (
        <span className="tc-select">
          <select ref={ref} required={required} className={cx("tc-input tc-select__control", className)} {...c} {...rest}>
            {options?.map((o) => (
              <option key={o.value} value={o.value} disabled={o.disabled}>
                {o.label}
              </option>
            ))}
            {children}
          </select>
          <IconChevronDown className="tc-select__chevron" />
        </span>
      )}
    </Field>
  );
});

export interface TextareaProps
  extends CommonFieldProps,
    Omit<TextareaHTMLAttributes<HTMLTextAreaElement>, "style"> {}

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(function Textarea(
  { label, hint, error, hideLabel, fieldClassName, required, id, className, ...rest },
  ref,
) {
  return (
    <Field
      label={label}
      hint={hint}
      error={error}
      required={required ?? false}
      hideLabel={hideLabel ?? false}
      {...(id ? { id } : {})}
      {...(fieldClassName ? { className: fieldClassName } : {})}
    >
      {(c) => (
        <textarea ref={ref} required={required} className={cx("tc-input tc-textarea", className)} {...c} {...rest} />
      )}
    </Field>
  );
});

export interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, "style" | "type"> {
  label: ReactNode;
  hint?: ReactNode;
}

export const Checkbox = forwardRef<HTMLInputElement, CheckboxProps>(function Checkbox(
  { label, hint, id, className, ...rest },
  ref,
) {
  const generated = useDomId("check");
  const controlId = id ?? generated;
  const hintId = hint ? `${controlId}-hint` : undefined;
  return (
    <div className={cx("tc-check", className)}>
      <input
        ref={ref}
        id={controlId}
        type="checkbox"
        className="tc-check__input"
        {...(hintId ? { "aria-describedby": hintId } : {})}
        {...rest}
      />
      <div>
        <label htmlFor={controlId} className="tc-check__label">
          {label}
        </label>
        {hint ? (
          <p id={hintId} className="tc-field__hint">
            {hint}
          </p>
        ) : null}
      </div>
    </div>
  );
});
