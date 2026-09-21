'use client';

import {
  createContext,
  useCallback,
  useContext,
  useLayoutEffect,
  useMemo,
  useSyncExternalStore,
} from 'react';
import { usePathname } from 'next/navigation';
import {
  isThemeableRoute,
  resolveThemePreference,
  THEME_COLORS,
  THEME_STORAGE_KEY,
  type Theme,
} from './config';

export { isThemeableRoute, THEME_STORAGE_KEY, type Theme } from './config';

/* ── 偏好设置：localStorage 外部存储 ─────────────────────────────────────── */

const listeners = new Set<() => void>();
let cachedTheme: Theme | null = null;

/** 只反映用户手动选择过的主题；从未选择过时返回 null，交给系统主题兜底。 */
function readStoredTheme(): Theme | null {
  try {
    const value = localStorage.getItem(THEME_STORAGE_KEY);
    return value === 'light' || value === 'dark' ? value : null;
  } catch {
    // 隐私模式下 localStorage 会抛错
    return null;
  }
}

function getSystemTheme(): Theme {
  try {
    return resolveThemePreference(
      null,
      window.matchMedia('(prefers-color-scheme: dark)').matches,
    );
  } catch {
    return 'light';
  }
}

function subscribeTheme(cb: () => void) {
  listeners.add(cb);

  const media = window.matchMedia('(prefers-color-scheme: dark)');
  const syncPreference = () => {
    const next = readStoredTheme() ?? getSystemTheme();
    if (next === cachedTheme) return;
    cachedTheme = next;
    listeners.forEach((listener) => listener());
  };
  window.addEventListener('storage', syncPreference);
  media.addEventListener('change', syncPreference);

  return () => {
    listeners.delete(cb);
    window.removeEventListener('storage', syncPreference);
    media.removeEventListener('change', syncPreference);
  };
}

function getThemeSnapshot(): Theme {
  if (cachedTheme === null) {
    cachedTheme = readStoredTheme() ?? getSystemTheme();
  }
  return cachedTheme;
}

function getThemeServerSnapshot(): Theme {
  return 'light';
}

function writeTheme(next: Theme) {
  cachedTheme = next;
  try {
    localStorage.setItem(THEME_STORAGE_KEY, next);
  } catch {
    // localStorage 不可用时仅在当前会话生效
  }
  listeners.forEach((cb) => cb());
}

/* ── Provider ───────────────────────────────────────────────────────────── */

interface ThemeContextValue {
  /** 用户手动选择过就用它；从未选择过则取页面加载时的系统主题。 */
  theme: Theme;
  /** 当前路由是否支持暗色 */
  themeable: boolean;
  setTheme: (next: Theme) => void;
  toggleTheme: () => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const themeable = isThemeableRoute(pathname ?? '');

  const theme = useSyncExternalStore(
    subscribeTheme,
    getThemeSnapshot,
    getThemeServerSnapshot,
  );

  const dark = themeable && theme === 'dark';

  useLayoutEffect(() => {
    const root = document.documentElement;
    root.classList.toggle('dark', dark);
    root.style.colorScheme = dark ? 'dark' : 'light';
    const themeColor = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]');
    themeColor?.setAttribute('content', dark ? THEME_COLORS.dark : THEME_COLORS.light);
  }, [dark]);

  const setTheme = useCallback((next: Theme) => {
    writeTheme(next);
  }, []);

  const toggleTheme = useCallback(() => {
    writeTheme(getThemeSnapshot() === 'dark' ? 'light' : 'dark');
  }, []);

  const value = useMemo(
    () => ({ theme, themeable, setTheme, toggleTheme }),
    [theme, themeable, setTheme, toggleTheme],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error('useTheme 必须在 ThemeProvider 内使用');
  return ctx;
}
