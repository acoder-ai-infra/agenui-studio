'use client';

import { useCallback, useEffect, useState } from 'react';
import dynamic from 'next/dynamic';
import { RefreshCw, Trash2, UploadCloud } from 'lucide-react';
import { useLocale } from '@/lib/i18n/LocaleContext';

import {
  DEFAULT_OPERATOR_CODE,
  OPERATOR_LANGUAGE,
  operatorSavePayload,
} from './operator-config';
import { useTheme } from '@/lib/theme/ThemeContext';

const MonacoEditor = dynamic(() => import('@monaco-editor/react'), {
  ssr: false,
  loading: () => <div className="h-full animate-pulse rounded bg-neutral-50" />,
});

interface OperatorItem {
  operatorKey: string;
  operatorVersionId: string;
  name: string;
  description: string;
  usageScenario: string;
  language: string;
  status: number; // 0 draft | 1 published
  versionNo: number;
  draftVersionNo?: number;
  latestPublishedVersionNo?: number;
}

const emptyForm = {
  operatorKey: '',
  name: '',
  description: '',
  usageScenario: '',
  code: DEFAULT_OPERATOR_CODE,
};

export default function OperatorsPage() {
  const { t } = useLocale();
  const { theme } = useTheme();
  const [items, setItems] = useState<OperatorItem[]>([]);
  const [form, setForm] = useState(emptyForm);
  const [editing, setEditing] = useState<string | null>(null);
  const [message, setMessage] = useState('');

  const refresh = useCallback(async () => {
    try {
      const res = await fetch('/api/v1/agenui/admin/local/operators');
      const data = (await res.json()) as { items: OperatorItem[] };
      setItems(data.items ?? []);
    } catch (err) {
      setMessage(String(err));
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const save = async () => {
    setMessage('');
    if (!form.name.trim() || !form.description.trim() || !form.code.trim()) {
      setMessage(t('operators.required'));
      return;
    }
    try {
      const res = await fetch('/api/v1/agenui/admin/local/operators/save', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(operatorSavePayload(form)),
      });
      const data = (await res.json()) as { error?: string };
      if (!res.ok) {
        setMessage(data.error ?? `HTTP ${res.status}`);
        return;
      }
      setForm(emptyForm);
      setEditing(null);
      refresh();
      setMessage(t('operators.draftSaved'));
    } catch (err) {
      setMessage(String(err));
    }
  };

  const publish = async (operatorVersionId: string) => {
    setMessage('');
    try {
      const res = await fetch('/api/v1/agenui/admin/local/operators/publish', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ operatorVersionId }),
      });
      const data = (await res.json()) as { error?: string };
      if (!res.ok) {
        setMessage(data.error ?? `HTTP ${res.status}`);
        return;
      }
      refresh();
      setMessage(t('operators.published'));
    } catch (err) {
      setMessage(String(err));
    }
  };

  const deleteDraft = async (operatorVersionId: string) => {
    setMessage('');
    try {
      await fetch('/api/v1/agenui/admin/local/operators/delete-draft', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ operatorVersionId }),
      });
      refresh();
    } catch (err) {
      setMessage(String(err));
    }
  };

  return (
    <div className="p-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-[20px] font-semibold text-neutral-900">{t('operators.title')}</h1>
          <p className="text-[13px] text-neutral-500 mt-1">
            {t('operators.intro')}
          </p>
        </div>
        <button
          onClick={refresh}
          className="inline-flex items-center gap-1.5 h-8 px-3 rounded-lg border border-neutral-200 text-[12px] text-neutral-600 hover:bg-neutral-50"
        >
          <RefreshCw className="w-3 h-3" /> {t('common.refresh')}
        </button>
      </div>

      <div className="grid grid-cols-5 gap-4 mt-4">
        <div className="col-span-2 rounded-xl border border-neutral-200/70 bg-surface p-4">
          <div className="text-[13px] font-semibold text-neutral-800">
            {editing ? t('operators.editDraft', { name: editing }) : t('operators.create')}
          </div>
          <div className="grid grid-cols-2 gap-3 mt-3">
            <label className="text-[11px] text-neutral-400 flex flex-col gap-1">
              {t('common.name')} *
              <input
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="meters_to_km"
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
            </label>
            <label className="text-[11px] text-neutral-400 flex flex-col gap-1">
              {t('operators.useCase')} *
              <input
                value={form.usageScenario}
                onChange={(e) => setForm({ ...form, usageScenario: e.target.value })}
                placeholder={t('operators.useCasePlaceholder')}
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
            </label>
            <label className="col-span-2 text-[11px] text-neutral-400 flex flex-col gap-1">
              {t('common.description')} *
              <input
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
                placeholder={t('operators.descriptionPlaceholder')}
                required
                aria-required="true"
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
              <span className="text-[10px] leading-4 text-neutral-400">
                {t('operators.descriptionHint')}
              </span>
            </label>
          </div>
          <div className="mt-3 h-[220px] rounded-lg border border-neutral-200 overflow-hidden">
            <MonacoEditor
              height="220px"
              language={OPERATOR_LANGUAGE}
              theme={theme === 'dark' ? 'vs-dark' : 'light'}
              value={form.code}
              onChange={(v) => setForm({ ...form, code: v ?? '' })}
              options={{ minimap: { enabled: false }, fontSize: 12 }}
            />
          </div>
          <div className="flex items-center gap-3 mt-3">
            <button
              onClick={save}
              className="h-8 rounded-lg bg-inverse px-4 text-[12px] font-medium text-inverse-fg hover:bg-inverse-hover"
            >
              {t('operators.saveDraft')}
            </button>
            {editing && (
              <button
                onClick={() => {
                  setEditing(null);
                  setForm(emptyForm);
                }}
                className="h-8 px-3 rounded-lg border border-neutral-200 text-[12px] text-neutral-600 hover:bg-neutral-50"
              >
                {t('common.cancel')}
              </button>
            )}
            {message && <span className="text-[12px] text-neutral-500">{message}</span>}
          </div>
        </div>

        <div className="col-span-3 rounded-xl border border-neutral-200/70 bg-surface overflow-hidden">
          <table className="w-full text-[13px]">
            <thead>
              <tr className="text-left text-[11px] text-neutral-400 border-b border-neutral-100">
                <th className="px-4 py-2.5 font-medium">{t('common.name')}</th>
                <th className="px-4 py-2.5 font-medium">{t('common.version')}</th>
                <th className="px-4 py-2.5 font-medium">{t('common.status')}</th>
                <th className="px-4 py-2.5 font-medium">{t('operators.useCase')}</th>
                <th className="px-4 py-2.5 font-medium text-right">{t('common.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {items.map((op) => (
                <tr key={op.operatorKey} className="border-b border-neutral-50 last:border-0">
                  <td className="px-4 py-2.5">
                    <div className="font-mono text-[12px] text-neutral-800">{op.name}</div>
                    <div className="text-[10px] text-neutral-400">{op.operatorKey}</div>
                  </td>
                  <td className="px-4 py-2.5 text-[12px] text-neutral-500">
                    {op.status === 0
                      ? t('operators.draftVersion', { version: op.versionNo })
                      : t('operators.latestVersion', { version: op.latestPublishedVersionNo ?? op.versionNo })}
                    {op.status === 1 && op.draftVersionNo
                      ? t('operators.alsoDraft', { version: op.draftVersionNo })
                      : ''}
                  </td>
                  <td className="px-4 py-2.5">
                    <span
                      className={
                        op.status === 1
                          ? 'inline-block rounded-full bg-emerald-50 text-emerald-600 text-[11px] px-2 py-0.5'
                          : 'inline-block rounded-full bg-neutral-100 text-neutral-500 text-[11px] px-2 py-0.5'
                      }
                    >
                      {op.status === 1 ? t('operators.statusPublished') : t('operators.statusDraft')}
                    </span>
                  </td>
                  <td className="px-4 py-2.5 text-[12px] text-neutral-500">{op.usageScenario}</td>
                  <td className="px-4 py-2.5 text-right">
                    <div className="inline-flex items-center gap-1.5">
                      {op.status === 0 && (
                        <>
                          <button
                            onClick={() => publish(op.operatorVersionId)}
                            className="inline-flex h-7 items-center gap-1 rounded-md bg-inverse px-2.5 text-[11px] text-inverse-fg hover:bg-inverse-hover"
                          >
                            <UploadCloud className="w-3 h-3" /> {t('operators.publish')}
                          </button>
                          <button
                            onClick={() => deleteDraft(op.operatorVersionId)}
                            className="h-7 px-2 rounded-md border border-neutral-200 text-[11px] text-red-500 hover:bg-red-50"
                          >
                            <Trash2 className="w-3 h-3" />
                          </button>
                        </>
                      )}
                      {op.status === 1 && (
                        <span className="text-[10px] text-neutral-400">{t('operators.immutable')}</span>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
              {items.length === 0 && (
                <tr>
                  <td colSpan={5} className="px-4 py-8 text-center text-[12px] text-neutral-400">
                    {t('operators.empty')}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
