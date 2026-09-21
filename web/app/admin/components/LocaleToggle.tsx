'use client';

import { Languages } from 'lucide-react';
import { cn } from '@/lib/utils';
import { useLocale } from '@/lib/i18n/LocaleContext';

/**
 * 显示当前生效的语言，tooltip 说明点一下会切到哪种语言。
 * 图标旁保留文字标签：看不懂当前界面语言的人也能找到切回去的入口。
 */
export function LocaleToggle({ className }: { className?: string }) {
  const { locale, toggleLocale, t } = useLocale();
  const label = t('locale.toggle');

  return (
    <button
      type="button"
      onClick={toggleLocale}
      title={label}
      aria-label={label}
      className={cn(
        'inline-flex items-center gap-1 h-7 px-1.5 rounded-lg text-neutral-500 transition-colors',
        // 与 ThemeToggle 一致：暗色 token 下 neutral-100/200 是深底色，文字需用高阶中性色
        'hover:bg-neutral-100 hover:text-neutral-700 dark:hover:bg-white/10',
        className,
      )}
    >
      <Languages aria-hidden="true" className="w-4 h-4" />
      <span className="text-[11px] font-medium leading-none">
        {locale === 'zh' ? '中' : 'EN'}
      </span>
    </button>
  );
}
