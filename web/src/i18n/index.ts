import { en } from "./en";
import { vi } from "./vi";

export type Locale = "en" | "vi";
export const LOCALES: readonly Locale[] = ["en", "vi"] as const;
export type MessageKey = keyof typeof en;

// Stored language preference; read by detectLocale() and written by the
// language switch (app/locale.tsx).
export const LOCALE_STORAGE_KEY = "tcdi.lang";

// `en` is complete by definition; `vi` is checked for key parity by the
// catalog test and lint:strings. The Record type makes a missing vi key a
// compile error.
const catalogs: Record<Locale, Record<MessageKey, string>> = { en, vi };

// Language choice (E11): stored preference, else navigator.language
// starting with "vi", else English.
export function detectLocale(): Locale {
  try {
    const stored = window.localStorage.getItem(LOCALE_STORAGE_KEY);
    if (stored === "en" || stored === "vi") return stored;
  } catch {
    // Storage blocked (privacy mode): fall back to the browser language.
  }
  const nav = typeof navigator === "undefined" ? "" : (navigator.language ?? "");
  return nav.toLowerCase().startsWith("vi") ? "vi" : "en";
}

let active: Locale = detectLocale();

export function getLocale(): Locale {
  return active;
}

// Swaps the catalog t() reads and updates <html lang>. Screens re-render
// through the LocaleProvider remount in app/locale.tsx — t() itself is not
// reactive, so nothing else needs to subscribe.
export function setActiveLocale(locale: Locale): void {
  active = locale;
  if (typeof document !== "undefined") {
    document.documentElement.setAttribute("lang", active);
  }
}

// <html lang> before the first React commit, so assistive tech sees the
// detected language even during the initial load.
if (typeof document !== "undefined") {
  document.documentElement.setAttribute("lang", active);
}

// Looks up key in the active catalog (English is the fallback for a gap)
// and substitutes {name} placeholders from params. A placeholder with no
// matching param throws — a rendered "{name}" is always a bug, so fail
// loudly instead of showing it.
export function t(
  key: MessageKey,
  params?: Record<string, string | number>,
): string {
  const message = catalogs[active][key] ?? en[key];
  if (message === undefined) {
    throw new Error(`i18n: unknown message key "${key}"`);
  }
  return message.replace(/\{([^{}]+)\}/g, (_match, name: string) => {
    const value = params?.[name];
    if (value === undefined) {
      throw new Error(`i18n: missing param "${name}" for key "${key}"`);
    }
    return String(value);
  });
}

// Date and number formats follow the chosen UI language, not the browser
// locale (E11): a user who picked English sees English dates even on a vi-VN
// machine. Intl does the work; the helpers below are the only places the
// portal formats values.

/** "Sep 30, 2026" / "30 thg 9, 2026". */
export function formatDate(
  value: Date | number | string,
  options: Intl.DateTimeFormatOptions = { dateStyle: "medium" },
): string {
  return new Intl.DateTimeFormat(active, options).format(new Date(value));
}

/** Medium date + short time, matching the old toLocaleString() coverage. */
export function formatDateTime(
  value: Date | number | string,
  options: Intl.DateTimeFormatOptions = { dateStyle: "medium", timeStyle: "short" },
): string {
  return new Intl.DateTimeFormat(active, options).format(new Date(value));
}

/** Clock time only ("14:05" / "2:05 PM"). */
export function formatTime(
  value: Date | number | string,
  options: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit" },
): string {
  return new Intl.DateTimeFormat(active, options).format(new Date(value));
}

export function formatNumber(
  value: number,
  options?: Intl.NumberFormatOptions,
): string {
  return new Intl.NumberFormat(active, options).format(value);
}
