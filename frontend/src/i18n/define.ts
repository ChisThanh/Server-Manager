// Language list and the helper feature modules use to declare their strings.
// Kept free of imports so module dictionaries can depend on it.

export const LANG_CODES = ["vi", "en", "zh", "ja", "ko", "fr", "de", "es", "pt", "ru"] as const;
export type LangCode = (typeof LANG_CODES)[number];

type Dict<T> = { [K in keyof T]: string };

/**
 * Declares a feature module's strings. `vi` is the source of truth (and the
 * default language) and `en` the fallback; both ship in the main bundle.
 * The other languages live in i18n/lang/<code>.ts (loaded on demand), where
 * TypeScript enforces that every module key is translated.
 */
export function defineModule<T extends Record<string, string>>(m: { vi: T; en: Dict<T> }) {
  return m;
}

/** @deprecated same as defineModule. */
export const draftModule = defineModule;
