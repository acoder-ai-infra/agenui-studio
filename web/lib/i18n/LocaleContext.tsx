'use client';

import {
  createContext,
  useCallback,
  useContext,
  useLayoutEffect,
  useMemo,
  useSyncExternalStore,
} from 'react';

import {
  format,
  messages,
  type Locale,
  type MessageKey,
  type MessageVars,
} from './messages';

export type { Locale, MessageKey } from './messages';

export const LOCALE_STORAGE_KEY = 'agenui-locale';

/** `<html lang>` values. Kept apart from the catalog keys, which stay short. */
const HTML_LANG: Record<Locale, string> = { zh: 'zh-CN', en: 'en' };

/* ── Preference: localStorage as an external store ──────────────────────── */

const listeners = new Set<() => void>();
let cachedLocale: Locale | null = null;

/** Only reflects an explicit user choice; null hands over to the browser. */
function readStoredLocale(): Locale | null {
  try {
    const value = localStorage.getItem(LOCALE_STORAGE_KEY);
    return value === 'zh' || value === 'en' ? value : null;
  } catch {
    // localStorage throws in private mode
    return null;
  }
}

function getBrowserLocale(): Locale {
  try {
    return navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en';
  } catch {
    return 'zh';
  }
}

/**
 * Only in-page switches are subscribed. `storage` events are ignored on purpose:
 * re-labelling a console mid-edit from another tab is disorienting, and a
 * hand-edited DevTools value should not drive rendering. A change made in
 * another tab takes effect on the next reload.
 */
function subscribeLocale(cb: () => void) {
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
  };
}

function getLocaleSnapshot(): Locale {
  // Resolved once per page load, then advanced only by writeLocale.
  if (cachedLocale === null) {
    cachedLocale = readStoredLocale() ?? getBrowserLocale();
  }
  return cachedLocale;
}

/**
 * Must match the `lang` rendered by the root layout so the server HTML is
 * self-consistent. When the client resolves a different locale,
 * useSyncExternalStore re-renders right after hydration instead of warning.
 */
function getLocaleServerSnapshot(): Locale {
  return 'zh';
}

function writeLocale(next: Locale) {
  cachedLocale = next;
  try {
    localStorage.setItem(LOCALE_STORAGE_KEY, next);
  } catch {
    // Session-only when localStorage is unavailable
  }
  listeners.forEach((cb) => cb());
}

/* ── Provider ───────────────────────────────────────────────────────────── */

/** Looks up a message in the active locale and fills `{name}` placeholders. */
export type Translate = (key: MessageKey, vars?: MessageVars) => string;

interface LocaleContextValue {
  locale: Locale;
  setLocale: (next: Locale) => void;
  toggleLocale: () => void;
  t: Translate;
}

const LocaleContext = createContext<LocaleContextValue | null>(null);

export function LocaleProvider({ children }: { children: React.ReactNode }) {
  const locale = useSyncExternalStore(
    subscribeLocale,
    getLocaleSnapshot,
    getLocaleServerSnapshot,
  );

  // Keep <html lang> truthful for screen readers, spell checking and
  // per-language font stacks. Layout effect avoids a post-paint attribute flip.
  useLayoutEffect(() => {
    document.documentElement.lang = HTML_LANG[locale];
  }, [locale]);

  const setLocale = useCallback((next: Locale) => {
    writeLocale(next);
  }, []);

  const toggleLocale = useCallback(() => {
    writeLocale(getLocaleSnapshot() === 'zh' ? 'en' : 'zh');
  }, []);

  const t = useCallback<Translate>(
    (key, vars) => format(messages[locale][key], vars),
    [locale],
  );

  const value = useMemo(
    () => ({ locale, setLocale, toggleLocale, t }),
    [locale, setLocale, toggleLocale, t],
  );

  return <LocaleContext.Provider value={value}>{children}</LocaleContext.Provider>;
}

export function useLocale(): LocaleContextValue {
  const ctx = useContext(LocaleContext);
  if (!ctx) throw new Error('useLocale must be used within LocaleProvider');
  return ctx;
}
