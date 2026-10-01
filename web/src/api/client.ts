import createClient, { type Client, type Middleware } from "openapi-fetch";
import type { paths } from "./generated/schema";
import { PortalApiError } from "./errors";

export type ApiClient = Client<paths>;

// Non-HttpOnly CSRF cookie set by the login flow; echoed as X-CSRF-Token on
// every state-changing request (see ADR 0002 §6).
export const CSRF_COOKIE = "tcdi_csrf";
export const CSRF_HEADER = "X-CSRF-Token";

// JS-readable cookie the API sets at login carrying the deployment's
// configured session origin. launchSession refuses to POST a launch ticket
// to any other origin (SEC-26) — the ticket is bearer-equivalent and must
// never leave the configured session host.
export const SESSION_ORIGIN_COOKIE = "tcdi_session_origin";

function readCookie(name: string): string | undefined {
  for (const part of document.cookie.split(";")) {
    const idx = part.indexOf("=");
    if (idx > 0 && part.slice(0, idx).trim() === name) {
      return decodeURIComponent(part.slice(idx + 1).trim());
    }
  }
  return undefined;
}

export function getCsrfToken(): string | undefined {
  return readCookie(CSRF_COOKIE);
}

export function getSessionOrigin(): string | undefined {
  return readCookie(SESSION_ORIGIN_COOKIE);
}

const csrfMiddleware: Middleware = {
  async onRequest({ request }) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      const token = getCsrfToken();
      if (token) request.headers.set(CSRF_HEADER, token);
    }
    return request;
  },
};

export function createApi(fetchImpl?: typeof fetch): ApiClient {
  const client = createClient<paths>({
    baseUrl:
      typeof window !== "undefined" ? window.location.origin : "http://localhost",
    credentials: "same-origin",
    ...(fetchImpl ? { fetch: fetchImpl } : {}),
  });
  client.use(csrfMiddleware);
  return client;
}

export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}

// openapi-fetch resolves with { data, error, response }; unwrap to data or
// throw a PortalApiError carrying the stable error code.
export function unwrap<T>(result: {
  data?: T;
  error?: unknown;
  response: Response;
}): T {
  if (result.error !== undefined || !result.response.ok) {
    const body =
      typeof result.error === "object" && result.error !== null
        ? (result.error as Record<string, unknown>)
        : {};
    throw new PortalApiError(result.response.status, {
      code: typeof body.code === "string" ? body.code : undefined,
      message: typeof body.message === "string" ? body.message : undefined,
      retryable: typeof body.retryable === "boolean" ? body.retryable : undefined,
      requestId: typeof body.requestId === "string" ? body.requestId : undefined,
    });
  }
  return result.data as T;
}
