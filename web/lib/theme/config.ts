export type Theme = 'light' | 'dark';

export const THEME_STORAGE_KEY = 'agenui-theme';

export const THEME_COLORS: Record<Theme, string> = {
  light: '#FFFFFF',
  dark: '#0F1218',
};

/** Studio currently exposes theme switching only inside the admin workspace. */
export function isThemeableRoute(pathname: string): boolean {
  return pathname === '/admin' || pathname.startsWith('/admin/');
}

export function resolveThemePreference(
  storedTheme: string | null,
  systemPrefersDark: boolean,
): Theme {
  if (storedTheme === 'light' || storedTheme === 'dark') {
    return storedTheme;
  }
  return systemPrefersDark ? 'dark' : 'light';
}

/**
 * Runs before React hydrates so the first painted frame already has the right
 * root class, native-control color scheme, and browser chrome color.
 */
export const THEME_BOOTSTRAP_SCRIPT = `(() => {
  try {
    const root = document.documentElement;
    const themeable = location.pathname === '/admin' || location.pathname.startsWith('/admin/');
    const stored = localStorage.getItem('${THEME_STORAGE_KEY}');
    const theme = stored === 'light' || stored === 'dark'
      ? stored
      : (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    const dark = themeable && theme === 'dark';
    root.classList.toggle('dark', dark);
    root.style.colorScheme = dark ? 'dark' : 'light';
    let meta = document.querySelector('meta[name="theme-color"]');
    if (!meta) {
      meta = document.createElement('meta');
      meta.setAttribute('name', 'theme-color');
      document.head.appendChild(meta);
    }
    meta.setAttribute('content', dark ? '${THEME_COLORS.dark}' : '${THEME_COLORS.light}');
  } catch (_) {}
})();`;
