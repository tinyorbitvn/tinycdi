// App branding. The frontend server serves an optional branding directory at
// /branding/ (operator-supplied ConfigMap); without it /branding/branding.json
// answers 200 with the empty object, which yields the defaults like any other
// failure — 404, non-JSON, network error.

export interface Branding {
  productName: string;
  /** Same-origin logo path (light theme), or null. */
  logo: string | null;
  /** Dark-theme logo variant; falls back to `logo`. */
  logoDark: string | null;
}

export const DEFAULT_BRANDING: Branding = {
  productName: "TinyCDI",
  logo: "/brand/tinyorbit-mark.svg",
  logoDark: "/brand/tinyorbit-mark-dark.svg",
};

// Only same-origin paths under /branding/ are accepted for logos — an
// absolute or scheme-relative URL could point at a foreign origin.
const LOGO_PREFIX = "/branding/";

function logo(value: unknown, fallback: string | null): string | null {
  return typeof value === "string" && value.startsWith(LOGO_PREFIX) ? value : fallback;
}

/** Field-level sanitizing: an invalid field falls back on its own. */
export function parseBranding(body: unknown): Branding {
  const b = (typeof body === "object" && body !== null ? body : {}) as Record<string, unknown>;
  const lg = logo(b.logo, null);
  return {
    productName:
      typeof b.productName === "string" && b.productName.trim() !== ""
        ? b.productName
        : DEFAULT_BRANDING.productName,
    // The shipped dark-theme mark is paired with the shipped light mark only:
    // an operator logo without a dark variant is used in both themes.
    logo: lg ?? DEFAULT_BRANDING.logo,
    logoDark: logo(b.logoDark, lg ?? DEFAULT_BRANDING.logoDark),
  };
}

export async function loadBranding(fetchImpl: typeof fetch = fetch): Promise<Branding> {
  try {
    const res = await fetchImpl("/branding/branding.json", {
      credentials: "same-origin",
      headers: { Accept: "application/json" },
    });
    if (!res.ok) return DEFAULT_BRANDING;
    if (!(res.headers.get("content-type") ?? "").includes("json")) return DEFAULT_BRANDING;
    return parseBranding(await res.json());
  } catch {
    return DEFAULT_BRANDING;
  }
}
