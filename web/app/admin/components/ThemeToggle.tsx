'use client';

import { Sun, Moon } from 'lucide-react';
import { cn } from '@/lib/utils';
import { useLocale } from '@/lib/i18n/LocaleContext';
import { useTheme } from '@/lib/theme/ThemeContext';

export function ThemeToggle({ className }: { className?: string }) {
  const { theme, toggleTheme } = useTheme();
  const { t } = useLocale();
  const dark = theme === 'dark';
  const label = dark ? t('theme.switchToLight') : t('theme.switchToDark');

  return (
    <button
      type="button"
      onClick={toggleTheme}
      title={label}
      aria-label={label}
      className={cn(
        'inline-flex items-center justify-center w-7 h-7 rounded-lg text-neutral-500 transition-colors',
        // 暗色 token 下 neutral-100/200 是深底色，文字需用高阶中性色（如 700）
        'hover:bg-neutral-100 hover:text-neutral-700 dark:hover:bg-white/10',
        className,
      )}
    >
      {dark ? <Moon aria-hidden="true" className="h-4 w-4" /> : <Sun aria-hidden="true" className="h-4 w-4" />}
    </button>
  );
}
