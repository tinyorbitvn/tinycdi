import { useId as useReactId } from "react";

/** React's useId, made safe for use inside CSS/DOM id selectors. */
export function useDomId(prefix = "tc"): string {
  return `${prefix}-${useReactId().replace(/[^A-Za-z0-9_-]/g, "")}`;
}
