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
        // Session probe: GET /v1/me is the first /v1 call of every load. Its
        // 401 is the normal "signed out" answer, handled below — never logged
        // or rethrown (MeProvider reads the body again once the gate opens).
        unwrap(await api.GET("/v1/me"));
        if (!cancelled) setReady(true);
      } catch (e) {
        if (isPortalApiError(e) && e.code === "UNAUTHENTICATED") {
          if (!cancelled) onUnauthenticated();
          return;
        }
        // A backend without /v1/me yet: MeProvider runs on its stub principal.
        if (isPortalApiError(e) && (e.httpStatus === 404 || e.httpStatus === 501)) {
          if (!cancelled) setReady(true);
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
