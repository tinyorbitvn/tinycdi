import {
  createContext,
  Fragment,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  getLocale,
  LOCALE_STORAGE_KEY,
  setActiveLocale,
  type Locale,
} from "../i18n";

// Language preference: stored under tcdi.lang, else navigator.language,
// else English (E11). Detection ran in i18n/index.ts at module load; this
// provider only owns *switching*.

export interface LocaleContextValue {
  locale: Locale;
  /** Persist + apply a new UI language. */
  setLocale: (locale: Locale) => void;
}

const LocaleContext = createContext<LocaleContextValue | null>(null);

export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setState] = useState<Locale>(getLocale);

  const setLocale = useCallback((next: Locale) => {
    try {
      window.localStorage.setItem(LOCALE_STORAGE_KEY, next);
    } catch {
      // Storage blocked: still applied for this page view.
    }
    setActiveLocale(next);
    setState(next);
  }, []);

  const value = useMemo<LocaleContextValue>(
    () => ({ locale, setLocale }),
    [locale, setLocale],
  );

  // Remount the subtree on switch: t() reads a module-level catalog, so
  // context alone would only re-render consumers of this context — not the
  // ~60 files that call t() directly. A keyed Fragment forces a full
  // re-render without adding a DOM node.
  return (
    <LocaleContext.Provider value={value}>
      <Fragment key={locale}>{children}</Fragment>
    </LocaleContext.Provider>
  );
}

export function useLocale(): LocaleContextValue {
  const ctx = useContext(LocaleContext);
  if (!ctx) throw new Error("useLocale must be used inside <LocaleProvider>");
  return ctx;
}
