import { en } from "./en";

export type MessageKey = keyof typeof en;

const catalog: Record<string, string | undefined> = en;

// Looks up key in the English catalog and substitutes {name} placeholders
// from params. A placeholder with no matching param throws — a rendered
// "{name}" is always a bug, so fail loudly instead of showing it.
export function t(
  key: MessageKey,
  params?: Record<string, string | number>,
): string {
  const message = catalog[key];
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
