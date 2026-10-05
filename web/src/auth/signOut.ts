import type { ApiClient } from "../api/client";
import { unwrap } from "../api/client";
import { LOGIN_URL } from "./AuthGate";

// Public landing page after sign-out (served by the SPA fallback). It never
// starts a login on its own: the identity provider session may still be
// alive, and an automatic login would sign the user straight back in.
export const SIGNED_OUT_PATH = "/signed-out";

/** Where "Sign in again" goes — an explicit, user-initiated login. */
export const SIGN_IN_AGAIN_URL = `${LOGIN_URL}?returnTo=${encodeURIComponent("/")}`;

/** Only an absolute http(s) URL is a navigation target worth following. */
function safeEndSessionUrl(raw: unknown): string | null {
  if (typeof raw !== "string") return null;
  try {
    const u = new URL(raw);
    return u.protocol === "https:" || u.protocol === "http:" ? u.toString() : null;
  } catch {
    return null;
  }
}

/**
 * POST /v1/logout (CSRF header is added by the API client), then leave the
 * app: to the identity provider's end-session URL when the backend returned
 * one (RP-initiated logout, so the provider session ends too), otherwise to
 * the signed-out page. A 401 means the session was already gone — also the
 * signed-out page. Any other failure rejects so the caller can say so.
 *
 * `assign` is injectable for tests; production navigates the browser.
 */
export async function signOut(
  api: ApiClient,
  assign: (url: string) => void = (url) => window.location.assign(url),
): Promise<void> {
  await signOutRequest(api, "/v1/logout", assign);
}

/**
 * POST /v1/me/sessions:revoke-all — sign out everywhere (ADR 0007): every
 * session of the principal in the current tenant dies, including this
 * browser's. Navigation is identical to signOut.
 */
export async function signOutEverywhere(
  api: ApiClient,
  assign: (url: string) => void = (url) => window.location.assign(url),
): Promise<void> {
  await signOutRequest(api, "/v1/me/sessions:revoke-all", assign);
}

async function signOutRequest(
  api: ApiClient,
  path: "/v1/logout" | "/v1/me/sessions:revoke-all",
  assign: (url: string) => void,
): Promise<void> {
  const result = await api.POST(path);
  if (result.response.status === 401) {
    assign(SIGNED_OUT_PATH);
    return;
  }
  const data = unwrap(result); // throws PortalApiError on any other failure
  const endSession = result.response.status === 200 ? safeEndSessionUrl(data?.endSessionUrl) : null;
  assign(endSession ?? SIGNED_OUT_PATH);
}
