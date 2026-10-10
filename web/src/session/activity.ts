import { useCallback, useEffect, useRef } from "react";
import { usePathname } from "../app/router";
import type { ApiClient } from "../api/client";

// FIX-IDLE: the portal's read surface is passive server-side — a polling
// loop can never extend the idle window — so real user interaction is what
// keeps a session alive. This hook turns interaction into an explicit
// authenticated beat: POST /v1/session:touch on pointer, key and route
// events, throttled to one call per SESSION_TOUCH_INTERVAL_MS. The beat is
// a real mutation (cookie + CSRF, same bar as any other write); timer
// polls and background fetches never send it.
export const SESSION_TOUCH_INTERVAL_MS = 60_000;

/**
 * Sends the activity beat at most once per interval. The beat is
 * fire-and-forget — a failed call (401 mid-sign-out, transient 503) is
 * ignored; the rest of the app surfaces expiry via its normal requests.
 * State lives on refs so repeat mounts throttle independently.
 */
export function useActivityTouch(api: ApiClient): void {
  const lastSentAt = useRef(-Infinity);
  const inFlight = useRef(false);
  const pathname = usePathname();
  const prevPath = useRef(pathname);

  const beat = useCallback(() => {
    const now = Date.now();
    if (inFlight.current || now - lastSentAt.current < SESSION_TOUCH_INTERVAL_MS) return;
    lastSentAt.current = now;
    inFlight.current = true;
    void api
      .POST("/v1/session:touch", {})
      .catch(() => {})
      .finally(() => {
        inFlight.current = false;
      });
  }, [api]);

  useEffect(() => {
    const opts = { passive: true };
    window.addEventListener("pointerdown", beat, opts);
    window.addEventListener("keydown", beat, opts);
    return () => {
      window.removeEventListener("pointerdown", beat);
      window.removeEventListener("keydown", beat);
    };
  }, [beat]);

  // Navigating is interaction: a pathname change beats once per interval.
  useEffect(() => {
    if (prevPath.current === pathname) return;
    prevPath.current = pathname;
    beat();
  }, [pathname, beat]);
}
