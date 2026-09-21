'use client';

import { FormEvent, useEffect, useState } from 'react';
import { PackageOpen, RefreshCw } from 'lucide-react';
import { useLocale } from '@/lib/i18n/LocaleContext';

type PublicationTarget = {
  kind: 'callback' | 'mq';
  endpoint: string;
  enabled: boolean;
  configured?: boolean;
};

const endpoint = '/api/v1/agenui/admin/local/publication-target';
const field = 'h-10 w-full rounded-lg border border-neutral-200 bg-surface-raised px-3 text-[13px] text-neutral-800 outline-none transition-colors placeholder:text-neutral-500 focus:border-neutral-500 focus:ring-2 focus:ring-neutral-100';

export default function PublicationSettingsPage() {
  const { t } = useLocale();
  const [value, setValue] = useState<PublicationTarget>({
    kind: 'callback',
    endpoint: '',
    enabled: true,
  });
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const load = async () => {
    setError('');
    try {
      const response = await fetch(endpoint, { cache: 'no-store' });
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
      setValue({
        kind: body.kind === 'mq' ? 'mq' : 'callback',
        endpoint: body.endpoint || '',
        enabled: body.configured ? Boolean(body.enabled) : true,
        configured: Boolean(body.configured),
      });
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const save = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setBusy(true);
    setMessage('');
    setError('');
    try {
      const response = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(value),
      });
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
      setValue((current) => ({ ...current, configured: true }));
      setMessage(t('settings.publicationSaved'));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="max-w-[800px] p-6">
      <div className="flex items-start justify-between gap-5">
        <div>
          <h1 className="text-[20px] font-semibold text-neutral-900">{t('settings.publicationTitle')}</h1>
          <p className="mt-1 text-[13px] text-neutral-500">
            {t('settings.publicationIntro')}
          </p>
        </div>
        <button
          type="button"
          onClick={() => void load()}
          disabled={busy}
          className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-neutral-200 px-3 text-[12px] text-neutral-600 hover:bg-neutral-50 disabled:opacity-50"
        >
          <RefreshCw className="h-3 w-3" />{t('common.refresh')}
        </button>
      </div>

      <form id="publication-target" onSubmit={save} className="mt-5 overflow-hidden rounded-xl border border-neutral-200/70 bg-surface">
        <div className="border-b border-neutral-100 px-5 py-4">
          <div className="flex items-center gap-2 text-[13px] font-medium text-neutral-800">
            <PackageOpen className="h-4 w-4 text-neutral-500" />{t('settings.publicationSection')}
          </div>
          <p className="mt-1 text-[12px] leading-5 text-neutral-500">
            {t('settings.publicationSectionDesc')}
          </p>
        </div>

        <div className="grid gap-4 p-5">
          <label className="grid gap-1.5 text-[12px] font-medium text-neutral-700">
            {t('settings.deliveryType')}
            <select
              className={field}
              value={value.kind}
              onChange={(event) => setValue({ ...value, kind: event.target.value === 'mq' ? 'mq' : 'callback' })}
            >
              <option value="callback">HTTP Callback</option>
              <option value="mq">Message Queue Adapter</option>
            </select>
          </label>

          <label className="grid gap-1.5 text-[12px] font-medium text-neutral-700">
            {t('settings.targetAddress')}
            <input
              required
              className={field}
              placeholder={value.kind === 'callback' ? 'https://runtime.example.com/cards' : 'mq://topic-or-adapter-address'}
              value={value.endpoint}
              onChange={(event) => setValue({ ...value, endpoint: event.target.value })}
            />
          </label>

          <label className="inline-flex items-center gap-2 text-[12px] text-neutral-700">
            <input
              type="checkbox"
              checked={value.enabled}
              onChange={(event) => setValue({ ...value, enabled: event.target.checked })}
            />
            {t('settings.enableTarget')}
          </label>
        </div>

        <div className="flex justify-end border-t border-neutral-100 bg-neutral-50/60 px-5 py-3">
          <button disabled={busy} className="h-8 rounded-lg bg-inverse px-3.5 text-[12px] font-medium text-inverse-fg disabled:opacity-50">
            {busy ? t('common.saving') : t('settings.savePublication')}
          </button>
        </div>
      </form>

      {message && <p className="mt-3 rounded-lg bg-emerald-50 px-3 py-2 text-[12px] text-emerald-700">{message}</p>}
      {error && <p className="mt-3 rounded-lg bg-red-50 px-3 py-2 text-[12px] text-red-700">{error}</p>}
    </main>
  );
}
