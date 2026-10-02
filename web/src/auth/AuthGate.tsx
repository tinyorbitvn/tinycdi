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

  useEffect(() => {
    let cancelled = false;
    (async () => {
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
        // The session can lapse between the probe and /v1/me.
        if (isPortalApiError(e) && e.code === "UNAUTHENTICATED") {
          if (!cancelled) onUnauthenticated();
          return;
        }
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, onUnauthenticated]);

  if (error) {
    return (
      <main>
        <p role="alert">{t("auth.gate.apiUnreachable", { error })}</p>
      </main>
    );
  }
  if (!ready) return <main aria-busy="true">{t("auth.gate.checking")}</main>;
  return <>{children}</>;
}
