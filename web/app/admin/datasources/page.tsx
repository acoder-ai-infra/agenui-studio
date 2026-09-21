'use client';

import { useCallback, useEffect, useState } from 'react';
import { Plus, RefreshCw, Search, Sparkles } from 'lucide-react';
import {
  datasourceApi,
  type DataSourceItem,
  type FieldMeta,
  type RecallCandidate,
} from '@/lib/datasource-api';
import { useLocale } from '@/lib/i18n/LocaleContext';

const VALUE_TYPES = ['string', 'number', 'money', 'image', 'datetime', 'boolean'];

const emptyField = (): FieldMeta => ({
  path: '',
  name: '',
  semantic: '',
  valueType: 'string',
  unit: '',
  scope: 'list_item',
});

const emptyForm = {
  name: '',
  description: '',
  endpoint: '',
  method: 'GET',
  itemsPath: 'data.items',
  entityKey: '',
  tags: '',
};

export default function DataSourcesPage() {
  const { t } = useLocale();
  const [items, setItems] = useState<DataSourceItem[]>([]);
  const [form, setForm] = useState(emptyForm);
  const [fields, setFields] = useState<FieldMeta[]>([emptyField()]);
  const [editing, setEditing] = useState<string | null>(null);
  const [message, setMessage] = useState('');
  // Not translated: the seeded demo endpoints carry Chinese field semantics, so
  // an English default query would return no candidates.
  const [query, setQuery] = useState('每晚价格');
  const [results, setResults] = useState<RecallCandidate[]>([]);
  const [seedStatus, setSeedStatus] = useState('');

  const refresh = useCallback(async () => {
    try {
      const res = await datasourceApi.list();
      setItems(res.items ?? []);
    } catch (err) {
      setMessage(String(err));
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const save = async () => {
    setMessage('');
    if (!form.name.trim() || !form.endpoint.trim()) {
      setMessage(t('datasources.required'));
      return;
    }
    const valid = fields.filter((f) => f.path.trim() && f.name.trim());
    if (valid.length === 0) {
      setMessage(t('datasources.fieldRequired'));
      return;
    }
    try {
      await datasourceApi.save({
        ...(editing ? { id: editing } : {}),
        name: form.name.trim(),
        description: form.description,
        endpoint: form.endpoint.trim(),
        method: form.method,
        itemsPath: form.itemsPath,
        entityKey: form.entityKey,
        tags: form.tags ? form.tags.split(/[,，]\s*/) : [],
        fields: valid,
      });
      setForm(emptyForm);
      setFields([emptyField()]);
      setEditing(null);
      refresh();
      setMessage(t('datasources.saved'));
    } catch (err) {
      setMessage(String(err));
    }
  };

  const recall = async () => {
    try {
      const res = await datasourceApi.recall(query);
      setResults(res.items ?? []);
    } catch (err) {
      setMessage(String(err));
    }
  };

  const seed = async () => {
    setSeedStatus('');
    try {
      const res = await datasourceApi.initDemo();
      setSeedStatus(t('datasources.seeded', { count: res.report.apis }));
      refresh();
    } catch (err) {
      setSeedStatus(String(err));
    }
  };

  return (
    <div className="p-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-[20px] font-semibold text-neutral-900">{t('datasources.title')}</h1>
          <p className="text-[13px] text-neutral-500 mt-1">
            {t('datasources.intro')}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {seedStatus && <span className="text-[12px] text-neutral-500">{seedStatus}</span>}
          <button
            onClick={seed}
            className="inline-flex items-center gap-1.5 h-8 px-3 rounded-lg border border-neutral-200 text-[12px] text-neutral-600 hover:bg-neutral-50"
          >
            <Sparkles className="w-3 h-3" /> {t('datasources.seed')}
          </button>
          <button
            onClick={refresh}
            className="inline-flex items-center gap-1.5 h-8 px-3 rounded-lg border border-neutral-200 text-[12px] text-neutral-600 hover:bg-neutral-50"
          >
            <RefreshCw className="w-3 h-3" /> {t('common.refresh')}
          </button>
        </div>
      </div>

      <div className="grid grid-cols-5 gap-4 mt-4">
        {/* editor */}
        <div className="col-span-3 rounded-xl border border-neutral-200/70 bg-surface p-4">
          <div className="text-[13px] font-semibold text-neutral-800">
            {editing ? t('datasources.editEndpoint', { name: editing }) : t('datasources.createEndpoint')}
          </div>
          <div className="grid grid-cols-3 gap-3 mt-3">
            <label className="text-[11px] text-neutral-400 flex flex-col gap-1">
              {t('common.name')} *
              <input
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder={t('datasources.namePlaceholder')}
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
            </label>
            <label className="col-span-2 text-[11px] text-neutral-400 flex flex-col gap-1">
              Endpoint *
              <input
                value={form.endpoint}
                onChange={(e) => setForm({ ...form, endpoint: e.target.value })}
                placeholder="http://127.0.0.1:18082/demo/products"
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] font-mono text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
            </label>
            <label className="text-[11px] text-neutral-400 flex flex-col gap-1">
              Method
              <select
                value={form.method}
                onChange={(e) => setForm({ ...form, method: e.target.value })}
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] text-neutral-800"
              >
                <option>GET</option>
                <option>POST</option>
              </select>
            </label>
            <label className="text-[11px] text-neutral-400 flex flex-col gap-1">
              {t('datasources.itemsPath')}
              <input
                value={form.itemsPath}
                onChange={(e) => setForm({ ...form, itemsPath: e.target.value })}
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] font-mono text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
            </label>
            <label className="text-[11px] text-neutral-400 flex flex-col gap-1">
              {t('datasources.entityKey')}
              <input
                value={form.entityKey}
                onChange={(e) => setForm({ ...form, entityKey: e.target.value })}
                placeholder="product_id"
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] font-mono text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
            </label>
            <label className="col-span-3 text-[11px] text-neutral-400 flex flex-col gap-1">
              {t('datasources.notes')}
              <input
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
                className="h-8 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
            </label>
          </div>

          <div className="flex items-center justify-between mt-4">
            <div className="text-[12px] font-semibold text-neutral-700">{t('datasources.fieldTable')}</div>
            <button
              onClick={() => setFields([...fields, emptyField()])}
              className="inline-flex items-center gap-1 h-7 px-2.5 rounded-md border border-neutral-200 text-[11px] text-neutral-600 hover:bg-neutral-50"
            >
              <Plus className="w-3 h-3" /> {t('datasources.addField')}
            </button>
          </div>
          <div className="mt-2 space-y-2">
            <div className="grid grid-cols-[1fr_1fr_1.4fr_0.8fr_0.6fr_0.8fr_24px] gap-1.5 text-[10px] text-neutral-400">
              <span>path</span>
              <span>{t('common.name')}</span>
              <span>{t('datasources.semantics')}</span>
              <span>{t('common.type')}</span>
              <span>{t('datasources.unit')}</span>
              <span>{t('datasources.scope')}</span>
              <span />
            </div>
            {fields.map((f, i) => (
              <div key={i} className="grid grid-cols-[1fr_1fr_1.4fr_0.8fr_0.6fr_0.8fr_24px] gap-1.5">
                <input
                  value={f.path}
                  placeholder="price_cents"
                  onChange={(e) =>
                    setFields(fields.map((x, j) => (j === i ? { ...x, path: e.target.value } : x)))
                  }
                  className="h-7 rounded-md border border-neutral-200 bg-surface-raised px-1.5 text-[11px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
                />
                <input
                  value={f.name}
                  placeholder={t('datasources.fieldNamePlaceholder')}
                  onChange={(e) =>
                    setFields(fields.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))
                  }
                  className="h-7 rounded-md border border-neutral-200 bg-surface-raised px-1.5 text-[11px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
                />
                <input
                  value={f.semantic}
                  placeholder={t('datasources.semanticsPlaceholder')}
                  onChange={(e) =>
                    setFields(fields.map((x, j) => (j === i ? { ...x, semantic: e.target.value } : x)))
                  }
                  className="h-7 rounded-md border border-neutral-200 bg-surface-raised px-1.5 text-[11px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
                />
                <select
                  value={f.valueType}
                  onChange={(e) =>
                    setFields(fields.map((x, j) => (j === i ? { ...x, valueType: e.target.value } : x)))
                  }
                  className="h-7 rounded-md border border-neutral-200 bg-surface-raised px-1 text-[11px] text-neutral-800"
                >
                  {VALUE_TYPES.map((valueType) => (
                    <option key={valueType}>{valueType}</option>
                  ))}
                </select>
                <input
                  value={f.unit}
                  placeholder="cents"
                  onChange={(e) =>
                    setFields(fields.map((x, j) => (j === i ? { ...x, unit: e.target.value } : x)))
                  }
                  className="h-7 rounded-md border border-neutral-200 bg-surface-raised px-1.5 text-[11px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
                />
                <select
                  value={f.scope}
                  onChange={(e) =>
                    setFields(fields.map((x, j) => (j === i ? { ...x, scope: e.target.value } : x)))
                  }
                  className="h-7 rounded-md border border-neutral-200 bg-surface-raised px-1 text-[11px] text-neutral-800"
                >
                  <option value="list_item">list_item</option>
                  <option value="card">card</option>
                </select>
                <button
                  onClick={() => setFields(fields.filter((_, j) => j !== i))}
                  className="h-6 w-6 rounded text-[11px] text-red-500 bg-red-50 hover:bg-red-100"
                >
                  ✕
                </button>
              </div>
            ))}
          </div>

          <div className="flex items-center gap-3 mt-4">
            <button
              onClick={save}
              className="h-8 rounded-lg bg-inverse px-4 text-[12px] font-medium text-inverse-fg hover:bg-inverse-hover"
            >
              {t('datasources.saveEndpoint')}
            </button>
            {editing && (
              <button
                onClick={() => {
                  setEditing(null);
                  setForm(emptyForm);
                  setFields([emptyField()]);
                }}
                className="h-8 px-3 rounded-lg border border-neutral-200 text-[12px] text-neutral-600 hover:bg-neutral-50"
              >
                {t('common.cancel')}
              </button>
            )}
            {message && <span className="text-[12px] text-neutral-500">{message}</span>}
          </div>
        </div>

        {/* list + recall */}
        <div className="col-span-2 space-y-4">
          <div className="rounded-xl border border-neutral-200/70 bg-surface overflow-hidden">
            <div className="px-4 py-2.5 text-[12px] font-semibold text-neutral-700 border-b border-neutral-100">
              {t('datasources.registered', { count: items.length })}
            </div>
            <table className="w-full text-[12px]">
              <tbody>
                {items.map((api) => (
                  <tr key={api.id} className="border-b border-neutral-50 last:border-0">
                    <td className="px-4 py-2">
                      <span
                        className={
                          api.method === 'POST'
                            ? 'inline-block rounded bg-amber-50 text-amber-700 text-[10px] font-bold px-1.5 py-0.5 mr-1.5'
                            : 'inline-block rounded bg-emerald-50 text-emerald-600 text-[10px] font-bold px-1.5 py-0.5 mr-1.5'
                        }
                      >
                        {api.method || 'GET'}
                      </span>
                      <span className="text-neutral-800">{api.name}</span>
                      <div className="font-mono text-[10px] text-neutral-400 mt-0.5">
                        {api.id} · {t('datasources.fieldCount', { count: api.fields?.length ?? 0 })}
                      </div>
                    </td>
                    <td className="px-4 py-2 text-right">
                      <button
                        onClick={() => {
                          setEditing(api.id);
                          setForm({
                            name: api.name,
                            description: api.description ?? '',
                            endpoint: api.endpoint,
                            method: api.method || 'GET',
                            itemsPath: api.itemsPath ?? '',
                            entityKey: api.entityKey ?? '',
                            tags: (api.tags ?? []).join(', '),
                          });
                          setFields(
                            api.fields?.length ? api.fields.map((f) => ({ ...f })) : [emptyField()],
                          );
                        }}
                        className="h-6 px-2 rounded-md border border-neutral-200 text-[11px] text-neutral-600 hover:bg-neutral-50"
                      >
                        {t('common.edit')}
                      </button>
                    </td>
                  </tr>
                ))}
                {items.length === 0 && (
                  <tr>
                    <td className="px-4 py-6 text-center text-[11px] text-neutral-400">
                      {t('datasources.empty')}
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>

          <div className="rounded-xl border border-neutral-200/70 bg-surface p-4">
            <div className="text-[12px] font-semibold text-neutral-700">{t('datasources.retrievalTest')}</div>
            <p className="text-[11px] text-neutral-400 mt-1">
              {t('datasources.retrievalTestDesc')}
            </p>
            <div className="flex gap-2 mt-2">
              <input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                className="h-8 flex-1 rounded-md border border-neutral-200 bg-surface-raised px-2 text-[12px] text-neutral-800 placeholder:text-neutral-500 focus:border-neutral-400 focus:outline-none"
              />
              <button
                onClick={recall}
                className="inline-flex h-8 items-center gap-1 rounded-lg bg-inverse px-3 text-[12px] text-inverse-fg hover:bg-inverse-hover"
              >
                <Search className="w-3 h-3" /> {t('datasources.test')}
              </button>
            </div>
            {results.length > 0 && (
              <div className="mt-2 space-y-1.5">
                {results.slice(0, 5).map((c, i) => (
                  <div key={i} className="flex items-center gap-2 text-[11px]">
                    <span className="font-mono text-neutral-400 w-12">{c.score}</span>
                    <span className="text-neutral-800">{c.field.name}</span>
                    <span className="font-mono text-neutral-400">{c.field.path}</span>
                    <span className="ml-auto rounded-full bg-neutral-100 text-neutral-500 px-1.5 text-[10px]">
                      {c.provider}
                    </span>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
