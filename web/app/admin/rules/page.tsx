'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Atom,
  Check,
  CheckCircle2,
  CircleAlert,
  FileText,
  FileUp,
  FolderUp,
  Layers3,
  Loader2,
  Search,
  Sparkles,
  type LucideIcon,
} from 'lucide-react';
import { datasourceApi } from '@/lib/datasource-api';
import { useLocale, type MessageKey, type Translate } from '@/lib/i18n/LocaleContext';

interface JobItem {
  id: string;
  status: string;
  files: string[] | null;
  parsed: number;
  error?: string;
  createdAt: string;
}

interface SourceRuleItem {
  id: string;
  title: string;
  kind: 'layout' | 'element' | 'rule';
  version: string;
  summary: string;
  appliesTo: string[];
  notFor: string[];
  requires: string[];
  conflictsWith: string[];
  references: string[];
  content: string;
}

interface RuleLibrary {
  revisionId: string;
  revisionHash: string;
  items: SourceRuleItem[];
}

interface RuleUploadResult {
  error?: string;
  queued?: number;
  skipped?: number;
  rejected?: Array<{ fileName: string; reason: string }>;
}

type NoticeTone = 'success' | 'error' | 'neutral';
type RuleFilter = 'all' | SourceRuleItem['kind'];

const buttonBase =
  'inline-flex h-11 shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-lg px-3.5 text-[12px] font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400 focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50 sm:h-9';
const secondaryButton = `${buttonBase} border border-line bg-surface-raised text-neutral-700 hover:border-neutral-300 hover:bg-neutral-50`;
const primaryButton = `${buttonBase} bg-inverse text-inverse-fg hover:bg-inverse-hover`;

