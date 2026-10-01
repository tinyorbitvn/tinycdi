import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

// Theme preference: "light" / "dark" are forced via <html data-theme>;
// "system" resolves through prefers-color-scheme. The attribute always
// carries the resolved theme — the Orbit sheets key dark off
// [data-theme="dark"] only, so theme-init.js does the same before paint.

export type ThemePreference = "light" | "dark" | "system";
export const THEME_STORAGE_KEY = "tcdi.theme";

export function readStoredTheme(): ThemePreference {
  try {
    const v = window.localStorage.getItem(THEME_STORAGE_KEY);
    if (v === "light" || v === "dark" || v === "system") return v;
  } catch {
    // Storage blocked (privacy mode): fall back to the OS preference.
  }
  return "system";
}

export function applyTheme(resolved: "light" | "dark"): void {
  document.documentElement.setAttribute("data-theme", resolved);
}

function systemPrefersDark(): boolean {
  return typeof window.matchMedia === "function" && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

interface ThemeContextValue {
  preference: ThemePreference;
  /** The theme actually shown. */
  resolved: "light" | "dark";
  setPreference: (pref: ThemePreference) => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [preference, setPref] = useState<ThemePreference>(readStoredTheme);
  const [systemDark, setSystemDark] = useState(systemPrefersDark);

  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => setSystemDark(mq.matches);
    mq.addEventListener?.("change", onChange);
    return () => mq.removeEventListener?.("change", onChange);
  }, []);

  const resolved = preference === "system" ? (systemDark ? "dark" : "light") : preference;
  useEffect(() => applyTheme(resolved), [resolved]);

  const setPreference = useCallback((pref: ThemePreference) => {
    setPref(pref);
    try {
      window.localStorage.setItem(THEME_STORAGE_KEY, pref);
    } catch {
      // Not persisted; still applied for this page view.
    }
  }, []);

  const value = useMemo<ThemeContextValue>(
    () => ({ preference, resolved, setPreference }),
    [preference, resolved, setPreference],
  );
  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used inside <ThemeProvider>");
  return ctx;
}
