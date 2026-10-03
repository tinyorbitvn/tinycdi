import type { ReactElement } from "react";
import { render } from "@testing-library/react";
import {
  createMockApi,
  SESSION_COOKIE,
  SESSION_PRINCIPAL,
  CSRF_COOKIE,
  CSRF_TOKEN_VALUE,
  SESSION_ORIGIN_COOKIE,
  type MockRequest,
} from "../mock-api/handler.ts";
import { createApi, setCsrfToken } from "../../src/api/client";
import { ApiProvider } from "../../src/api/context";
import { ToastProvider } from "../../src/design";

export {
  createMockApi,
  CSRF_TOKEN_VALUE,
  SESSION_COOKIE,
  SESSION_PRINCIPAL,
  CSRF_COOKIE,
  SESSION_ORIGIN_COOKIE,
};

// The mock API's session origin — mirrors createMockApi's default and the
// value the mock login flow publishes in SESSION_ORIGIN_COOKIE.
export const MOCK_SESSION_ORIGIN = "http://127.0.0.1:4311";

// Turns the shared mock-api handler into a fetch() implementation. Cookies
// come from jsdom's document.cookie, like a real browser jar.
export function stubFetch(api = createMockApi()): typeof fetch {
  const impl = async (input: RequestInfo | URL): Promise<Response> => {
    const req = input instanceof Request ? input : new Request(String(input));
    const url = new URL(req.url);
    const headers: Record<string, string> = {};
    req.headers.forEach((v, k) => {
      headers[k.toLowerCase()] = v;
    });
    headers.cookie = document.cookie;
    const rawBody = await req.text();
    let body: Record<string, unknown> | undefined;
    try {
      body = rawBody ? (JSON.parse(rawBody) as Record<string, unknown>) : undefined;
    } catch {
      body = undefined;
    }
    const mreq: MockRequest = {
      method: req.method,
      path: url.pathname,
      query: url.searchParams,
      headers,
      rawBody,
      ...(body !== undefined ? { body } : {}),
    };
    const resp = api.handle(mreq);
    // Null-body statuses (204/205/304) must not carry a body, even an empty one.
    const nullBody = resp.status === 204 || resp.status === 205 || resp.status === 304;
    return new Response(
      nullBody ? null : typeof resp.body === "string" ? resp.body : JSON.stringify(resp.body ?? ""),
      { status: resp.status, headers: resp.headers as HeadersInit },
    );
  };
  return impl as typeof fetch;
}

export function loginCookies(): void {
  document.cookie = `${SESSION_COOKIE}=${SESSION_PRINCIPAL}; path=/`;
  // v0.2 (P1/D17): the CSRF token and session domain come from GET /v1/me
  // after login — model that bootstrap by installing the published token.
  setCsrfToken(CSRF_TOKEN_VALUE);
}

export function clearCookies(): void {
  for (const name of [SESSION_COOKIE, CSRF_COOKIE, SESSION_ORIGIN_COOKIE]) {
    document.cookie = `${name}=; path=/; expires=Thu, 01 Jan 1970 00:00:00 GMT`;
  }
}

export function renderWithApi(ui: ReactElement, api = createMockApi()) {
  const client = createApi(stubFetch(api));
  const utils = render(
    <ApiProvider client={client}>
      <ToastProvider>{ui}</ToastProvider>
    </ApiProvider>,
  );
  return { api, client, ...utils };
}
