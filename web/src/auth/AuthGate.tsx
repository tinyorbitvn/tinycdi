import { useEffect, useState, type ReactNode } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";

// Login lives on the portal origin ahead of the API (OIDC-backed session
// bootstrap). The contract has no auth endpoints; the server redirects
// unauthenticated browsers here, and the portal does the same proactively.
export const LOGIN_URL = "/v1/login";

export function loginUrl(returnTo: string): string {
  return `${LOGIN_URL}?returnTo=${encodeURIComponent(returnTo)}`;
}

// Injectable for tests; production navigates the browser to the login flow.
export function defaultLoginRedirect(): void {
  window.location.assign(
    loginUrl(window.location.pathname + window.location.search),
  );
}

const MAX_BACKOFF_MS = 30_000;

export function AuthGate({
  children,
  onUnauthenticated = defaultLoginRedirect,
}: {
  children: ReactNode;
  onUnauthenticated?: () => void;
}) {
  const api = useApi();
  const [ready, setReady] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [retrying, setRetrying] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let failures = 0;
    const attempt = async () => {
      try {
        // Passive probe first: GET /v1/session answers 200 {authenticated}
        // for signed-out callers too, so an anonymous load issues no failed
        // request (the browser logs those to the console, whatever the page
        // does with them). Only a live session goes on to GET /v1/me.
        const probe = unwrap(await api.GET("/v1/session"));
        if (!probe.authenticated) {
          if (!cancelled) onUnauthenticated();
          return;
        }
        // MeProvider reads the body again once the gate opens.
        unwrap(await api.GET("/v1/me"));
        if (!cancelled) setReady(true);
      } catch (e) {
        if (cancelled) return;
        // The session can lapse between the probe and /v1/me.
        if (isPortalApiError(e) && e.code === "UNAUTHENTICATED") {
          onUnauthenticated();
          return;
        }
        // Retryable failures — 503 UNAVAILABLE during a DB outage, 429,
        // network errors — are never a sign-out and never a dead end: back
        // off (honouring Retry-After) and probe again (V3.27).
        if (isPortalApiError(e) && !e.retryable) {
          setError(e instanceof Error ? e.message : String(e));
          return;
        }
        failures += 1;
        setRetrying(e instanceof Error ? e.message : String(e));
        const backoff = Math.min(1_000 * 2 ** failures, MAX_BACKOFF_MS);
        const delay = Math.max(
          isPortalApiError(e) ? (e.retryAfterMs ?? 0) : 0,
          backoff,
        );
        timer = setTimeout(() => void attempt(), delay);
      }
    };
    void attempt();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [api, onUnauthenticated]);

  if (error) {
    return (
      <main>
        <p role="alert">{t("auth.gate.apiUnreachable", { error })}</p>
      </main>
    );
  }
  if (!ready) {
    return (
      <main aria-busy="true">
        {retrying === null ? (
          t("auth.gate.checking")
        ) : (
          <p role="status">{t("auth.gate.retrying", { error: retrying })}</p>
        )}
      </main>
    );
  }
  return <>{children}</>;
}
