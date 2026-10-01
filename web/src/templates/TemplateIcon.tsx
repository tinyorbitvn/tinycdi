import { IconGlobe, IconLaptop, IconMonitor, cx } from "../design";
import type { ExperienceKind, RuntimeKind } from "./types";

/**
 * Visual identity for a template, derived from its runtime and experience
 * (the contract carries no icon): browser → globe, Windows VM → laptop,
 * Linux desktop → monitor. Decorative — the template name carries meaning.
 */
export function TemplateIcon({
  runtime,
  experience,
  size = "md",
  className,
}: {
  runtime: RuntimeKind;
  experience: ExperienceKind;
  size?: "sm" | "md" | "lg";
  className?: string;
}) {
  const Icon = experience === "Browser" ? IconGlobe : runtime === "WindowsVM" ? IconLaptop : IconMonitor;
  const kind = experience === "Browser" ? "browser" : runtime === "WindowsVM" ? "windows" : "linux";
  const px = size === "sm" ? 16 : size === "lg" ? 24 : 20;
  return (
    <span className={cx("tc-tpl-icon", `tc-tpl-icon--${kind}`, `tc-tpl-icon--${size}`, className)} aria-hidden="true">
      <Icon size={px} />
    </span>
  );
}
