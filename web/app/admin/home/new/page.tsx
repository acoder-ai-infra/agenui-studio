'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { useLocale } from '@/lib/i18n/LocaleContext';

export default function CreatePage() {
  const router = useRouter();
  const { t } = useLocale();
  const [prompt, setPrompt] = useState('');

  const start = () => {
    if (!prompt.trim()) {
      return;
    }
    router.push(
      `/admin/home?session=new&prompt=${encodeURIComponent(prompt.trim())}`,
    );
  };

  return (
    <div className="p-6 max-w-[720px]">
      <h1 className="text-[20px] font-semibold text-neutral-900">{t('newCard.title')}</h1>
      <p className="text-[13px] text-neutral-500 mt-1">
        {t('newCard.intro')}
      </p>
      <div className="rounded-xl border border-neutral-200/70 bg-surface p-5 mt-4">
        <textarea
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          placeholder={t('newCard.placeholder')}
          className="min-h-[140px] w-full rounded-lg border border-neutral-200 bg-surface-raised p-3 text-[13px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
        />
        <div className="flex items-center gap-3 mt-3">
          <button
            disabled={!prompt.trim()}
            onClick={start}
            className="h-9 rounded-lg bg-inverse px-4 text-[13px] font-medium text-inverse-fg hover:bg-inverse-hover disabled:opacity-40"
          >
            {t('newCard.open')}
          </button>
          <span className="text-[12px] text-neutral-400">
            {t('newCard.hint')}
          </span>
        </div>
      </div>
    </div>
  );
}
