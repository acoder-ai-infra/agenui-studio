import { describe, expect, it } from 'vitest';

import {
  isThemeableRoute,
  resolveThemePreference,
  THEME_BOOTSTRAP_SCRIPT,
  THEME_COLORS,
} from './config';

describe('theme configuration', () => {
  it('limits theme switching to the admin workspace', () => {
    expect(isThemeableRoute('/admin')).toBe(true);
    expect(isThemeableRoute('/admin/home')).toBe(true);
    expect(isThemeableRoute('/administrator')).toBe(false);
    expect(isThemeableRoute('/')).toBe(false);
  });

  it('prefers an explicit choice and otherwise follows the system', () => {
    expect(resolveThemePreference('light', true)).toBe('light');
    expect(resolveThemePreference('dark', false)).toBe('dark');
    expect(resolveThemePreference(null, true)).toBe('dark');
    expect(resolveThemePreference('invalid', false)).toBe('light');
  });

  it('keeps the pre-hydration script aligned with root theme contracts', () => {
    expect(THEME_BOOTSTRAP_SCRIPT).toContain("classList.toggle('dark'");
    expect(THEME_BOOTSTRAP_SCRIPT).toContain('root.style.colorScheme');
    expect(THEME_BOOTSTRAP_SCRIPT).toContain('meta[name="theme-color"]');
    expect(THEME_BOOTSTRAP_SCRIPT).toContain(THEME_COLORS.dark);
  });
});
