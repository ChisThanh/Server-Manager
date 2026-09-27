import { create } from "zustand";
import { vi, type Messages } from "./vi";
import { en } from "./en";

import { MODULES } from "./modules";
import type { LangCode as Code } from "./define";

type ModuleMap = typeof MODULES;
/** Every key a feature module declares. */
export type ModuleKey = { [M in keyof ModuleMap]: keyof ModuleMap[M]["vi"] }[keyof ModuleMap];
export type Key = keyof Messages | ModuleKey;
export type Params = Record<string, string | number>;

type Flat = Record<string, string>;
// Vietnamese (default) and English (fallback) are bundled; every other
// language (core + module strings) is a separate chunk loaded when chosen.
const load = (core: () => Promise<Flat>, mods: () => Promise<{ default: Flat }>) => () =>
  Promise.all([core(), mods()]).then(([c, m]) => ({ ...c, ...m.default }));

export const LANGUAGES = [
  { code: "vi", name: "Tiếng Việt" },
  { code: "en", name: "English" },
  { code: "zh", name: "简体中文", load: load(() => import("./zh").then((m) => m.zh), () => import("./lang/zh")) },
  { code: "ja", name: "日本語", load: load(() => import("./ja").then((m) => m.ja), () => import("./lang/ja")) },
  { code: "ko", name: "한국어", load: load(() => import("./ko").then((m) => m.ko), () => import("./lang/ko")) },
  { code: "fr", name: "Français", load: load(() => import("./fr").then((m) => m.fr), () => import("./lang/fr")) },
  { code: "de", name: "Deutsch", load: load(() => import("./de").then((m) => m.de), () => import("./lang/de")) },
  { code: "es", name: "Español", load: load(() => import("./es").then((m) => m.es), () => import("./lang/es")) },
  { code: "pt", name: "Português", load: load(() => import("./pt").then((m) => m.pt), () => import("./lang/pt")) },
  { code: "ru", name: "Русский", load: load(() => import("./ru").then((m) => m.ru), () => import("./lang/ru")) },
] as const satisfies readonly { code: Code; name: string; load?: () => Promise<Flat> }[];

export type LangCode = Code;

const STORAGE_KEY = "sm.lang";

function initialLang(): LangCode {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved && LANGUAGES.some((l) => l.code === saved)) return saved as LangCode;
  } catch {
    /* storage unavailable */
  }
  return "vi"; // Vietnamese is the default
}

// Merged dictionaries (core + modules) per loaded language.
const merged = new Map<string, Flat>();
function bundled(code: "vi" | "en"): Flat {
  let d = merged.get(code);
  if (!d) {
    d = { ...((code === "vi" ? vi : en) as Flat) };
    for (const m of Object.values(MODULES) as Record<string, Flat>[]) Object.assign(d, m[code] ?? {});
    merged.set(code, d);
  }
  return d;
}

bundled("vi");
bundled("en");

/** Loads a language's dictionary (no-op for vi/en or when already loaded). */
async function ensure(code: LangCode): Promise<void> {
  if (code === "vi" || code === "en" || merged.has(code)) return;
  const l = LANGUAGES.find((x) => x.code === code);
  if (l && "load" in l) merged.set(code, await l.load());
}

export const useLang = create<{ lang: LangCode; setLang: (l: LangCode) => Promise<void> }>((set) => ({
  lang: "vi",
  setLang: async (lang) => {
    try {
      await ensure(lang);
    } catch {
      return; // chunk failed to load: keep the current language
    }
    try {
      localStorage.setItem(STORAGE_KEY, lang);
    } catch {
      /* ignore */
    }
    document.documentElement.lang = lang;
    set({ lang });
  },
}));

/** Loads the saved language before the first render (main.tsx awaits it). */
export async function initI18n() {
  const lang = initialLang();
  try {
    await ensure(lang);
    useLang.setState({ lang });
  } catch {
    useLang.setState({ lang: "vi" });
  }
  document.documentElement.lang = useLang.getState().lang;
}

function dict(): Flat {
  const code = useLang.getState().lang;
  return merged.get(code) ?? bundled("vi");
}

export function hasKey(key: string): key is Key {
  return key in bundled("vi");
}

/** Translates a key in the current language, filling {placeholders}. */
export function t(key: Key, params?: Params): string {
  let s = dict()[key] ?? bundled("en")[key] ?? bundled("vi")[key] ?? key;
  if (params) {
    for (const [k, v] of Object.entries(params)) s = s.split(`{${k}}`).join(String(v));
  }
  return s;
}

/** Hook form: re-renders the component when the language changes. */
export function useT() {
  useLang((s) => s.lang);
  return t;
}
