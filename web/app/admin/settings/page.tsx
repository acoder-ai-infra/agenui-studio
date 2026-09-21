'use client';

import { FormEvent, useEffect, useState } from 'react';
import { AlertTriangle, Check, KeyRound, RefreshCw, RotateCcw, ShieldCheck } from 'lucide-react';
import { useLocale, type MessageKey } from '@/lib/i18n/LocaleContext';
import {
  hasModelConnectionChanged,
  modelConfigFromResponse,
  requiresModelAPIKey,
  switchModelProtocol,
  type ModelConfig,
  type ModelProtocol,
} from '@/lib/model-config';

const endpoint = '/api/v1/agenui/admin/local/model';
const restartEndpoint = '/api/v1/agenui/admin/local/restart';
const protocols: { value: ModelProtocol; titleKey: MessageKey; descriptionKey: MessageKey }[] = [
  {
    value: 'openai_compatible',
    titleKey: 'settings.protocolOpenAI',
    descriptionKey: 'settings.protocolOpenAIDesc',
  },
  {
    value: 'anthropic',
    titleKey: 'settings.protocolAnthropic',
    descriptionKey: 'settings.protocolAnthropicDesc',
  },
];

export default function SystemSettingsPage() {
  const { t } = useLocale();
  const [value, setValue] = useState<ModelConfig | null>(null);
  const [savedValue, setSavedValue] = useState<ModelConfig | null>(null);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [restartRequired, setRestartRequired] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [restartSupported, setRestartSupported] = useState(false);
  const [serviceInstanceId, setServiceInstanceId] = useState('');

  const load = async () => {
    setError('');
    try {
      const response = await fetch(endpoint, { cache: 'no-store' });
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
      const loaded = modelConfigFromResponse(body as Record<string, unknown>);
      setSavedValue(loaded);
      setValue(loaded);
      setRestartSupported(body.restartSupported === true);
      setServiceInstanceId(typeof body.serviceInstanceId === 'string' ? body.serviceInstanceId : '');
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const save = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!value) return;
    const connectionChanged = hasModelConnectionChanged(value, savedValue);
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
      setMessage(t('settings.saved'));
      if (connectionChanged) setRestartRequired(true);
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(false);
    }
  };

  const updateValue = (patch: Partial<ModelConfig>) => {
    setValue((current) => current ? { ...current, ...patch } : current);
  };

  const selectProtocol = (protocol: ModelProtocol) => {
    setValue(switchModelProtocol(savedValue, protocol));
  };

  const restartAgent = async () => {
    setRestarting(true);
    setMessage('');
    setError('');
    try {
      const response = await fetch(restartEndpoint, { method: 'POST' });
      const body = await response.json().catch(() => ({})) as Record<string, unknown>;
      if (!response.ok) throw new Error(typeof body.error === 'string' ? body.error : `HTTP ${response.status}`);
      const previousInstanceId = typeof body.serviceInstanceId === 'string'
        ? body.serviceInstanceId
        : serviceInstanceId;
      const deadline = Date.now() + 20_000;
      let observedUnavailable = false;
      while (Date.now() < deadline) {
        await new Promise((resolve) => window.setTimeout(resolve, 500));
        try {
          const nextResponse = await fetch(endpoint, { cache: 'no-store' });
          if (!nextResponse.ok) {
            observedUnavailable = true;
            continue;
          }
          const nextBody = await nextResponse.json() as Record<string, unknown>;
          const nextInstanceId = typeof nextBody.serviceInstanceId === 'string' ? nextBody.serviceInstanceId : '';
          const restarted = previousInstanceId
            ? Boolean(nextInstanceId && nextInstanceId !== previousInstanceId)
            : observedUnavailable;
          if (!restarted) continue;
          const loaded = modelConfigFromResponse(nextBody);
          setSavedValue(loaded);
          setValue(loaded);
          setRestartSupported(nextBody.restartSupported === true);
          setServiceInstanceId(nextInstanceId);
          setRestartRequired(false);
          setMessage(t('settings.restartComplete'));
          return;
        } catch {
          observedUnavailable = true;
        }
      }
      throw new Error(t('settings.restartTimeout'));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setRestarting(false);
    }
  };

  const field = 'h-10 w-full rounded-lg border border-neutral-200 bg-surface-raised px-3 text-[13px] text-neutral-800 outline-none transition-colors placeholder:text-neutral-500 focus:border-neutral-500 focus:ring-2 focus:ring-neutral-100';
  const isAnthropic = value?.protocol === 'anthropic';

  return (
    <main className="max-w-[800px] p-4 sm:p-6">
      <div className="flex items-start justify-between gap-5">
        <div>
          <h1 className="text-[20px] font-semibold text-neutral-900">{t('settings.title')}</h1>
          <p className="mt-1 text-[13px] text-neutral-500">
            {t('settings.intro')}
          </p>
        </div>
        <button
          type="button"
          onClick={() => void load()}
          disabled={busy}
          className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-neutral-200 px-3 text-[12px] text-neutral-600 hover:bg-neutral-50"
        >
          <RefreshCw className="h-3 w-3" />{t('common.refresh')}
        </button>
      </div>

      <form
        onSubmit={save}
        aria-busy={!value}
        className="mt-5 overflow-hidden rounded-xl border border-neutral-200/70 bg-surface"
      >
        <div className="border-b border-neutral-100 px-5 py-4">
          <div className="flex items-center gap-2 text-[13px] font-medium text-neutral-800">
            <KeyRound className="h-4 w-4 text-neutral-500" />{t('settings.modelSection')}
          </div>
          <p className="mt-1 text-[12px] leading-5 text-neutral-500">
            {t('settings.modelSectionDesc')}
          </p>
        </div>

        {!value && (
          <div role="status" className="border-b border-neutral-100 bg-neutral-50/60 px-5 py-3 text-[12px] text-neutral-500">
            {t('common.loading')}
          </div>
        )}

        <div className="grid gap-5 p-5">
          <fieldset disabled={!value}>
            <legend className="mb-2 text-[12px] font-medium text-neutral-700">{t('settings.protocol')}</legend>
            <div role="radiogroup" className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              {protocols.map((protocol) => {
                const selected = value?.protocol === protocol.value;
                return (
                  <button
                    type="button"
                    role="radio"
                    aria-checked={selected}
                    disabled={!value}
                    key={protocol.value}
                    onClick={() => selectProtocol(protocol.value)}
                    className={`relative rounded-xl border p-3.5 text-left transition disabled:cursor-wait ${selected ? 'border-neutral-800 bg-neutral-50 shadow-sm' : 'border-neutral-200 bg-surface-raised hover:border-neutral-300'}`}
                  >
                    <span className="flex items-center justify-between gap-3">
                      <span className="text-[13px] font-semibold text-neutral-900">{t(protocol.titleKey)}</span>
                      <span className={`inline-flex h-5 w-5 items-center justify-center rounded-full ${selected ? 'bg-inverse text-inverse-fg' : 'border border-neutral-200 text-transparent'}`}>
                        <Check className="h-3 w-3" />
                      </span>
                    </span>
                    <span className="mt-1.5 block pr-5 text-[11px] leading-5 text-neutral-500">{t(protocol.descriptionKey)}</span>
                  </button>
                );
              })}
            </div>
          </fieldset>

          <label className="grid gap-1.5 text-[12px] font-medium text-neutral-700">
            {t('settings.apiBase')}
            <input
              required
              disabled={!value}
              className={field}
              placeholder={isAnthropic ? 'https://api.anthropic.com' : 'https://dashscope.aliyuncs.com/compatible-mode/v1'}
              value={value?.baseUrl || ''}
              onChange={(event) => updateValue({ baseUrl: event.target.value })}
            />
            <span className="font-normal text-[11px] text-neutral-500">
              {isAnthropic ? t('settings.apiBaseHintAnthropic') : t('settings.apiBaseHintOpenAI')}
            </span>
          </label>

          <label className="grid gap-1.5 text-[12px] font-medium text-neutral-700">
            {t('settings.modelName')}
            <input
              required
              disabled={!value}
              className={field}
              placeholder={isAnthropic ? 'claude-sonnet-4-5' : 'qwen-max'}
              value={value?.model || ''}
              onChange={(event) => updateValue({ model: event.target.value })}
            />
          </label>

          <label className="grid gap-1.5 text-[12px] font-medium text-neutral-700">
            API Key
            <input
              className={field}
              type="password"
              disabled={!value}
              required={value ? requiresModelAPIKey(value, savedValue) : true}
              autoComplete="new-password"
              placeholder={value?.apiKeyMasked ? t('settings.apiKeySaved', { masked: value.apiKeyMasked }) : t('settings.apiKeyRequired')}
              value={value?.apiKey || ''}
              onChange={(event) => updateValue({ apiKey: event.target.value })}
            />
          </label>
        </div>

        <div className="flex flex-col items-stretch justify-between gap-3 border-t border-neutral-100 bg-neutral-50/60 px-5 py-3 sm:flex-row sm:items-center sm:gap-4">
          <div className="inline-flex items-center gap-1.5 text-[11px] text-neutral-500">
            <ShieldCheck className="h-3.5 w-3.5 text-emerald-600" />{t('settings.apiKeyNotice')}
          </div>
          <button disabled={busy || !value} className="h-10 rounded-lg bg-inverse px-3.5 text-[12px] font-medium text-inverse-fg hover:bg-inverse-hover disabled:opacity-50 sm:h-8">
            {busy ? t('common.saving') : t('settings.save')}
          </button>
        </div>
      </form>

      {message && <p className="mt-3 rounded-lg bg-emerald-50 px-3 py-2 text-[12px] text-emerald-700">{message}</p>}
      {restartRequired && (
        <section
          role="status"
          aria-live="polite"
          className="mt-3 flex flex-col gap-3 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 sm:flex-row sm:items-center sm:justify-between"
        >
          <div className="flex min-w-0 items-start gap-2.5">
            <AlertTriangle aria-hidden="true" className="mt-0.5 h-4 w-4 shrink-0 text-amber-600" />
            <div className="min-w-0">
              <h2 className="text-[12px] font-semibold text-amber-900">{t('settings.restartNoticeTitle')}</h2>
              <p className="mt-0.5 text-[11px] leading-5 text-amber-800">{t('settings.restartNoticeBody')}</p>
              <p className="text-[11px] leading-5 text-amber-700">{t('settings.restartNoticeWarning')}</p>
            </div>
          </div>
          {restartSupported ? (
            <button
              type="button"
              onClick={() => void restartAgent()}
              disabled={restarting}
              aria-busy={restarting}
              className="inline-flex h-10 shrink-0 items-center justify-center gap-1.5 rounded-lg bg-amber-900 px-3.5 text-[12px] font-medium text-white hover:bg-amber-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-amber-500 focus-visible:ring-offset-2 disabled:cursor-wait disabled:opacity-60 sm:h-8"
            >
              <RotateCcw aria-hidden="true" className={`h-3.5 w-3.5 ${restarting ? 'animate-spin' : ''}`} />
              {restarting ? t('settings.restartingAgent') : t('settings.restartAgent')}
            </button>
          ) : (
            <p className="shrink-0 text-[11px] text-amber-800">{t('settings.restartUnsupported')}</p>
          )}
        </section>
      )}
      {error && <p className="mt-3 rounded-lg bg-red-50 px-3 py-2 text-[12px] text-red-700">{error}</p>}
    </main>
  );
}