export default function RulesPage() {
  const { t } = useLocale();
  const [jobs, setJobs] = useState<JobItem[]>([]);
  const [library, setLibrary] = useState<RuleLibrary | null>(null);
  const [message, setMessage] = useState('');
  const [noticeTone, setNoticeTone] = useState<NoticeTone>('neutral');
  const [uploading, setUploading] = useState(false);
  const [seeding, setSeeding] = useState(false);
  const [filter, setFilter] = useState<RuleFilter>('all');
  const [query, setQuery] = useState('');
  const fileRef = useRef<HTMLInputElement>(null);
  const folderRef = useRef<HTMLInputElement>(null);

  const refresh = useCallback(async () => {
    try {
      const [jobsResponse, libraryResponse] = await Promise.all([
        fetch('/api/v1/agenui/admin/local/rules/jobs', { cache: 'no-store' }),
        fetch('/api/v1/agenui/admin/local/rules/library', { cache: 'no-store' }),
      ]);
      if (!jobsResponse.ok || !libraryResponse.ok) {
        throw new Error(`HTTP ${!jobsResponse.ok ? jobsResponse.status : libraryResponse.status}`);
      }
      const [jobData, source] = await Promise.all([
        jobsResponse.json() as Promise<{ items: JobItem[] }>,
        libraryResponse.json() as Promise<RuleLibrary>,
      ]);
      setJobs(jobData.items ?? []);
      setLibrary(source);
    } catch (err) {
      setNoticeTone('error');
      setMessage(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const hasActiveJob = jobs.some((job) => ['queued', 'running', 'processing'].includes(job.status));
  useEffect(() => {
    if (!hasActiveJob) return;
    const timer = window.setInterval(() => void refresh(), 3000);
    return () => window.clearInterval(timer);
  }, [hasActiveJob, refresh]);

  const upload = async (list: FileList | null) => {
    if (!list || list.length === 0) return;
    setMessage('');
    setUploading(true);
    const form = new FormData();
    for (const file of Array.from(list)) form.append('files', file, file.name);
    try {
      const response = await fetch('/api/v1/agenui/admin/local/rules/upload', {
        method: 'POST',
        body: form,
      });
      const data = (await response.json()) as RuleUploadResult;
      if (!response.ok) throw new Error(data.error ?? `HTTP ${response.status}`);
      const skipped = data.skipped ?? 0;
      const rejected = data.rejected ?? [];
      const notes = [
        skipped > 0 ? t('rules.skipped', { count: skipped }) : '',
        rejected.length > 0
          ? t('rules.rejected', {
              count: rejected.length,
              names: rejected.map((item) => item.fileName).join(t('common.listSeparator')),
            })
          : '',
      ].filter(Boolean);
      setNoticeTone('success');
      setMessage(t('rules.queued', {
        count: data.queued ?? 0,
        notes: notes.length > 0
          ? t('rules.notesWrapper', { notes: notes.join(t('common.clauseSeparator')) })
          : '',
      }));
      await refresh();
    } catch (err) {
      setNoticeTone('error');
      setMessage(err instanceof Error ? err.message : String(err));
    } finally {
      setUploading(false);
      if (fileRef.current) fileRef.current.value = '';
      if (folderRef.current) folderRef.current.value = '';
    }
  };

  const initializeDemo = async () => {
    setMessage('');
    setSeeding(true);
    try {
      const result = await datasourceApi.initDemo();
      setNoticeTone('success');
      setMessage(t('rules.seeded', { count: result.report.rules }));
      await refresh();
    } catch (err) {
      setNoticeTone('error');
      setMessage(err instanceof Error ? err.message : String(err));
    } finally {
      setSeeding(false);
    }
  };

  const layouts = library?.items.filter((item) => item.kind === 'layout') ?? [];
  const elements = library?.items.filter((item) => item.kind === 'element') ?? [];
  const rules = library?.items.filter((item) => item.kind === 'rule') ?? [];
  const latestJob = jobs[0];
  const attentionJob = latestJob && latestJob.status !== 'done' ? latestJob : null;
  const historyJobs = attentionJob ? jobs.slice(1) : jobs;
  const normalizedQuery = query.trim().toLowerCase();
  const visibleItems = (library?.items ?? []).filter((item) => {
    if (filter !== 'all' && item.kind !== filter) return false;
    if (!normalizedQuery) return true;
    const presentation = rulePresentation(item, t);
    return [
      presentation.title,
      presentation.description,
      ...(presentation.useCases ?? []),
      ...(presentation.includes ?? []),
      item.id,
      item.title,
      item.summary,
      item.content,
      ...(item.appliesTo ?? []),
      ...(item.requires ?? []),
    ]
      .join(' ')
      .toLowerCase()
      .includes(normalizedQuery);
  });

  const filterOptions: Array<{ value: RuleFilter; label: MessageKey; count: number }> = [
    { value: 'all', label: 'rules.filterAll', count: library?.items.length ?? 0 },
    { value: 'layout', label: 'rules.filterLayout', count: layouts.length },
    { value: 'element', label: 'rules.filterElement', count: elements.length },
    { value: 'rule', label: 'rules.filterAtomic', count: rules.length },
  ];
  const filters = filterOptions.filter((item) => item.value === 'all' || item.count > 0);

  useEffect(() => {
    if (filter === 'layout' && layouts.length === 0) setFilter('all');
    if (filter === 'element' && elements.length === 0) setFilter('all');
    if (filter === 'rule' && rules.length === 0) setFilter('all');
  }, [elements.length, filter, layouts.length, rules.length]);

  return (
    <main className="mx-auto w-full max-w-[1600px] px-4 py-5 sm:px-6 sm:py-6 lg:px-8">
      <header className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="max-w-3xl">
          <p className="text-[11px] font-semibold uppercase tracking-[0.14em] text-neutral-400">
            {t('rules.eyebrow')}
          </p>
          <h1 className="mt-1.5 text-[24px] font-semibold tracking-tight text-neutral-900">{t('rules.title')}</h1>
          <p className="mt-2 text-[13px] leading-6 text-neutral-500">{t('rules.intro')}</p>
        </div>
        <button type="button" onClick={() => void initializeDemo()} disabled={seeding} className={secondaryButton}>
          {seeding
            ? <Loader2 aria-hidden="true" className="h-3.5 w-3.5 animate-spin motion-reduce:animate-none" />
            : <Sparkles aria-hidden="true" className="h-3.5 w-3.5" />}
          {seeding ? t('rules.seeding') : t('rules.seed')}
        </button>
      </header>

      <input ref={fileRef} type="file" multiple accept=".md,.markdown" className="hidden" onChange={(event) => void upload(event.target.files)} />
      <input
        ref={folderRef}
        type="file"
        multiple
        /* @ts-expect-error webkitdirectory lets users pick a folder */
        webkitdirectory=""
        directory=""
        className="hidden"
        onChange={(event) => void upload(event.target.files)}
      />

      {message && <Notice tone={noticeTone}>{message}</Notice>}

      <section className="mt-5 overflow-hidden rounded-xl border border-line bg-surface-raised shadow-sm">
        <div className="flex flex-col gap-4 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
          <div className="flex min-w-0 items-start gap-3">
            <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-neutral-100 text-neutral-600">
              <FileUp aria-hidden="true" className="h-3.5 w-3.5" />
            </div>
            <div className="min-w-0">
              <h2 className="text-[12px] font-semibold text-neutral-800">{t('rules.importTitle')}</h2>
              <p className="mt-1 text-[11px] leading-5 text-neutral-500">{t('rules.fileHint')}</p>
            </div>
          </div>
          <div className="flex shrink-0 flex-wrap gap-2 pl-11 sm:pl-0">
            <button type="button" onClick={() => folderRef.current?.click()} disabled={uploading} className={secondaryButton}>
              <FolderUp aria-hidden="true" className="h-3.5 w-3.5" />
              {t('rules.uploadFolder')}
            </button>
            <button type="button" onClick={() => fileRef.current?.click()} disabled={uploading} className={primaryButton}>
              {uploading
                ? <Loader2 aria-hidden="true" className="h-3.5 w-3.5 animate-spin motion-reduce:animate-none" />
                : <FileUp aria-hidden="true" className="h-3.5 w-3.5" />}
              {uploading ? t('rules.uploading') : t('rules.selectFiles')}
            </button>
          </div>
        </div>
        <div className="flex flex-col gap-2 border-t border-line bg-neutral-50/60 px-4 py-3 sm:flex-row sm:items-center sm:px-5">
          <span className="shrink-0 text-[10px] font-medium text-neutral-500">{t('rules.templateTitle')}</span>
          <div className="flex min-w-0 flex-wrap gap-2 sm:ml-auto">
            <TemplateLink href="/rule-templates/layout.md" label={t('rules.layoutTemplate')} />
            <TemplateLink href="/rule-templates/element.md" label={t('rules.elementTemplate')} />
            <TemplateLink href="/rule-templates/atomic-rule.md" label={t('rules.atomicTemplate')} />
          </div>
        </div>
      </section>

      {attentionJob && (
        <section aria-label={t('rules.latestJob')} className="mt-4 overflow-hidden rounded-xl border border-line bg-surface-raised shadow-sm">
          <JobRow job={attentionJob} />
        </section>
      )}

      <section className="mt-5 rounded-2xl border border-line bg-surface-raised p-4 shadow-sm sm:p-5">
        <div className="flex min-w-0 items-center justify-between gap-3 border-b border-line pb-4">
          <div className="flex min-w-0 items-start gap-2.5">
            <Check aria-hidden="true" className="mt-0.5 h-4 w-4 shrink-0 text-emerald-600" />
            <div className="min-w-0">
              <div className="flex min-w-0 flex-wrap items-center gap-2">
                <h2 className="text-pretty text-[14px] font-semibold text-neutral-900">{t('rules.published')}</h2>
                {library && (
                  <span className="shrink-0 rounded-full bg-emerald-50 px-2 py-0.5 text-[9px] font-medium text-emerald-700">
                    {t('rules.statusDone')}
                  </span>
                )}
              </div>
              <p className="mt-1 text-[11px] leading-5 text-neutral-500">{t('rules.publishedDescription')}</p>
            </div>
          </div>
          {library && (
            <span className="shrink-0 text-[10px] font-medium tabular-nums text-neutral-500">
              {t('rules.activeCount', { count: library.items.length })}
            </span>
          )}
        </div>

        {!library ? (
          <div className="flex min-h-40 items-center justify-center gap-2 text-[12px] text-neutral-400">
            <Loader2 aria-hidden="true" className="h-4 w-4 animate-spin motion-reduce:animate-none" />
            {t('common.loading')}
          </div>
        ) : (
          <>
            <div className="mt-4 flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
              <div className="flex max-w-full gap-1 overflow-x-auto rounded-xl bg-neutral-100 p-1" role="tablist" aria-label={t('rules.filterLabel')}>
                {filters.map((item) => (
                  <button
                    key={item.value}
                    type="button"
                    role="tab"
                    aria-selected={filter === item.value}
                    onClick={() => setFilter(item.value)}
                    className={`inline-flex h-9 shrink-0 items-center gap-1.5 whitespace-nowrap rounded-lg px-3 text-[11px] font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400 ${
                      filter === item.value
                        ? 'bg-surface-raised text-neutral-900 shadow-sm'
                        : 'text-neutral-500 hover:text-neutral-800'
                    }`}
                  >
                    {t(item.label)}
                    <span className="text-[9px] text-neutral-400">{item.count}</span>
                  </button>
                ))}
              </div>
              <label className="relative block w-full lg:w-72">
                <Search aria-hidden="true" className="pointer-events-none absolute left-3 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-neutral-400" />
                <span className="sr-only">{t('rules.searchLabel')}</span>
                <input
                  name="rule-search"
                  autoComplete="off"
                  spellCheck={false}
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                  placeholder={t('rules.searchPlaceholder')}
                  className="h-11 w-full rounded-lg border border-line bg-surface pl-9 pr-3 text-[12px] text-neutral-800 outline-none placeholder:text-neutral-400 focus:border-neutral-400 focus:ring-2 focus:ring-neutral-100 sm:h-9"
                />
              </label>
            </div>
            {visibleItems.length === 0 ? (
              <EmptyState icon={FileText} text={normalizedQuery ? t('rules.searchEmpty') : t('rules.categoryEmpty')} />
            ) : (
              <div className="mt-4 grid gap-3 md:grid-cols-2">
                {visibleItems.map((item) => <SourceCard key={item.id} item={item} />)}
              </div>
            )}
            <details className="mt-4 border-t border-line pt-3 text-[10px] text-neutral-500">
              <summary className="w-fit cursor-pointer select-none rounded-sm font-medium hover:text-neutral-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400">
                {t('rules.versionDetails')}
              </summary>
              <div className="mt-2 grid gap-1 rounded-lg bg-neutral-50 px-3 py-2 sm:grid-cols-[auto_minmax(0,1fr)] sm:gap-x-3">
                <span>{t('rules.revisionId')}</span>
                <code translate="no" className="min-w-0 break-all text-neutral-700">{library.revisionId}</code>
                <span>{t('rules.contentHash')}</span>
                <code translate="no" className="min-w-0 break-all text-neutral-700">{library.revisionHash}</code>
              </div>
            </details>
          </>
        )}
      </section>

      {historyJobs.length > 0 && (
        <section className="mt-4 overflow-hidden rounded-xl border border-line bg-surface-raised shadow-sm">
          <div className="flex items-center justify-between gap-3 border-b border-line bg-neutral-50/60 px-4 py-3 sm:px-5">
            <div>
              <h2 className="text-[12px] font-semibold text-neutral-800">{t('rules.jobs')}</h2>
              <p className="mt-0.5 text-[10px] text-neutral-500">{t('rules.jobsDescription')}</p>
            </div>
            <span className="shrink-0 text-[10px] text-neutral-400">{t('rules.jobCount', { count: historyJobs.length })}</span>
          </div>
          <div className="divide-y divide-line">
            {historyJobs.slice(0, 5).map((job) => <JobRow key={job.id} job={job} />)}
          </div>
        </section>
      )}

    </main>
  );
}

function Notice({ tone, children }: { tone: NoticeTone; children: React.ReactNode }) {
  const Icon = tone === 'success' ? CheckCircle2 : CircleAlert;
  const color = tone === 'error'
    ? 'border-red-200 bg-red-50 text-red-700'
    : tone === 'success'
      ? 'border-emerald-200 bg-emerald-50 text-emerald-700'
      : 'border-line bg-neutral-50 text-neutral-700';
  return (
    <div role={tone === 'error' ? 'alert' : 'status'} className={`mt-4 flex items-start gap-2 rounded-xl border px-3.5 py-3 text-[12px] leading-5 ${color}`}>
      <Icon aria-hidden="true" className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      <span className="min-w-0 break-words">{children}</span>
    </div>
  );
}

function TemplateLink({ href, label }: { href: string; label: string }) {
  const { t } = useLocale();
  return (
    <a
      href={href}
      download
      className="inline-flex h-8 items-center gap-2 rounded-lg border border-line bg-surface-raised px-2.5 text-[10px] font-medium text-neutral-600 transition-colors hover:border-neutral-300 hover:bg-neutral-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
    >
      <FileText aria-hidden="true" className="h-3 w-3 text-neutral-400" />
      <span>{label}</span>
      <span className="text-neutral-400">{t('rules.download')}</span>
    </a>
  );
}

function JobRow({ job }: { job: JobItem }) {
  const { locale, t } = useLocale();
  const tone = job.status === 'failed'
    ? 'bg-red-50 text-red-700'
    : job.status === 'done'
      ? 'bg-emerald-50 text-emerald-700'
      : 'bg-sky-50 text-sky-700';
  const fileNames = (job.files ?? []).join(t('common.listSeparator'));
  const created = job.createdAt
    ? new Intl.DateTimeFormat(locale === 'zh' ? 'zh-CN' : 'en', {
        month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
      }).format(new Date(job.createdAt))
    : '';
  return (
    <article className="grid gap-3 px-5 py-3.5 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-center sm:px-6">
      <div className="min-w-0">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="truncate font-mono text-[11px] font-medium text-neutral-700">{job.id}</span>
          <span className={`shrink-0 rounded-full px-2 py-0.5 text-[9px] font-medium ${tone}`}>{jobStatusLabel(job.status, t)}</span>
        </div>
        <p className="mt-1 truncate text-[11px] text-neutral-500" title={fileNames}>{fileNames}</p>
        {job.error && <p className="mt-1 text-[11px] leading-5 text-red-600">{job.error}</p>}
      </div>
      <div className="flex items-center gap-3 text-[10px] text-neutral-400 sm:justify-end">
        <span>{t('rules.parsedCount', { count: job.parsed })}</span>
        {created && <time dateTime={job.createdAt}>{created}</time>}
      </div>
    </article>
  );
}

function EmptyState({ icon: Icon, text }: { icon: LucideIcon; text: string }) {
  return (
    <div className="flex flex-col items-center justify-center px-6 py-9 text-center">
      <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-neutral-100 text-neutral-400">
        <Icon aria-hidden="true" className="h-4 w-4" />
      </div>
      <p className="mt-2 max-w-xl text-[11px] leading-5 text-neutral-400">{text}</p>
    </div>
  );
}

function SourceCard({ item }: { item: SourceRuleItem }) {
  const { t } = useLocale();
  const separator = t('common.listSeparator');
  const presentation = rulePresentation(item, t);
  const relations = [
    { label: t('rules.useCases'), values: presentation.useCases ?? item.appliesTo ?? [], tone: 'text-neutral-700' },
    { label: t('rules.includes'), values: presentation.includes ?? item.requires ?? [], tone: 'text-neutral-700' },
    { label: t('rules.notFor'), values: item.notFor ?? [], tone: 'text-amber-700' },
    { label: t('rules.conflictsWith'), values: item.conflictsWith ?? [], tone: 'text-amber-700' },
  ];
  const kindLabel = item.kind === 'layout'
    ? t('rules.filterLayout')
    : item.kind === 'element'
      ? t('rules.filterElement')
      : t('rules.filterAtomic');
  const KindIcon = item.kind === 'layout' ? Layers3 : item.kind === 'element' ? FileText : Atom;
  return (
    <article className="min-w-0 rounded-xl border border-line bg-surface p-4 transition-colors hover:border-neutral-300">
      <div className="flex min-w-0 items-start justify-between gap-3">
        <div className="min-w-0">
          <span className="inline-flex items-center gap-1 rounded-md bg-neutral-100 px-1.5 py-0.5 text-[9px] font-medium text-neutral-500">
            <KindIcon aria-hidden="true" className="h-2.5 w-2.5" />{kindLabel}
          </span>
          <h3 className="mt-2 min-w-0 text-pretty break-words text-[14px] font-semibold leading-5 text-neutral-900">{presentation.title}</h3>
        </div>
      </div>
      <p className="mt-2 text-pretty text-[11px] leading-5 text-neutral-500">{presentation.description}</p>
      {relations.some((relation) => relation.values.length > 0) && (
        <dl className="mt-3 grid gap-1.5 border-t border-line pt-3 text-[10px] leading-4">
          {relations.filter((relation) => relation.values.length > 0).map((relation) => (
            <div key={relation.label} className="flex gap-2">
              <dt className="shrink-0 text-neutral-400">{relation.label}</dt>
              <dd className={`min-w-0 break-words ${relation.tone}`}>{relation.values.join(separator)}</dd>
            </div>
          ))}
        </dl>
      )}
      <details className="mt-3 border-t border-line pt-3">
        <summary className="w-fit cursor-pointer select-none rounded-sm text-[10px] font-medium text-neutral-500 hover:text-neutral-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400">
          {t('rules.technicalDetails')}
        </summary>
        <dl className="mt-2 grid gap-1 rounded-t-lg bg-neutral-50 px-3 pt-3 text-[10px] leading-4 sm:grid-cols-[auto_minmax(0,1fr)] sm:gap-x-3">
          <dt className="text-neutral-400">{t('rules.ruleId')}</dt>
          <dd translate="no" className="min-w-0 break-all font-mono text-neutral-600">{item.id}</dd>
          <dt className="text-neutral-400">{t('rules.version')}</dt>
          <dd translate="no" className="font-mono text-neutral-600">v{item.version}</dd>
        </dl>
        <p className="bg-neutral-50 px-3 pt-3 text-[9px] font-medium text-neutral-400">{t('rules.sourceMarkdown')}</p>
        <pre translate="no" className="max-h-64 overflow-auto rounded-b-lg bg-neutral-50 p-3 font-mono text-[10px] leading-5 text-neutral-600 whitespace-pre-wrap">{item.content}</pre>
      </details>
    </article>
  );
}

function rulePresentation(item: SourceRuleItem, t: Translate) {
  switch (item.id) {
    case 'layout.p01.public-demo-summary':
      return {
        title: t('rules.demoSummaryTitle'),
        description: t('rules.demoSummaryDescription'),
        useCases: [t('rules.demoSummaryUseCases')],
        includes: [t('rules.demoSummaryIncludes')],
      };
    case 'layout.p02.information-action':
      return {
        title: t('rules.demoInformationTitle'),
        description: t('rules.demoInformationDescription'),
        useCases: [t('rules.demoInformationUseCases')],
        includes: [t('rules.demoInformationIncludes')],
      };
    case 'layout.p03.repeated-item-list':
      return {
        title: t('rules.demoListTitle'),
        description: t('rules.demoListDescription'),
        useCases: [t('rules.demoListUseCases')],
        includes: [t('rules.demoListIncludes')],
      };
    case 'rule.public-demo.foundation':
      return {
        title: t('rules.demoFoundationTitle'),
        description: t('rules.demoFoundationDescription'),
        useCases: [t('rules.demoFoundationUseCases')],
        includes: [t('rules.demoFoundationIncludes')],
      };
    default:
      return {
        title: item.title || item.id,
        description: item.summary,
        useCases: item.appliesTo,
        includes: item.requires,
      };
  }
}

function jobStatusLabel(status: string, t: Translate): string {
  switch (status) {
    case 'done': return t('rules.statusDone');
    case 'failed': return t('rules.statusFailed');
    case 'running':
    case 'processing': return t('rules.statusProcessing');
    case 'queued': return t('rules.statusQueued');
    default: return status;
  }
}
