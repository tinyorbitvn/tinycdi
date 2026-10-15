import { t } from "../i18n";
import { isPortalApiError } from "./errors";

/** "CODE · Request ID: id" for support; null for non-API errors. */
export function errorMeta(error: unknown): string | null {
  if (!isPortalApiError(error)) return null;
  return error.requestId
    ? `${error.code} · ${t("errors.banner.requestId", { id: error.requestId })}`
    : error.code;
}

/** Muted detail line under an error alert (stable code + request id). */
export function ErrorMeta({ error }: { error: unknown }) {
  const meta = errorMeta(error);
  return meta ? <p className="tc-alert__meta">{meta}</p> : null;
}
