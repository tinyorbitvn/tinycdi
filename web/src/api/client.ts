import createClient, { type Client, type Middleware } from "openapi-fetch";
import type { paths } from "./generated/schema";
import { PortalApiError, type ErrorDetails } from "./errors";

export type ApiClient = Client<paths>;

// The CSRF token is derived server-side and returned in the GET /v1/me body
// (P1/D17). It lives only in module state — installed by web/src/app/me.tsx
// via setCsrfToken — and is echoed on every state-changing request. No
// export reads document.cookie; the tcdi_csrf cookie is gone in v0.2.
export const CSRF_HEADER = "X-CSRF-Token";

let csrfToken: string | undefined;

export function setCsrfToken(token: string | undefined): void {
  csrfToken = token;
}

function csrfMiddleware(fetchImpl: typeof fetch, baseUrl: string): Middleware {
  // The request body is consumed by fetch(); keep a clone per request so a
  // CSRF_FAILED retry can replay it verbatim.
  const clones = new WeakMap<Request, Request>();
  return {
    async onRequest({ request }) {
      if (request.method !== "GET" && request.method !== "HEAD") {
        if (csrfToken) request.headers.set(CSRF_HEADER, csrfToken);
        clones.set(request, request.clone());
      }
      return request;
    },
    async onResponse({ request, response }) {
      if (response.status !== 403) return response;
      const retry = clones.get(request);
      if (!retry) return response; // already retried, or a safe method
      let code: string | undefined;
      try {
        const body = (await response.clone().json()) as Record<string, unknown>;
        code = typeof body?.code === "string" ? body.code : undefined;
      } catch {
        return response;
      }
      if (code !== "CSRF_FAILED") return response;
      clones.delete(request); // one retry only
      // The session likely rotated: refresh the token from /v1/me once,
      // then replay the request. A second 403 surfaces to the caller.
      try {
        const me = await fetchImpl(new URL("/v1/me", baseUrl), {
          credentials: "same-origin",
          headers: { Accept: "application/json" },
        });
        if (me.ok) {
          const body = (await me.json()) as Record<string, unknown>;
          if (typeof body.csrfToken === "string") setCsrfToken(body.csrfToken);
        }
      } catch {
        /* fall through and surface the original 403's retry */
      }
      if (csrfToken) retry.headers.set(CSRF_HEADER, csrfToken);
      return fetchImpl(retry);
    },
  };
}

export function createApi(fetchImpl?: typeof fetch): ApiClient {
  const baseUrl =
    typeof window !== "undefined" ? window.location.origin : "http://localhost";
  const impl = fetchImpl ?? ((input: RequestInfo | URL, init?: RequestInit) => fetch(input, init));
  const client = createClient<paths>({
    baseUrl,
    credentials: "same-origin",
    fetch: impl,
  });
  client.use(csrfMiddleware(impl, baseUrl));
  return client;
}

export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}

// openapi-fetch resolves with { data, error, response }; unwrap to data or
// throw a PortalApiError carrying the stable error code.
// Retry-After carries seconds or an HTTP date; anything else is ignored.
function retryAfterMs(response: Response): number | undefined {
  const raw = response.headers.get("retry-after");
  if (!raw) return undefined;
  const seconds = Number(raw);
  if (Number.isFinite(seconds)) return Math.max(0, Math.round(seconds * 1000));
  const at = Date.parse(raw);
  return Number.isNaN(at) ? undefined : Math.max(0, at - Date.now());
}

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
    throw new PortalApiError(
      result.response.status,
      {
        code: typeof body.code === "string" ? body.code : undefined,
        message: typeof body.message === "string" ? body.message : undefined,
        retryable: typeof body.retryable === "boolean" ? body.retryable : undefined,
        requestId: typeof body.requestId === "string" ? body.requestId : undefined,
        details:
          typeof body.details === "object" && body.details !== null
            ? (body.details as ErrorDetails)
            : undefined,
      },
      retryAfterMs(result.response),
    );
  }
  return result.data as T;
}
