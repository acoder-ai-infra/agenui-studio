'use client';

import dynamic from 'next/dynamic';
import Link from 'next/link';
import {
  Suspense,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ClipboardEvent as ReactClipboardEvent,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from 'react';
import { useSearchParams } from 'next/navigation';
import {
  SendHorizontal,
  Check,
  X,
  Loader2,
  CircleHelp,
  TriangleAlert,
  LayoutTemplate,
  Code2,
  PanelRightClose,
  PanelRightOpen,
  Download,
  Paperclip,
  Image as ImageIcon,
  Smartphone,
  ChevronDown,
  Database,
  MousePointerClick,
  Cpu,
  Layers3,
  Package,
  Workflow,
} from 'lucide-react';
import { artifactContentURL, sessionApi, sessionReplay, sessionPresentation, sessionProgress, sessionRunStatus, resumeSessionRun, isRecoverableRunStatus, resolveControl, decodeAGenUIResult, type CanonicalFrame, type ChatAttachmentView, type ControlAnswer, type ControlQuestionView, type RunCursorMap, type StreamOutcome } from "@/lib/generate-api";
import {
  applyProgressFrame,
  applyHarnessProcess,
  createAssistantTextReplayGuard,
  finalizeProgressTimeline,
  runFailureCode,
  formatProgressOutput,
  groupProgressActivities,
  inferRunningProgressStage,
  type ProgressActivityGroup,
  type ProgressStageGroup,
  type ProgressStatus,
  type ProgressTimeline,
} from '@/lib/agenui-console-state';
import {
  isNextStepsProcess,
  nextStepsFromProcess,
  nextStepSupportingText,
  nextStepsToolName,
  type NextStepsPlan,
} from '@/lib/agenui-next-steps';
import { useTheme } from '@/lib/theme/ThemeContext';
import { useLocale, type MessageKey, type Translate } from '@/lib/i18n/LocaleContext';
import type { ColorScheme } from '@/lib/agenui-types';
import { cn } from '@/lib/utils';
import { notifyRecentChatsChanged } from '@/lib/recent-chats';
import {
  isSupportedReferenceImage,
  MAX_REFERENCE_IMAGES,
  referenceImagesFromClipboard,
} from '@/lib/reference-images';
import { PhoneShellWrapper } from '../composer/components/PhoneShellWrapper';
import {
  analyzeImplementation,
  buildRuntimePackage,
  type ImplementationArtifacts,
  type ImplementationInsight,
} from '@/lib/agenui-implementation';

const A2uiPreview = dynamic(
  () => import('../composer/components/A2uiPreview').then((mod) => mod.A2uiPreview),
  { ssr: false, loading: () => <div className="min-h-[300px]" /> },
);

const MonacoEditor = dynamic(() => import('@monaco-editor/react'), {
  ssr: false,
  loading: () => <div className="h-full animate-pulse rounded bg-neutral-50" />,
});

const TABS = ['createSurface', 'updateComponents', 'updateDataModel'] as const;
type Tab = (typeof TABS)[number];
type ImplementationView = 'structure' | 'data' | 'actions' | 'binding' | 'runtime';
type DSLImplementationView = Extract<ImplementationView, 'structure' | 'data' | 'actions'>;
type ImplementationSection = 'dsl' | 'binding' | 'delivery';

interface WorkbenchPackage extends ImplementationArtifacts {
  protocol: Record<string, unknown>[];
}

interface PublicationTargetStatus {
  configured: boolean;
  enabled: boolean;
  kind?: 'callback' | 'mq';
  endpoint?: string;
}

interface SessionRecoveryTarget {
  sessionId: string;
  runId: string;
  assistantLineId: number;
  answeredControlIDs: string[];
  existingText: string;
}

type PanelSizes = { chat: number; preview: number; protocol: number };
type PanelDivider = 'chat-preview' | 'preview-protocol';
type PreviewDeviceID = 'iphone-15-pro' | 'iphone-15-pro-max' | 'pixel-8' | 'compact-android';

interface PreviewDevice {
  id: PreviewDeviceID;
  /** 商品型号名不翻译；只有通用描述型号名走 labelKey。 */
  label?: string;
  labelKey?: MessageKey;
  width: number;
  height: number;
  platform: 'ios' | 'android';
}

const DEFAULT_PANEL_SIZES: PanelSizes = { chat: 36, preview: 36, protocol: 28 };
const PANEL_SIZE_STORAGE_KEY = 'agenui.workbench.panel-sizes.v1';
const PREVIEW_DEVICE_STORAGE_KEY = 'agenui.workbench.preview-device.v1';
const PREVIEW_DEVICES: readonly PreviewDevice[] = [
  { id: 'iphone-15-pro', label: 'iPhone 15 Pro', width: 393, height: 852, platform: 'ios' },
  { id: 'iphone-15-pro-max', label: 'iPhone 15 Pro Max', width: 430, height: 932, platform: 'ios' },
  { id: 'pixel-8', label: 'Pixel 8', width: 412, height: 915, platform: 'android' },
  { id: 'compact-android', labelKey: 'home.deviceCompactAndroid', width: 360, height: 800, platform: 'android' },
];

function resizePanelSizes(sizes: PanelSizes, divider: PanelDivider, delta: number): PanelSizes {
  if (divider === 'chat-preview') {
    const combined = sizes.chat + sizes.preview;
    const chat = Math.min(Math.max(sizes.chat + delta, 22), combined - 25);
    return { ...sizes, chat, preview: combined - chat };
  }
  const combined = sizes.preview + sizes.protocol;
  const preview = Math.min(Math.max(sizes.preview + delta, 25), combined - 22);
  return { ...sizes, preview, protocol: combined - preview };
}

const TAB_META: Record<Tab, { labelKey: MessageKey; descriptionKey: MessageKey }> = {
  createSurface: {
    labelKey: 'home.tabSurface',
    descriptionKey: 'home.tabSurfaceDesc',
  },
  updateComponents: {
    labelKey: 'home.tabComponents',
    descriptionKey: 'home.tabComponentsDesc',
  },
  updateDataModel: {
    labelKey: 'home.tabDataModel',
    descriptionKey: 'home.tabDataModelDesc',
  },
};

/** Error codes Harness labels itself; every other code falls back to generic. */
const RUN_FAILURE_KEYS: Record<string, MessageKey> = {
  SUB_AGENT_TARGET_RESOLVE_FAILED: 'home.runFailureSubAgent',
  TOOL_SCHEMA_VALIDATION_FAILED: 'home.runFailureToolSchema',
  CONTEXT_BUILD_FAILED: 'home.runFailureContextBuild',
  RESUME_FAILED: 'home.runFailureResume',
  MODEL_PROVIDER_4XX: 'home.runFailureModelRequest',
  MODEL_PROVIDER_5XX: 'home.runFailureModelUnavailable',
  MODEL_RATE_LIMITED: 'home.runFailureModelRateLimited',
  MODEL_TIMEOUT: 'home.runFailureModelTimeout',
};

/**
 * Render a run failure in the active locale.
 *
 * The server sentence is in the server locale, so it is replaced whenever the
 * stable code can be recovered. A text without a code is passed through: it is
 * still the most accurate thing we have.
 */
function runFailureText(raw: string, t: Translate): string {
  const code = runFailureCode(raw);
  if (!code) return raw;
  return t('home.runFailure', {
    label: t(RUN_FAILURE_KEYS[code] ?? 'home.runFailureGeneric'),
    code,
  });
}

/**
 * Stage and step captions of the execution process.
 *
 * Harness labels both in the server locale, but every frame also carries a
 * stable identity, so the console renders its own caption once it recognises
 * the identity. An unlisted identity keeps the server wording: a newly added
 * tool then reads as the server named it instead of going blank.
 */
const PROCESS_STAGE_KEYS: Record<string, MessageKey> = {
  content_contract: 'process.stage.content_contract',
  ui_generation: 'process.stage.ui_generation',
  data_binding: 'process.stage.data_binding',
  reasoning: 'process.stage.reasoning',
  execution: 'process.stage.execution',
};

const PROCESS_ACTIVITY_KEYS: Record<string, MessageKey> = {
  search_developer_apis: 'process.activity.search_developer_apis',
  list_developer_operators: 'process.activity.list_developer_operators',
  agenui_execute_operator: 'process.activity.agenui_execute_operator',
  agenui_read_design_knowledge: 'process.activity.agenui_read_design_knowledge',
  agenui_submit_content_contract: 'process.activity.agenui_submit_content_contract',
  agenui_preflight_capabilities: 'process.activity.agenui_preflight_capabilities',
  agenui_resolve_design_edit: 'process.activity.agenui_resolve_design_edit',
  agenui_submit_edit_contract: 'process.activity.agenui_submit_edit_contract',
  agenui_prepare_binding_edit: 'process.activity.agenui_prepare_binding_edit',
  agenui_workspace: 'process.activity.agenui_workspace',
};

function processStageLabel(stage: ProgressStageGroup, t: Translate): string {
  const key = PROCESS_STAGE_KEYS[stage.stageId];
  if (key) return t(key);
  return stage.label || t('process.stage.execution');
}

function processActivityLabel(activity: ProgressActivityGroup, t: Translate): string {
  const key = PROCESS_ACTIVITY_KEYS[activity.activityKey];
  if (key) return t(key);
  return activity.label || t('process.activity.fallback');
}

const DSL_IMPLEMENTATION_VIEWS: readonly DSLImplementationView[] = ['structure', 'data', 'actions'];
const IMPLEMENTATION_SECTIONS: readonly ImplementationSection[] = ['dsl', 'binding', 'delivery'];
const IMPLEMENTATION_META: Record<ImplementationView, { labelKey: MessageKey; descriptionKey: MessageKey }> = {
  structure: { labelKey: 'home.viewStructure', descriptionKey: 'home.viewStructureDesc' },
  data: { labelKey: 'home.viewData', descriptionKey: 'home.viewDataDesc' },
  actions: { labelKey: 'home.viewActions', descriptionKey: 'home.viewActionsDesc' },
  binding: { labelKey: 'home.viewBinding', descriptionKey: 'home.viewBindingDesc' },
  runtime: { labelKey: 'home.viewRuntime', descriptionKey: 'home.viewRuntimeDesc' },
};

const IMPLEMENTATION_SECTION_META: Record<ImplementationSection, { labelKey: MessageKey; descriptionKey: MessageKey }> = {
  dsl: { labelKey: 'home.sectionDsl', descriptionKey: 'home.sectionDslDesc' },
  binding: { labelKey: 'home.sectionBinding', descriptionKey: 'home.sectionBindingDesc' },
  delivery: { labelKey: 'home.sectionDelivery', descriptionKey: 'home.sectionDeliveryDesc' },
};

function isImplementationView(value: string | null): value is ImplementationView {
  return value === 'structure' || value === 'data' || value === 'actions' || value === 'binding' || value === 'runtime';
}

function isDSLImplementationView(value: ImplementationView): value is DSLImplementationView {
  return value === 'structure' || value === 'data' || value === 'actions';
}

function implementationSectionForView(view: ImplementationView): ImplementationSection {
  if (view === 'binding') return 'binding';
  if (view === 'runtime') return 'delivery';
  return 'dsl';
}

function adjacentTab<T extends string>(values: readonly T[], current: T, key: string): T | null {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(key)) return null;
  if (key === 'Home') return values[0];
  if (key === 'End') return values[values.length - 1];
  const offset = key === 'ArrowRight' ? 1 : -1;
  const index = values.indexOf(current);
  return values[(index + offset + values.length) % values.length];
}

function downloadJSON(filename: string, value: unknown): void {
  const blob = new Blob([JSON.stringify(value, null, 2)], { type: 'application/json;charset=utf-8' });
  const href = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = href;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(href);
}

function downloadStem(sessionID: string): string {
  const normalized = sessionID.replace(/[^a-zA-Z0-9_-]+/g, '-').replace(/^-+|-+$/g, '');
  return normalized || 'agenui';
}

interface ChatLine {
  id: number;
  role: 'user' | 'assistant';
  text?: string;
  commentary?: string;
  attachments?: ChatAttachmentView[];
  chips?: {
    id: string;
    label: string;
    detail?: string;
    warning?: string;
    state: 'run' | 'done' | 'err';
  }[];
  artifacts?: string[];
  error?: string;
  progress?: ProgressTimeline;
  nextSteps?: NextStepsPlan;
  turnCompleted?: boolean;
  controlAnswered?: boolean;
  confirmation?: {
    question: string;
    options: { id: string; label: string; type: string; placeholder?: string }[];
  };
  control?: {
    id: string;
    runId: string;
    questions: ControlQuestionView[];
  };
}

let lineSeq = 1;

// 过程分组元数据来自 Harness 原生 data-process；前端做通用聚合渲染，并按帧里
// 的稳定标识渲染阶段与步骤标题。

function ProgressStatusIcon({ status }: { status: ProgressStatus }) {
  if (status === 'completed') return <Check className="h-4 w-4 shrink-0 text-emerald-600" />;
  if (status === 'failed') return <X className="h-4 w-4 shrink-0 text-red-500" />;
  if (status === 'waiting') return <CircleHelp className="h-4 w-4 shrink-0 text-amber-500" />;
  if (status === 'cancelled') return <X className="h-4 w-4 shrink-0 text-neutral-400" />;
  return <Loader2 className="h-4 w-4 shrink-0 animate-spin text-progress" />;
}

function ProcessDisclosure({ initialOpen, className, children }: {
  initialOpen: boolean;
  className: string;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(initialOpen);
  return (
    <details
      open={open}
      onToggle={(event) => setOpen(event.currentTarget.open)}
      className={className}
    >
      {children}
    </details>
  );
}

function ProgressRows({ timeline, rootRunActive }: {
  timeline: ProgressTimeline;
  rootRunActive: boolean;
}) {
  const { t } = useLocale();
  const stages = groupProgressActivities(timeline.activities, timeline.subagents ?? []);
  if (stages.length === 0) return null;
  const inferredRunningStageId = inferRunningProgressStage(stages, rootRunActive);
  return (
    <div className="mt-4 border-t border-neutral-100 pt-3">
      <div className="flex items-center gap-2 px-0.5">
        <Layers3 aria-hidden="true" className="h-3.5 w-3.5 text-neutral-400" />
        <span className="text-[11px] font-semibold text-neutral-700">{t('home.processTitle')}</span>
        <span className="ml-auto text-[9px] text-neutral-400">{t('home.processStages', { count: stages.length })}</span>
      </div>
      <div className="mt-1 divide-y divide-neutral-100">
      {stages.map((stage) => {
        const inferredRunning = stage.stageId === inferredRunningStageId;
        const displayStatus: ProgressStatus = inferredRunning ? 'in_progress' : stage.status;
        const active = displayStatus === 'in_progress' || displayStatus === 'waiting';
        const failed = displayStatus === 'failed';
        const callCount = stage.activities.reduce((total, activity) => total + activity.count, 0);
        const activeSubagents = stage.subagents.filter((subagent) => (
          subagent.status === 'in_progress' || subagent.status === 'waiting'
        ));
        const countLabel = callCount > 0
          ? active
            ? t('home.processCountActive', { count: callCount })
            : t('home.countItems', { count: callCount })
          : active
            ? t('home.processPreparing')
            : t('home.processSubtask');
        return (
          <ProcessDisclosure
            key={stage.stageId}
            initialOpen={active || failed}
            className="group/stage py-2.5"
          >
            <summary className="flex cursor-pointer list-none items-center gap-2 text-[12px] font-semibold">
              <ProgressStatusIcon status={displayStatus} />
              <span className={failed ? 'text-red-600' : active ? 'text-info' : 'text-neutral-800'}>
                {processStageLabel(stage, t)}
              </span>
              <span className="ml-auto text-[9px] font-normal text-neutral-400">{countLabel}</span>
              <span className="text-neutral-300 transition-transform group-open/stage:rotate-180">⌄</span>
            </summary>
            <div className="ml-2 mt-2 space-y-1 border-l-2 border-neutral-100 pl-4">
              {stage.activities.map((activity) => {
                const activityActive = activity.status === 'in_progress' || activity.status === 'waiting';
                const activityFailed = activity.status === 'failed';
                // A grouped activity has several distinct results, so showing only
                // the last one would be misleading. Its individual calls remain in
                // the debug transcript; a one-off tool can expose its result here.
                const resultInvocation = activity.count === 1
                  ? activity.invocations.find((invocation) => (
                      invocation.output !== undefined || Boolean(invocation.output_ref)
                    ))
                  : undefined;
                const formattedResult = resultInvocation?.output !== undefined
                  ? formatProgressOutput(resultInvocation.output)
                  : undefined;
                const resultRef = resultInvocation?.output_ref;
                const hasResult = Boolean(formattedResult || resultRef);
                const hasDetails = Boolean(activity.summary) || activity.details.length > 0 || hasResult;
                return (
                  <ProcessDisclosure
                    key={activity.activityKey}
                    initialOpen={false}
                    className="group/activity py-1"
                  >
                    <summary className={`flex list-none items-center gap-2 text-[11px] font-medium ${hasDetails ? 'cursor-pointer' : 'cursor-default'}`}>
                      <ProgressStatusIcon status={activity.status} />
                      <span className={activityFailed ? 'text-red-600' : activityActive ? 'text-info' : 'text-neutral-700'}>
                        {processActivityLabel(activity, t)}{activity.count > 1 ? ` ×${activity.count}` : ''}
                      </span>
                      {hasResult && (
                        <span className="ml-auto text-[9px] font-normal text-info">{t('process.resultAvailable')}</span>
                      )}
                      {hasDetails && (
                        <span className={`${hasResult ? '' : 'ml-auto'} text-neutral-300 transition-transform group-open/activity:rotate-180`}>⌄</span>
                      )}
                    </summary>
                    {hasDetails && (
                      <div className="ml-6 mt-1 space-y-1">
                        {activity.summary && <p className="text-[10px] leading-5 text-neutral-500">{activity.summary}</p>}
                        {activity.details.map((detail, index) => (
                          <div key={`${detail.label}-${index}`} className="grid grid-cols-[72px_1fr] gap-2 text-[10px] leading-5">
                            <span className="text-neutral-400">{detail.label}</span>
                            <span className="text-neutral-500">{detail.value}</span>
                          </div>
                        ))}
                        {hasResult && (
                          <div className="mt-1 border-l border-neutral-200 pl-2.5">
                            <div className="text-[9px] font-medium leading-4 text-neutral-400">
                              {t('process.resultTitle')}
                            </div>
                            {formattedResult && (
                              <pre className="mt-1 max-h-48 max-w-full overflow-auto whitespace-pre-wrap break-words font-mono text-[9px] leading-4 text-neutral-600 [overflow-wrap:anywhere]">
                                {formattedResult.text}
                              </pre>
                            )}
                            {resultRef && (
                              <a
                                href={artifactContentURL(resultRef)}
                                target="_blank"
                                rel="noreferrer"
                                className="mt-1 inline-flex text-[9px] leading-4 text-info hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-info"
                              >
                                {t('process.openFullResult')}
                              </a>
                            )}
                          </div>
                        )}
                      </div>
                    )}
                  </ProcessDisclosure>
                );
              })}
              {(activeSubagents.length > 0 || inferredRunning) && (
                <div role="status" className="flex items-center gap-2 py-1 text-[10px] text-info">
                  <Loader2 aria-hidden="true" className="h-3.5 w-3.5 animate-spin" />
                  <span>{inferredRunning
                    ? t('home.agentContinuing')
                    : callCount > 0
                      ? t('home.subtasksContinuing')
                      : t('home.subtasksPreparing')}</span>
                </div>
              )}
              {stage.activities.length === 0 && activeSubagents.length === 0 && !inferredRunning && (
                <div className={`py-1 text-[10px] ${failed ? 'text-red-500' : 'text-neutral-500'}`}>
                  {failed
                    ? t('home.subtasksFailed')
                    : stage.status === 'cancelled'
                      ? t('home.subtasksCancelled')
                      : t('home.subtasksCompleted')}
                </div>
              )}
              </div>
          </ProcessDisclosure>
        );
      })}
      </div>
    </div>
  );
}

function NextStepsCard({ plan, disabled, onSelect }: {
  plan: NextStepsPlan;
  disabled: boolean;
  onSelect: (prompt: string) => void;
}) {
  const { t } = useLocale();
  return (
    <section className="mt-3 rounded-xl bg-sky-50/80 px-3 py-3" aria-label={t('home.nextStepsTitle')}>
      <div className="flex items-center gap-2.5">
        <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-info text-info-fg shadow-sm">
          <MousePointerClick aria-hidden="true" className="h-3.5 w-3.5" />
        </span>
        <span className="min-w-0">
          <span className="block text-[13px] font-semibold leading-5 text-neutral-900">{t('home.nextStepsTitle')}</span>
          <span className="block text-[10px] leading-4 text-neutral-500">{t('home.nextStepsPrompt')}</span>
        </span>
      </div>
      <div className="mt-2.5 divide-y divide-sky-100 border-t border-sky-100">
        {plan.items.map((item) => {
          const description = nextStepSupportingText(item);
          return (
            <button
              key={item.id}
              type="button"
              disabled={disabled}
              onClick={() => onSelect(item.prompt)}
              aria-label={t('home.nextStepPrefill', { label: item.label })}
              className="group flex w-full items-start gap-3 py-2.5 text-left transition-colors hover:text-info focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-info disabled:cursor-not-allowed disabled:opacity-50"
            >
              <span className="min-w-0 flex-1">
                <span className="block break-words text-[12px] font-semibold leading-5 text-neutral-800 group-hover:text-info">
                  {item.label}
                </span>
                {description && (
                  <span className="mt-0.5 block break-words text-[10px] leading-4 text-neutral-500">
                    {description}
                  </span>
                )}
              </span>
              <span aria-hidden="true" className="mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-surface-raised text-[12px] font-semibold text-info shadow-sm transition-transform group-hover:translate-x-0.5">→</span>
            </button>
          );
        })}
      </div>
    </section>
  );
}

function ControlFreeInput({ onSend, placeholder }: {
  onSend: (text: string) => void;
  placeholder?: string;
}) {
  const { t } = useLocale();
  const [text, setText] = useState('');
  return (
    <div className="mt-2 flex gap-2">
      <input
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && text.trim()) {
            onSend(text.trim());
            setText('');
          }
        }}
        placeholder={placeholder ?? t('home.answerPlaceholder')}
        className="h-7 flex-1 rounded-md border border-sky-200 bg-surface-raised px-2 text-[11px] text-neutral-800 placeholder:text-neutral-500 focus:border-sky-400 focus:outline-none"
      />
      <button
        onClick={() => {
          if (text.trim()) {
            onSend(text.trim());
            setText('');
          }
        }}
        className="h-7 rounded-md border border-sky-300 px-2 text-[11px] text-info hover:bg-sky-100"
      >
        {t('common.send')}
      </button>
    </div>
  );
}

function AskUserCard({
  control,
  disabled,
  onSubmit,
}: {
  control: NonNullable<ChatLine['control']>;
  disabled: boolean;
  onSubmit: (answers: ControlAnswer[]) => void;
}) {
  const { t } = useLocale();
  const [drafts, setDrafts] = useState<Record<string, ControlAnswer>>({});
  const selectedCount = control.questions.filter((question) => {
    const answer = drafts[question.id];
    return Boolean(answer?.optionID || answer?.text?.trim());
  }).length;
  const complete = control.questions.length > 0 && selectedCount === control.questions.length;

  useEffect(() => {
    setDrafts({});
  }, [control.id]);

  return (
    <section className="mt-3 overflow-hidden rounded-xl border border-sky-200 bg-sky-50/70 shadow-sm" aria-label={t('home.askUserTitle')}>
      <div className="flex items-start gap-2.5 border-b border-sky-100 bg-surface-raised/70 px-3.5 py-3">
        <span className="mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-info text-info-fg">
          <CircleHelp aria-hidden="true" className="h-4 w-4" />
        </span>
        <span className="min-w-0">
          <span className="block text-[13px] font-semibold leading-5 text-neutral-900">{t('home.askUserTitle')}</span>
          <span className="block text-[10px] leading-4 text-neutral-500">
            {t('home.askUserHint', { count: control.questions.length })}
          </span>
        </span>
      </div>
      <div className="divide-y divide-sky-100">
        {control.questions.map((question, questionIndex) => {
          const draft = drafts[question.id];
          const inputOption = question.options.find((option) => option.type === 'input');
          const selectOptions = question.options.filter((option) => option.type !== 'input' && option.label);
          return (
            <fieldset key={question.id} disabled={disabled} className="min-w-0 px-3.5 py-3.5">
              <legend className="w-full">
                <span className="flex items-center gap-2 text-[10px] font-semibold text-info">
                  <span className="flex h-5 min-w-5 items-center justify-center rounded-full bg-sky-100 px-1.5 tabular-nums">
                    {questionIndex + 1}
                  </span>
                  {question.header || t('home.askUserQuestion', { index: questionIndex + 1 })}
                </span>
                <span className="mt-1.5 block text-[12px] font-semibold leading-5 text-neutral-800">
                  {question.question}
                </span>
              </legend>
              {selectOptions.length > 0 && (
                <div className="mt-2.5 grid gap-2">
                  {selectOptions.map((option) => {
                    const selected = draft?.optionID === option.id;
                    return (
                      <button
                        key={option.id}
                        type="button"
                        aria-pressed={selected}
                        onClick={() => setDrafts((current) => ({
                          ...current,
                          [question.id]: {
                            questionID: question.id,
                            optionID: option.id,
                            text: option.label,
                          },
                        }))}
                        className={cn(
                          'flex min-h-11 w-full items-start gap-2.5 rounded-lg border px-3 py-2.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-info disabled:cursor-not-allowed disabled:opacity-60',
                          selected
                            ? 'border-info bg-surface-raised shadow-sm'
                            : 'border-sky-100 bg-surface-raised/70 hover:border-sky-300 hover:bg-surface-raised',
                        )}
                      >
                        <span
                          aria-hidden="true"
                          className={cn(
                            'mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded-full border',
                            selected ? 'border-info bg-info' : 'border-neutral-300 bg-surface-raised',
                          )}
                        >
                          {selected && <span className="h-1.5 w-1.5 rounded-full bg-info-fg" />}
                        </span>
                        <span className="min-w-0">
                          <span className="block break-words text-[11px] font-semibold leading-5 text-neutral-800">{option.label}</span>
                          {option.description && (
                            <span className="mt-0.5 block break-words text-[10px] leading-4 text-neutral-500">{option.description}</span>
                          )}
                        </span>
                      </button>
                    );
                  })}
                </div>
              )}
              <label className="mt-2.5 block">
                <span className="text-[10px] font-medium text-neutral-500">{t('home.askUserCustom')}</span>
                <input
                  value={draft?.optionID ? '' : draft?.text ?? ''}
                  onChange={(event) => setDrafts((current) => ({
                    ...current,
                    [question.id]: {
                      questionID: question.id,
                      text: event.target.value,
                    },
                  }))}
                  placeholder={inputOption?.placeholder ?? t('home.customAnswerPlaceholder')}
                  className="mt-1.5 h-10 w-full rounded-lg border border-sky-100 bg-surface-raised px-3 text-[11px] text-neutral-800 placeholder:text-neutral-400 focus:border-info focus:outline-none focus:ring-2 focus:ring-info/15 disabled:cursor-not-allowed disabled:opacity-60"
                />
              </label>
            </fieldset>
          );
        })}
      </div>
      <div className="flex flex-col gap-2 border-t border-sky-100 bg-surface-raised/60 px-3.5 py-3 sm:flex-row sm:items-center sm:justify-between">
        <span aria-live="polite" className="text-[10px] text-neutral-500">
          {t('home.askUserSelected', { selected: selectedCount, total: control.questions.length })}
        </span>
        <button
          type="button"
          disabled={disabled || !complete}
          onClick={() => onSubmit(control.questions.map((question) => drafts[question.id]!))}
          className="min-h-10 rounded-lg bg-info px-4 text-[11px] font-semibold text-info-fg transition-colors hover:bg-info-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-info focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-45"
        >
          {t('home.askUserSubmit')}
        </button>
      </div>
    </section>
  );
}

function ProtocolPendingState({ running, error }: { running: boolean; error?: string }) {
  const { t } = useLocale();
  if (error) {
    return (
      <div className="flex h-full items-center justify-center p-8">
        <div className="max-w-[300px] text-center">
          <div className="mx-auto flex h-11 w-11 items-center justify-center rounded-xl bg-red-50 text-red-600">
            <TriangleAlert aria-hidden="true" className="h-5 w-5" />
          </div>
          <p className="mt-4 text-[13px] font-semibold text-neutral-800">{t('home.dslFailed')}</p>
          <p className="mt-1.5 break-words text-[11px] leading-5 text-neutral-500">{error}</p>
          <p className="mt-3 text-[11px] text-neutral-400">{t('home.dslFailedHint')}</p>
        </div>
      </div>
    );
  }

  if (!running) {
    return (
      <div className="flex h-full items-center justify-center p-8">
        <div className="max-w-[280px] text-center">
          <div className="mx-auto flex h-11 w-11 items-center justify-center rounded-xl bg-neutral-100 text-neutral-600">
            <Code2 aria-hidden="true" className="h-5 w-5" />
          </div>
          <p className="mt-4 text-[13px] font-semibold text-neutral-800">{t('home.dslWaiting')}</p>
          <p className="mt-1.5 text-[11px] leading-5 text-neutral-500">
            {t('home.dslWaitingHint')}
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="h-full overflow-y-auto p-5">
      <div className="flex items-start gap-3 rounded-xl border border-sky-200 bg-sky-50 p-3.5">
        <Loader2 aria-hidden="true" className="mt-0.5 h-4 w-4 shrink-0 animate-spin text-sky-600 motion-reduce:animate-none" />
        <div>
          <p className="text-[12px] font-semibold text-sky-800">{t('home.dslBuilding')}</p>
          <p className="mt-0.5 text-[10px] leading-4 text-sky-700">{t('home.dslBuildingHint')}</p>
        </div>
      </div>

      <div className="mt-5 space-y-2.5">
        {TABS.map((tab) => (
          <div key={tab} className="rounded-lg border border-neutral-200/80 bg-surface-raised px-3 py-2.5">
            <div className="flex items-center gap-2">
              <span className="h-1.5 w-1.5 shrink-0 animate-pulse rounded-full bg-progress motion-reduce:animate-none" />
              <span className="text-[11px] font-medium text-neutral-700">{t(TAB_META[tab].labelKey)}</span>
              <span className="ml-auto text-[10px] text-neutral-400">{t('home.building')}</span>
            </div>
            <code className="mt-1 block pl-3.5 text-[9px] text-neutral-400" translate="no">{tab}</code>
          </div>
        ))}
      </div>

      <div aria-hidden="true" className="mt-5 rounded-xl border border-neutral-200/70 bg-code p-4">
        <div className="space-y-2.5 animate-pulse motion-reduce:animate-none">
          <div className="h-2 w-12 rounded bg-code-dim/50" />
          <div className="ml-3 h-2 w-4/5 rounded bg-code-dim/30" />
          <div className="ml-6 h-2 w-3/5 rounded bg-code-dim/30" />
          <div className="ml-6 h-2 w-2/3 rounded bg-code-dim/30" />
          <div className="ml-3 h-2 w-1/2 rounded bg-code-dim/30" />
          <div className="h-2 w-8 rounded bg-code-dim/50" />
        </div>
      </div>
    </div>
  );
}

function RuntimeDeliveryEndpoint({
  hasPackage,
  draft,
  running,
  collapsed,
  publicationTarget,
}: {
  hasPackage: boolean;
  draft: boolean;
  running: boolean;
  collapsed: boolean;
  publicationTarget: PublicationTargetStatus | null;
}) {
  const { t } = useLocale();
  const publicationReady = publicationTarget?.configured === true && publicationTarget.enabled === true;
  const state = running && !hasPackage
    ? { label: t('home.inProgress'), className: 'bg-sky-50 text-sky-700' }
    : !hasPackage
      ? { label: t('home.deliveryWaiting'), className: 'bg-neutral-100 text-neutral-500' }
      : draft
        ? { label: t('home.deliveryPendingBinding'), className: 'bg-amber-50 text-amber-700' }
        : publicationTarget && !publicationReady
          ? { label: t('home.deliveryNoTarget'), className: 'bg-amber-50 text-amber-700' }
          : { label: t(publicationReady ? 'home.deliveryPublishable' : 'home.deliveryDownloadable'), className: 'bg-emerald-50 text-emerald-700' };

  const endpointLabel = hasPackage && !draft && publicationTarget && !publicationReady
    ? t('home.deliveryHintNoTarget')
    : t('home.deliveryHintDefault');

  return (
    <div
      aria-label={t('home.deliveryAria', { endpoint: endpointLabel, state: state.label })}
      className={`flex h-12 shrink-0 items-center border-t border-line bg-surface-raised ${collapsed ? 'justify-center px-2' : 'gap-2.5 px-3'}`}
    >
      <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-neutral-100 text-neutral-600">
        <Package aria-hidden="true" className="h-3.5 w-3.5" />
      </div>
      {collapsed ? (
        <span className="sr-only">{endpointLabel}</span>
      ) : (
        <>
          <div className="min-w-0 flex-1">
            <p className="text-[9px] font-semibold uppercase tracking-[0.1em] text-neutral-400">{t('home.deliveryEndpoint')}</p>
            <p className="truncate text-[10px] font-medium text-neutral-700">{endpointLabel}</p>
          </div>
          <span className={`shrink-0 rounded-full px-1.5 py-0.5 text-[8px] font-medium ${state.className}`}>
            {state.label}
          </span>
        </>
      )}
    </div>
  );
}

function PreviewSkeleton({ colorScheme }: { colorScheme: ColorScheme }) {
  const { t } = useLocale();
  const dark = colorScheme === 'dark';
  const muted = dark ? 'bg-white/[0.12]' : 'bg-neutral-200';
  const strong = dark ? 'bg-white/20' : 'bg-neutral-300/80';

  return (
    <div
      role="status"
      aria-live="polite"
      data-color-scheme={colorScheme}
      className={cn(
        'min-h-[560px] px-4 py-5',
        dark ? 'bg-device-canvas-dark text-device-ink-dark' : 'bg-[#f6f7f9] text-neutral-500',
      )}
    >
      <span className="sr-only">{t('home.previewGenerating')}</span>
      <div className="animate-pulse motion-reduce:animate-none">
        <div className="flex items-center justify-between">
          <div>
            <div className={cn('h-3 w-20 rounded-full', strong)} />
            <div className={cn('mt-2 h-2 w-32 rounded-full', muted)} />
          </div>
          <div className={cn('h-8 w-8 rounded-full', muted)} />
        </div>

        <div className={cn('mt-5 rounded-2xl p-4 shadow-[0_4px_18px_rgba(15,23,42,0.14)] ring-1', dark ? 'bg-[#242830] ring-white/10' : 'bg-white ring-neutral-200/70')}>
          <div className={cn('h-3 w-2/3 rounded-full', strong)} />
          <div className={cn('mt-2 h-2 w-full rounded-full', muted)} />
          <div className={cn('mt-1.5 h-2 w-4/5 rounded-full', muted)} />
          <div className="mt-4 flex gap-2">
            <div className={cn('h-6 w-16 rounded-full', muted)} />
            <div className={cn('h-6 w-20 rounded-full', muted)} />
          </div>
        </div>

        <div className="mt-3 space-y-3">
          {[0, 1, 2].map((item) => (
            <div key={item} className={cn('flex gap-3 rounded-2xl p-3 ring-1', dark ? 'bg-[#242830] ring-white/10' : 'bg-white ring-neutral-200/70')}>
              <div className={cn('h-16 w-16 shrink-0 rounded-xl', muted)} />
              <div className="min-w-0 flex-1 py-1">
                <div className={cn('h-3 w-4/5 rounded-full', strong)} />
                <div className={cn('mt-2 h-2 w-3/5 rounded-full', muted)} />
                <div className="mt-4 flex items-end justify-between">
                  <div className={cn('h-3 w-14 rounded-full', muted)} />
                  <div className={cn('h-7 w-14 rounded-full', muted)} />
                </div>
              </div>
            </div>
          ))}
        </div>
      </div>
      <div className={cn('mt-5 flex items-center justify-center gap-2 text-[10px] font-medium', dark ? 'text-white/60' : 'text-neutral-500')}>
        <Loader2 aria-hidden="true" className="h-3 w-3 animate-spin motion-reduce:animate-none" />
        {t('home.previewGeneratingRunnable')}
      </div>
    </div>
  );
}

function ImplementationDetail({
  view,
  insight,
  onDownloadDSL,
  onPackageRuntime,
}: {
  view: Exclude<ImplementationView, 'structure'>;
  insight: ImplementationInsight;
  onDownloadDSL: () => void;
  onPackageRuntime: () => void;
}) {
  const { t } = useLocale();
  if (view === 'data') {
    return (
      <div className="h-full overflow-y-auto bg-surface-sunken p-3">
        <div className="rounded-lg border border-line bg-surface px-3 py-2.5">
          <div className="flex items-center gap-2">
            <Database aria-hidden="true" className="h-4 w-4 text-neutral-500" />
            <span className="text-[12px] font-semibold text-neutral-800">{t('home.dataModelTitle')}</span>
          </div>
          <p className="mt-1 text-[10px] leading-4 text-neutral-500">
            {t('home.dataModelHint')}
          </p>
        </div>

        <section className="mt-3 rounded-lg border border-line bg-surface">
          <div className="flex items-center border-b border-line px-3 py-2">
            <span className="text-[11px] font-semibold text-neutral-700">{t('home.dataFields')}</span>
            <span className="ml-auto rounded-full bg-neutral-100 px-2 py-0.5 text-[9px] text-neutral-500">
              {t('home.countItems', { count: insight.dataFields.length })}
            </span>
          </div>
          <div className="divide-y divide-line">
            {insight.dataFields.length > 0 ? insight.dataFields.map((path) => (
              <div key={path} className="px-3 py-2 font-mono text-[10px] text-neutral-600" translate="no">{path}</div>
            )) : (
              <p className="px-3 py-4 text-[10px] text-neutral-400">{t('home.dataFieldsEmpty')}</p>
            )}
          </div>
        </section>

        <section className="mt-3 rounded-lg border border-line bg-surface">
          <div className="flex items-center border-b border-line px-3 py-2">
            <span className="text-[11px] font-semibold text-neutral-700">{t('home.componentRefs')}</span>
            <span className="ml-auto rounded-full bg-neutral-100 px-2 py-0.5 text-[9px] text-neutral-500">
              {t('home.countPlaces', { count: insight.bindings.length })}
            </span>
          </div>
          <div className="divide-y divide-line">
            {insight.bindings.length > 0 ? insight.bindings.map((binding) => (
              <div key={`${binding.componentId}:${binding.property}:${binding.path}`} className="px-3 py-2">
                <div className="flex min-w-0 items-center gap-2 text-[10px]">
                  <code className="truncate text-neutral-700" translate="no">{binding.componentId}.{binding.property}</code>
                  <span className="text-neutral-300">→</span>
                  <code className="truncate text-info" translate="no">{binding.path}</code>
                </div>
              </div>
            )) : (
              <p className="px-3 py-4 text-[10px] text-neutral-400">{t('home.componentRefsEmpty')}</p>
            )}
          </div>
        </section>
      </div>
    );
  }

  if (view === 'actions') {
    return (
      <div className="h-full overflow-y-auto bg-surface-sunken p-3">
        <div className="rounded-lg border border-line bg-surface px-3 py-2.5">
          <div className="flex items-center gap-2">
            <MousePointerClick aria-hidden="true" className="h-4 w-4 text-neutral-500" />
            <span className="text-[12px] font-semibold text-neutral-800">{t('home.viewActions')}</span>
            <span className="ml-auto rounded-full bg-neutral-100 px-2 py-0.5 text-[9px] text-neutral-500">
              {t('home.countItems', { count: insight.actions.length })}
            </span>
          </div>
          <p className="mt-1 text-[10px] leading-4 text-neutral-500">{t('home.actionsHint')}</p>
        </div>
        <div className="mt-3 space-y-2">
          {insight.actions.length > 0 ? insight.actions.map((action, index) => (
            <div key={`${action.componentId}:${action.name}:${index}`} className="rounded-lg border border-line bg-surface px-3 py-2.5">
              <div className="flex items-center gap-2">
                <span className="h-1.5 w-1.5 rounded-full bg-emerald-500" />
                <code className="truncate text-[11px] font-medium text-neutral-800" translate="no">{action.name}</code>
              </div>
              <div className="mt-1 pl-3.5 text-[9px] text-neutral-400">
                {action.componentType} · <code translate="no">{action.componentId}</code>
              </div>
            </div>
          )) : (
            <div className="rounded-lg border border-dashed border-line px-4 py-8 text-center">
              <p className="text-[11px] font-medium text-neutral-600">{t('home.actionsEmpty')}</p>
              <p className="mt-1 text-[10px] text-neutral-400">{t('home.actionsEmptyHint')}</p>
            </div>
          )}
        </div>
      </div>
    );
  }

  if (view === 'binding') {
    return (
      <div className="h-full overflow-y-auto bg-surface-sunken p-3">
        <div className="rounded-lg border border-line bg-surface px-3 py-3">
          <div className="flex items-center gap-2">
            <Workflow aria-hidden="true" className={`h-4 w-4 ${insight.bindingComplete ? 'text-emerald-600' : 'text-amber-600'}`} />
            <span className="text-[12px] font-semibold text-neutral-800">
              {t(insight.bindingComplete ? 'home.bindingDone' : 'home.bindingWaiting')}
            </span>
          </div>
          <p className="mt-1 text-[10px] leading-4 text-neutral-600">
            {t('home.bindingHint')}
          </p>
        </div>

        <section className="mt-3 rounded-lg border border-line bg-surface">
          <div className="flex items-center border-b border-line px-3 py-2">
            <span className="text-[11px] font-semibold text-neutral-700">{t('nav.datasources')}</span>
            <span className="ml-auto text-[9px] text-neutral-400">{t('home.countGeneric', { count: insight.dataSources.length })}</span>
          </div>
          {insight.dataSources.length > 0 ? (
            <div className="divide-y divide-line">
              {insight.dataSources.map((source) => (
                <div key={`${source.id}:${source.name}`} className="px-3 py-2.5">
                  <div className="truncate text-[10px] font-medium text-neutral-700">{source.name}</div>
                  <div className="mt-0.5 truncate font-mono text-[9px] text-neutral-400" translate="no">
                    {source.path || source.id}
                  </div>
                </div>
              ))}
            </div>
          ) : <p className="px-3 py-4 text-[10px] text-neutral-400">{t('home.dataSourcesEmpty')}</p>}
        </section>

        <section className="mt-3 rounded-lg border border-line bg-surface">
          <div className="flex items-center border-b border-line px-3 py-2">
            <span className="text-[11px] font-semibold text-neutral-700">{t('home.fieldMappings')}</span>
            <span className="ml-auto text-[9px] text-neutral-400">{t('home.countMappings', { count: insight.runtimeBindings.length })}</span>
          </div>
          {insight.runtimeBindings.length > 0 ? (
            <div className="divide-y divide-line">
              {insight.runtimeBindings.map((binding, index) => (
                <div key={`${binding.sourceId}:${binding.refKey}:${index}`} className="px-3 py-2.5">
                  <div className="flex min-w-0 items-center gap-1.5 font-mono text-[9px]" translate="no">
                    <span className="truncate text-info">{binding.sourceKey || t('home.bindingUnbound')}</span>
                    <span className="shrink-0 text-neutral-300">→</span>
                    <span className="truncate text-neutral-700">{binding.refKey || t('home.bindingUnknownField')}</span>
                  </div>
                  {(binding.operatorRef || binding.transforms.length > 0) && (
                    <div className="mt-1 truncate text-[9px] text-neutral-400">
                      {t('home.operatorPrefix', { list: [binding.operatorRef, ...binding.transforms].filter(Boolean).join(' · ') })}
                    </div>
                  )}
                </div>
              ))}
            </div>
          ) : <p className="px-3 py-4 text-[10px] text-neutral-400">{t('home.fieldMappingsEmpty')}</p>}
        </section>

        <section className="mt-3 rounded-lg border border-line bg-surface px-3 py-2.5">
          <div className="flex items-center justify-between">
            <span className="text-[11px] font-semibold text-neutral-700">{t('nav.operators')}</span>
            <span className="text-[9px] text-neutral-400">{t('home.countGeneric', { count: insight.operators.length })}</span>
          </div>
          <div className="mt-2 flex flex-wrap gap-1.5">
            {insight.operators.length > 0 ? insight.operators.map((operator) => (
              <span key={operator.id} className="rounded-md bg-neutral-100 px-2 py-1 font-mono text-[9px] text-neutral-600" translate="no">
                {operator.label}
              </span>
            )) : <span className="text-[10px] text-neutral-400">{t('home.operatorsEmpty')}</span>}
          </div>
        </section>
      </div>
    );
  }

  const stages = [
    { label: 'DSL', detail: t('home.countComponents', { count: insight.componentCount }), ready: insight.previewReady },
    { label: t('nav.datasources'), detail: t('home.countGeneric', { count: insight.dataSources.length }), ready: insight.dataSources.length > 0 },
    { label: t('nav.operators'), detail: insight.operators.length > 0 ? t('home.countGeneric', { count: insight.operators.length }) : t('home.noOperatorNeeded'), ready: true },
    { label: t('home.viewBinding'), detail: t('home.countMappingRows', { count: insight.runtimeBindings.length }), ready: insight.bindingComplete },
  ];
  return (
    <div className="h-full overflow-y-auto bg-surface-sunken p-3">
      <div className="rounded-lg border border-line bg-surface px-3 py-3">
        <div className="flex items-center gap-2">
          <Package aria-hidden="true" className={`h-4 w-4 ${insight.bundleReady ? 'text-emerald-600' : 'text-amber-600'}`} />
          <span className="text-[12px] font-semibold text-neutral-800">
            {t(insight.bundleReady ? 'home.runtimeReady' : 'home.runtimeNotReady')}
          </span>
        </div>
        <p className="mt-1 text-[10px] leading-4 text-neutral-600">
          {t('home.runtimeHint')}
        </p>
      </div>

      <div className="mt-3 rounded-lg border border-line bg-surface p-3">
        <p className="text-[10px] font-semibold uppercase tracking-[0.12em] text-neutral-400">{t('home.runtimeContents')}</p>
        <div className="mt-3 space-y-1.5">
          {stages.map((stage, index) => (
            <div key={stage.label}>
              <div className="flex items-center gap-2 rounded-md bg-surface-sunken px-2.5 py-2">
                <span className={`flex h-4 w-4 items-center justify-center rounded-full text-[9px] ${
                  stage.ready ? 'bg-emerald-100 text-emerald-700' : 'bg-neutral-200 text-neutral-500'
                }`}>{stage.ready ? '✓' : index + 1}</span>
                <span className="text-[10px] font-medium text-neutral-700">{stage.label}</span>
                <span className="ml-auto truncate text-[9px] text-neutral-400">{stage.detail}</span>
              </div>
              {index < stages.length - 1 && <div className="ml-[17px] h-2 border-l border-line" />}
            </div>
          ))}
        </div>
      </div>

      <div className="mt-3 rounded-lg border border-line bg-surface px-3 py-2.5">
        <div className="flex items-center gap-2">
          <Cpu aria-hidden="true" className="h-4 w-4 text-neutral-500" />
          <span className="text-[11px] font-semibold text-neutral-700">{t('home.callParams')}</span>
          <code className="ml-auto rounded bg-neutral-100 px-1.5 py-0.5 text-[9px] text-neutral-500" translate="no">application/json</code>
        </div>
        <p className="mt-1.5 text-[10px] leading-4 text-neutral-500">{t('home.callParamsHint')}</p>
      </div>

      <div className="mt-3 grid grid-cols-2 gap-2">
        <button
          type="button"
          onClick={onDownloadDSL}
          className="inline-flex h-9 items-center justify-center gap-1.5 rounded-lg border border-line bg-surface text-[10px] font-medium text-neutral-700 transition-colors hover:bg-neutral-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
        >
          <Download aria-hidden="true" className="h-3.5 w-3.5" />
          {t('home.downloadDsl')}
        </button>
        <button
          type="button"
          onClick={onPackageRuntime}
          disabled={!insight.bundleReady}
          title={t(insight.bundleReady ? 'home.packageRuntimeReady' : 'home.packageRuntimeBlocked')}
          className="inline-flex h-9 items-center justify-center gap-1.5 rounded-lg bg-inverse text-[10px] font-medium text-inverse-fg transition-colors hover:bg-inverse-hover disabled:cursor-not-allowed disabled:opacity-35 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
        >
          <Package aria-hidden="true" className="h-3.5 w-3.5" />
          {t('home.packageRuntime')}
        </button>
      </div>
      <p className="mt-3 px-1 text-[9px] leading-4 text-neutral-400">
        {t('home.runtimeFootnote')}
      </p>
    </div>
  );
}

function PanelResizeHandle({
  divider,
  position,
  onPointerDown,
  onKeyDown,
  onReset,
  hidden = false,
}: {
  divider: PanelDivider;
  position: number;
  onPointerDown: (divider: PanelDivider, event: ReactPointerEvent<HTMLDivElement>) => void;
  onKeyDown: (divider: PanelDivider, event: ReactKeyboardEvent<HTMLDivElement>) => void;
  onReset: () => void;
  hidden?: boolean;
}) {
  const { t } = useLocale();
  return (
    <div
      role="separator"
      aria-label={divider === 'chat-preview' ? t('home.resizeChatPreview') : t('home.resizePreviewProtocol')}
      aria-orientation="vertical"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(position)}
      tabIndex={hidden ? -1 : 0}
      onPointerDown={(event) => !hidden && onPointerDown(divider, event)}
      onKeyDown={(event) => !hidden && onKeyDown(divider, event)}
      onDoubleClick={() => !hidden && onReset()}
      title={t('home.resizeHint')}
      className={`group relative z-10 h-full touch-none bg-line outline-none ${
        hidden
          ? 'pointer-events-none opacity-0'
          : 'cursor-col-resize transition-colors hover:bg-progress focus-visible:bg-progress'
      }`}
    >
      {!hidden && (
        <span className="absolute left-1/2 top-1/2 h-12 w-1 -translate-x-1/2 -translate-y-1/2 rounded-full bg-neutral-400 transition-colors group-hover:bg-surface group-focus-visible:bg-surface" />
      )}
    </div>
  );
}

function controlQuestionsFromFrame(frame: CanonicalFrame): ControlQuestionView[] {
  const source = Array.isArray(frame.payload?.questions) ? frame.payload.questions : [];
  const questions = source.flatMap((value, questionIndex) => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return [];
    const question = value as Record<string, unknown>;
    const body = String(question.question ?? '').trim();
    if (!body) return [];
    const rawOptions = Array.isArray(question.options) ? question.options : [];
    const options = rawOptions.flatMap((rawOption, optionIndex) => {
      if (!rawOption || typeof rawOption !== 'object' || Array.isArray(rawOption)) return [];
      const option = rawOption as Record<string, unknown>;
      const label = String(option.label ?? '');
      return [{
        id: String(option.id ?? `q${questionIndex}_opt${optionIndex}`),
        label,
        description: String(option.description ?? '').trim() || undefined,
        type: option.type === 'input' || !label ? 'input' as const : 'select' as const,
        placeholder: String(option.placeholder ?? '').trim() || undefined,
      }];
    });
    return [{
      id: String(question.id ?? `q${questionIndex}`),
      header: String(question.header ?? '').trim() || undefined,
      question: body,
      options,
    }];
  });
  if (questions.length > 0) return questions;
  const fallbackQuestion = String(frame.payload?.question ?? '').trim();
  if (!fallbackQuestion) return [];
  const rawOptions = Array.isArray(frame.payload?.options) ? frame.payload.options : [];
  return [{
    id: String(frame.payload?.question_id ?? 'q0'),
    question: fallbackQuestion,
    options: rawOptions.flatMap((rawOption, optionIndex) => {
      if (!rawOption || typeof rawOption !== 'object' || Array.isArray(rawOption)) return [];
      const option = rawOption as Record<string, unknown>;
      const label = String(option.label ?? '');
      return [{
        id: String(option.id ?? `q0_opt${optionIndex}`),
        label,
        type: !label ? 'input' as const : 'select' as const,
      }];
    }),
  }];
}

// applyFrameToLine is the single canonical-frame → view-model reducer, shared
// by the live stream and the refresh replay (principle 3: mapping happens
// only after consuming canonical frames).
function applyFrameToLine(frame: CanonicalFrame, line: ChatLine): void {
  const chips = line.chips ?? (line.chips = []);
  const artifacts = line.artifacts ?? (line.artifacts = []);
  const step = String(frame.payload?.step ?? '');
  const tool = String(frame.payload?.tool ?? '');
  const processID = String(frame.payload?.process_id ?? '');
  const processKey = processID
    ? `${frame.run_id ?? ''}\u0000${processID}`
    : `${frame.run_id ?? ''}\u0000${step || tool}`;
  const detail = String(frame.payload?.detail ?? '');
  const warning = String(frame.payload?.projection_warning ?? '');
  switch (frame.event_type) {
    case 'agent_commentary':
      if (!line.commentary) line.commentary = String(frame.payload?.text ?? '').trim() || undefined;
      break;
    case 'harness_process': {
      const process = frame.payload ?? {};
      if (isNextStepsProcess(process)) {
        const plan = nextStepsFromProcess(process);
        if (plan) line.nextSteps = plan;
        break;
      }
      line.progress = applyHarnessProcess(line.progress, process);
      break;
    }
    case 'progress_init':
    case 'progress_activity':
      line.progress = applyProgressFrame(line.progress, frame.payload ?? {});
      break;
    case 'runtime_step_started':
      if (step) {
        const chip = chips.find((candidate) => candidate.id === processKey);
        if (chip) {
          Object.assign(chip, { label: step, detail, warning, state: 'run' as const });
        } else {
          chips.push({ id: processKey, label: step, detail, warning, state: 'run' });
        }
      }
      break;
    case 'runtime_step_completed': {
      const chip = chips.find((candidate) => candidate.id === processKey);
      if (chip) {
        Object.assign(chip, { label: step || chip.label, detail: detail || chip.detail, warning, state: 'done' as const });
      } else if (step) {
        chips.push({ id: processKey, label: step, detail, warning, state: 'done' });
      }
      break;
    }
    case 'tool_call_started':
      if (tool === nextStepsToolName) break;
      if (tool && !chips.some((chip) => chip.id === processKey)) {
        chips.push({ id: processKey, label: tool, state: 'run' });
      }
      break;
    case 'tool_call_completed': {
      if (tool === nextStepsToolName) break;
      const chip = chips.find((candidate) => candidate.id === processKey);
      if (chip) {
        chip.state = 'done';
      } else if (tool) {
        chips.push({ id: processKey, label: tool, state: 'done' });
      }
      break;
    }
    case 'runtime_step_failed': {
      const chip = chips.find((candidate) => candidate.id === processKey);
      if (chip) {
        Object.assign(chip, { label: step || chip.label, detail: detail || chip.detail, warning, state: 'err' as const });
      } else if (step) {
        chips.push({ id: processKey, label: step, detail, warning, state: 'err' });
      }
      const explicitError = String(frame.payload?.error ?? '');
      if (explicitError) line.error = explicitError;
      break;
    }
    case 'tool_call_failed': {
      if (tool === nextStepsToolName) break;
      const chip = chips.find((candidate) => candidate.id === processKey);
      if (chip) {
        chip.state = 'err';
      } else if (tool) {
        chips.push({ id: processKey, label: tool, state: 'err' });
      }
      break;
    }
    case 'artifact_created':
      {
        const artifact = `${frame.payload?.kind} #${(frame.artifact_ref ?? '').slice(0, 8)}`;
        if (!artifacts.includes(artifact)) artifacts.push(artifact);
      }
      break;
    case 'agent_text_delta':
      line.turnCompleted = false;
      line.text = `${line.text ?? ''}${frame.payload?.text ?? ''}`;
      break;
    case 'control_request_created':
      line.turnCompleted = false;
      line.progress = finalizeProgressTimeline(line.progress, 'interrupted');
      {
        const controlID = String(frame.payload?.control_id ?? '');
        const alreadyAnswered = line.control?.id === controlID && line.controlAnswered === true;
        line.control = {
          id: controlID,
          runId: String(frame.run_id ?? ''),
          questions: controlQuestionsFromFrame(frame),
        };
        // Resume streams may replay the original request after the response was
        // accepted. A durable answered state is monotonic for the same control.
        line.controlAnswered = alreadyAnswered || frame.payload?.answered === true;
      }
      break;
    case 'design_confirmation':
      line.progress = finalizeProgressTimeline(line.progress, 'interrupted');
      line.confirmation = {
        question: String(frame.payload?.question ?? frame.payload?.preamble ?? ''),
        options: (frame.payload?.options as {
          id: string; label: string; type: string; placeholder?: string;
        }[] | undefined) ?? [],
      };
      break;
    case 'control_response_received':
      line.turnCompleted = false;
      line.control = undefined;
      break;
    case 'run_resume_failed':
      line.turnCompleted = false;
      line.error = String(frame.payload?.error ?? '') || 'RESUME_FAILED';
      break;
    case 'run_failed':
      line.turnCompleted = false;
      line.nextSteps = undefined;
      line.progress = finalizeProgressTimeline(line.progress, 'failed');
      for (const chip of chips) {
        if (chip.state === 'run') chip.state = 'err';
      }
      line.error = String(frame.payload?.error ?? frame.error?.message ?? '') || 'run failed';
      break;
    case 'run_completed':
      line.turnCompleted = true;
      line.progress = finalizeProgressTimeline(line.progress, 'completed');
      for (const chip of chips) {
        if (chip.state === 'run') chip.state = 'done';
      }
      break;
    default:
      break;
  }
}

function attachmentsFromFrame(frame: CanonicalFrame): ChatAttachmentView[] {
  const raw = frame.payload?.attachments;
  if (!Array.isArray(raw)) return [];
  return raw.flatMap((value) => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return [];
    const attachment = value as Record<string, unknown>;
    const name = String(attachment.name ?? '').trim();
    const artifactRef = String(attachment.artifactRef ?? '').trim();
    if (!name && !artifactRef) return [];
    return [{
      name: name || 'attachment',
      artifactRef: artifactRef || undefined,
      mimeType: String(attachment.mimeType ?? '') || undefined,
      type: String(attachment.type ?? '') || undefined,
    }];
  });
}

function resolvePreviewColorScheme(
  protocol: Record<string, unknown>[],
  fallback: ColorScheme,
): ColorScheme {
  for (let index = protocol.length - 1; index >= 0; index -= 1) {
    const message = protocol[index];
    const createSurface = message?.createSurface;
    if (!createSurface || typeof createSurface !== 'object' || Array.isArray(createSurface)) continue;
    const theme = (createSurface as Record<string, unknown>).theme;
    if (!theme || typeof theme !== 'object' || Array.isArray(theme)) continue;
    const value = (theme as Record<string, unknown>).colorScheme;
    if (value === 'light' || value === 'dark') return value;
  }
  return fallback;
}

function ComposerInner() {
  const params = useSearchParams();
  const { theme } = useTheme();
  const { locale, t } = useLocale();
  const requestedSessionID = params.get('session') ?? 'demo-1';
  const requestedImplementationView = params.get('view');
  const initialImplementationView = isImplementationView(requestedImplementationView)
    ? requestedImplementationView
    : 'structure';
  const [sessionId, setSessionId] = useState(requestedSessionID);
  const [lines, setLines] = useState<ChatLine[]>([]);
  const [input, setInput] = useState('');
  const [attachments, setAttachments] = useState<File[]>([]);
  const [running, setRunning] = useState(false);
  const [publishing, setPublishing] = useState(false);
  const [publicationTarget, setPublicationTarget] = useState<PublicationTargetStatus | null>(null);
  const [publicationGuideOpen, setPublicationGuideOpen] = useState(false);
  const [hasPackage, setHasPackage] = useState(false);
  const [draftPreview, setDraftPreview] = useState(false);
  const [pkg, setPkg] = useState<WorkbenchPackage | null>(null);
  const [structureSource, setStructureSource] = useState('');
  const [status, setStatus] = useState('');
  const [statusLink, setStatusLink] = useState<{ href: string; label: string } | null>(null);
  const [modelConfigured, setModelConfigured] = useState<boolean | null>(null);
  const [protocolCollapsed, setProtocolCollapsed] = useState(false);
  const [implementationView, setImplementationView] = useState<ImplementationView>(initialImplementationView);
  const [lastDSLView, setLastDSLView] = useState<DSLImplementationView>(
    isDSLImplementationView(initialImplementationView) ? initialImplementationView : 'structure',
  );
  const [panelSizes, setPanelSizes] = useState<PanelSizes>(DEFAULT_PANEL_SIZES);
  const [panelSizesReady, setPanelSizesReady] = useState(false);
  const [previewDeviceId, setPreviewDeviceId] = useState<PreviewDeviceID>('iphone-15-pro');
  const [previewDeviceReady, setPreviewDeviceReady] = useState(false);
  const [recoveryTarget, setRecoveryTarget] = useState<SessionRecoveryTarget | null>(null);
  const chatRef = useRef<HTMLDivElement>(null);
  const workspaceRef = useRef<HTMLDivElement>(null);
  const assistantRef = useRef<ChatLine | null>(null);
  const booted = useRef('');
  const routedSessionRef = useRef(requestedSessionID);
  const internalSessionRef = useRef('');
  const attachmentInputRef = useRef<HTMLInputElement>(null);
  const composerInputRef = useRef<HTMLTextAreaElement>(null);
  const attachmentPreviewURLsRef = useRef<Set<string>>(new Set());
  const answeredControlIDsRef = useRef<Set<string>>(new Set());
  const streamCursorRef = useRef<RunCursorMap>({});

  useEffect(() => () => {
    attachmentPreviewURLsRef.current.forEach((previewURL) => URL.revokeObjectURL(previewURL));
    attachmentPreviewURLsRef.current.clear();
  }, []);

  useEffect(() => {
    try {
      const stored = window.localStorage.getItem(PANEL_SIZE_STORAGE_KEY);
      if (stored) {
        const parsed = JSON.parse(stored) as Partial<PanelSizes>;
        if (
          Number.isFinite(parsed.chat) &&
          Number.isFinite(parsed.preview) &&
          Number.isFinite(parsed.protocol) &&
          Math.abs((parsed.chat ?? 0) + (parsed.preview ?? 0) + (parsed.protocol ?? 0) - 100) < 0.1
        ) {
          setPanelSizes({
            chat: parsed.chat as number,
            preview: parsed.preview as number,
            protocol: parsed.protocol as number,
          });
        }
      }
    } catch {
      // Ignore invalid local preferences and keep the balanced default layout.
    } finally {
      setPanelSizesReady(true);
    }
  }, []);

  useEffect(() => {
    if (!panelSizesReady) return;
    try {
      window.localStorage.setItem(PANEL_SIZE_STORAGE_KEY, JSON.stringify(panelSizes));
    } catch {
      // The workbench remains usable when browser storage is unavailable.
    }
  }, [panelSizes, panelSizesReady]);

  useEffect(() => {
    try {
      const stored = window.localStorage.getItem(PREVIEW_DEVICE_STORAGE_KEY) as PreviewDeviceID | null;
      if (stored && PREVIEW_DEVICES.some((device) => device.id === stored)) {
        setPreviewDeviceId(stored);
      }
    } catch {
      // Device selection is optional; fall back to the default viewport.
    } finally {
      setPreviewDeviceReady(true);
    }
  }, []);

  useEffect(() => {
    if (!previewDeviceReady) return;
    try {
      window.localStorage.setItem(PREVIEW_DEVICE_STORAGE_KEY, previewDeviceId);
    } catch {
      // Preview remains functional when browser storage is unavailable.
    }
  }, [previewDeviceId, previewDeviceReady]);

  const previewDevice = PREVIEW_DEVICES.find((device) => device.id === previewDeviceId) ?? PREVIEW_DEVICES[0];
  const previewColorScheme = useMemo(
    () => resolvePreviewColorScheme(Array.isArray(pkg?.protocol) ? pkg.protocol : [], theme),
    [pkg, theme],
  );
  const implementation = useMemo(
    () => analyzeImplementation(Array.isArray(pkg?.protocol) ? pkg.protocol : [], {
      bindings: pkg?.bindings,
      apis: pkg?.apis,
      generationID: pkg?.generationID,
      revision: pkg?.revision,
    }),
    [pkg],
  );
  const implementationSection = implementationSectionForView(implementationView);

  const downloadDSL = useCallback(() => {
    if (!pkg?.protocol) return;
    downloadJSON(`${downloadStem(sessionId)}.agenui.dsl.json`, pkg.protocol);
  }, [pkg, sessionId]);

  const packageRuntime = useCallback(() => {
    if (!pkg?.protocol) return;
    if (sessionId !== 'new' && sessionId !== 'demo-1') {
      const anchor = document.createElement('a');
      anchor.href = `/api/v1/agenui/agent/sessions/${encodeURIComponent(sessionId)}/package`;
      anchor.download = '';
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      return;
    }
    try {
      const runtimePackage = buildRuntimePackage(pkg.protocol, {
        bindings: pkg.bindings,
        apis: pkg.apis,
        generationID: pkg.generationID,
        revision: pkg.revision,
      });
      downloadJSON(`${downloadStem(sessionId)}.agenui.runtime.json`, runtimePackage);
    } catch (error) {
      setStatus(error instanceof Error ? error.message : t('home.packageFailed'));
    }
  }, [pkg, sessionId]);

  const selectImplementationView = useCallback((view: ImplementationView) => {
    setImplementationView(view);
    if (isDSLImplementationView(view)) setLastDSLView(view);
    const url = new URL(window.location.href);
    if (view === 'structure') url.searchParams.delete('view');
    else url.searchParams.set('view', view);
    window.history.replaceState(window.history.state, '', `${url.pathname}${url.search}${url.hash}`);
  }, []);

  const openImplementation = useCallback((view: ImplementationView) => {
    setProtocolCollapsed(false);
    selectImplementationView(view);
  }, [selectImplementationView]);

  const selectImplementationSection = useCallback((section: ImplementationSection) => {
    if (section === 'dsl') openImplementation(lastDSLView);
    else if (section === 'binding') openImplementation('binding');
    else openImplementation('runtime');
  }, [lastDSLView, openImplementation]);

  const moveImplementationSection = useCallback((
    event: ReactKeyboardEvent<HTMLButtonElement>,
    current: ImplementationSection,
  ) => {
    const next = adjacentTab(IMPLEMENTATION_SECTIONS, current, event.key);
    if (!next) return;
    event.preventDefault();
    selectImplementationSection(next);
    requestAnimationFrame(() => document.getElementById(`implementation-section-${next}`)?.focus());
  }, [selectImplementationSection]);

  const moveDSLView = useCallback((
    event: ReactKeyboardEvent<HTMLButtonElement>,
    current: DSLImplementationView,
  ) => {
    const next = adjacentTab(DSL_IMPLEMENTATION_VIEWS, current, event.key);
    if (!next) return;
    event.preventDefault();
    openImplementation(next);
    requestAnimationFrame(() => document.getElementById(`implementation-view-${next}`)?.focus());
  }, [openImplementation]);

  useEffect(() => {
    const view = isImplementationView(requestedImplementationView) ? requestedImplementationView : 'structure';
    setImplementationView(view);
    if (isDSLImplementationView(view)) setLastDSLView(view);
  }, [requestedImplementationView]);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const response = await fetch('/api/v1/agenui/admin/local/model', { cache: 'no-store' });
        if (!response.ok) {
          return;
        }
        const body = await response.json() as { configured?: boolean };
        if (!cancelled && typeof body.configured === 'boolean') {
          setModelConfigured(body.configured);
        }
      } catch {
        // Keep the reminder hidden when configuration status cannot be verified.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const loadPublicationTarget = useCallback(async (): Promise<PublicationTargetStatus | null> => {
    try {
      const response = await fetch('/api/v1/agenui/admin/local/publication-target', { cache: 'no-store' });
      const body = await response.json().catch(() => ({})) as Partial<PublicationTargetStatus>;
      if (!response.ok) return null;
      const target: PublicationTargetStatus = {
        configured: body.configured === true,
        enabled: body.enabled === true,
        kind: body.kind === 'mq' ? 'mq' : body.kind === 'callback' ? 'callback' : undefined,
        endpoint: typeof body.endpoint === 'string' ? body.endpoint : undefined,
      };
      setPublicationTarget(target);
      return target;
    } catch {
      return null;
    }
  }, []);

  useEffect(() => {
    void loadPublicationTarget();
  }, [loadPublicationTarget]);

  // Only react to a route-selected session. A streamed first turn also changes
  // sessionId after the server allocates its durable ID; treating that internal
  // change as sidebar navigation clears the live user/assistant lines while the
  // request is still running and leaves the composer looking frozen.
  useEffect(() => {
    if (routedSessionRef.current === requestedSessionID) {
      return;
    }
    routedSessionRef.current = requestedSessionID;
    if (internalSessionRef.current === requestedSessionID) {
      return;
    }
    internalSessionRef.current = '';
    setSessionId(requestedSessionID);
    setHasPackage(false);
    setDraftPreview(false);
    setPkg(null);
    setLines([]);
    setRunning(false);
    setRecoveryTarget(null);
    setStructureSource('');
    selectImplementationView('structure');
    assistantRef.current = null;
    streamCursorRef.current = {};
  }, [requestedSessionID, selectImplementationView]);

  useEffect(() => {
    chatRef.current?.scrollTo({ top: chatRef.current.scrollHeight });
  }, [lines]);

  const applyProtocolMessages = useCallback((
    protocol: Record<string, unknown>[],
    artifacts: ImplementationArtifacts = {},
    draft = false,
  ) => {
    if (protocol.length === 0) {
      return false;
    }
    setPkg({ protocol, ...artifacts });
    setHasPackage(true);
    setDraftPreview(draft);
    setStructureSource(JSON.stringify(protocol, null, 2));
    return true;
  }, []);

  useEffect(() => {
    if (hasPackage) {
      setProtocolCollapsed(false);
    }
  }, [hasPackage]);

  const resetPanelSizes = useCallback(() => {
    setPanelSizes(DEFAULT_PANEL_SIZES);
  }, []);

  const resizePanelsByKeyboard = useCallback((
    divider: PanelDivider,
    event: ReactKeyboardEvent<HTMLDivElement>,
  ) => {
    if (event.key === 'Home') {
      event.preventDefault();
      resetPanelSizes();
      return;
    }
    if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') {
      return;
    }
    event.preventDefault();
    const step = event.shiftKey ? 5 : 2;
    setPanelSizes((current) => resizePanelSizes(
      current,
      divider,
      event.key === 'ArrowRight' ? step : -step,
    ));
  }, [resetPanelSizes]);

  const beginPanelResize = useCallback((
    divider: PanelDivider,
    event: ReactPointerEvent<HTMLDivElement>,
  ) => {
    const workspace = workspaceRef.current;
    if (!workspace) return;
    event.preventDefault();
    event.currentTarget.setPointerCapture?.(event.pointerId);
    const startX = event.clientX;
    const startSizes = panelSizes;
    const workspaceWidth = workspace.getBoundingClientRect().width;
    const previousUserSelect = document.body.style.userSelect;
    const previousCursor = document.body.style.cursor;
    document.body.style.userSelect = 'none';
    document.body.style.cursor = 'col-resize';

    const move = (moveEvent: PointerEvent) => {
      const delta = ((moveEvent.clientX - startX) / workspaceWidth) * 100;
      setPanelSizes(resizePanelSizes(startSizes, divider, delta));
    };
    const finish = () => {
      document.body.style.userSelect = previousUserSelect;
      document.body.style.cursor = previousCursor;
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', finish);
      window.removeEventListener('pointercancel', finish);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', finish);
    window.addEventListener('pointercancel', finish);
  }, [panelSizes]);

  const loadPackage = useCallback(async (sid: string) => {
    try {
      const presentation = await sessionPresentation(sid);
      if (!presentation?.result) {
        return;
      }
      applyProtocolMessages(decodeAGenUIResult(presentation.result), {
        bindings: presentation.bindings,
        apis: presentation.apis,
        generationID: presentation.generationID,
        revision: presentation.revision,
      }, presentation.draft === true || presentation.executable === false);
    } catch {
      /* not generated yet: preserve the last successful preview */
    }
  }, [applyProtocolMessages]);

  const previewMessages = Array.isArray(pkg?.protocol)
    ? (pkg.protocol as Record<string, unknown>[])
    : [];

  const beginAssistant = useCallback(() => {
    const line: ChatLine = { id: lineSeq++, role: 'assistant', chips: [], artifacts: [] };
    assistantRef.current = line;
    setLines((prev) => [...prev, line]);
    return line;
  }, []);

  const patchAssistant = useCallback((patch: Partial<ChatLine>) => {
    const current = assistantRef.current;
    if (!current) {
      return;
    }
    Object.assign(current, patch);
    setLines((prev) => prev.map((l) => (l.id === current.id ? { ...current } : l)));
  }, []);

  const onFrame = useCallback(
    (frame: CanonicalFrame) => {
      const current = assistantRef.current;
      if (!current) {
        return;
      }
      const controlIDs = [frame.payload?.control_id, frame.payload?.request_id]
        .map((value) => String(value ?? ''))
        .filter(Boolean);
      if (
        frame.event_type === 'control_request_created' &&
        controlIDs.some((controlID) => answeredControlIDsRef.current.has(controlID))
      ) {
        // Native resume replays the interrupt event before continuing. It is
        // history, not a new question, so do not create a second prompt card.
        return;
      }
      applyFrameToLine(frame, current);
      patchAssistant({
        chips: [...(current.chips ?? [])],
        artifacts: [...(current.artifacts ?? [])],
        nextSteps: current.nextSteps,
      });
    },
    [patchAssistant],
  );

  const recoverProgressFromSession = useCallback(async (sid: string, target: ChatLine) => {
    const frames = await sessionProgress(sid);
    let changed = false;
    for (const frame of frames) {
      const previous = target.progress;
      const previousChips = JSON.stringify(target.chips ?? []);
      const previousPlanID = target.nextSteps?.planId;
      applyFrameToLine(frame, target);
      changed = changed
        || target.progress !== previous
        || JSON.stringify(target.chips ?? []) !== previousChips
        || target.nextSteps?.planId !== previousPlanID;
    }
    if (changed) {
      setLines((previous) => previous.map((line) => (
        line.id === target.id ? { ...target } : line
      )));
    }
  }, []);

  useEffect(() => {
    if (!recoveryTarget) {
      return;
    }
    const controller = new AbortController();
    const pendingAnsweredControlIDs = new Set(recoveryTarget.answeredControlIDs);
    const cursor: RunCursorMap = {};

    const recoveredFrameHandler = () => {
      let reconcileText = createAssistantTextReplayGuard(
        assistantRef.current?.text ?? recoveryTarget.existingText,
      );
      return (frame: CanonicalFrame) => {
        if (assistantRef.current?.id !== recoveryTarget.assistantLineId) {
          return;
        }
        const controlIDs = [frame.payload?.control_id, frame.payload?.request_id]
          .map((value) => String(value ?? ''))
          .filter(Boolean);
        const answeredControlID = controlIDs.find((controlID) => pendingAnsweredControlIDs.has(controlID));
        if (frame.event_type === 'control_request_created' && answeredControlID) {
          pendingAnsweredControlIDs.delete(answeredControlID);
          if (pendingAnsweredControlIDs.size === 0) {
            reconcileText = createAssistantTextReplayGuard(assistantRef.current?.text ?? '');
          }
          return;
        }
        if (pendingAnsweredControlIDs.size > 0) {
          return;
        }
        if (frame.event_type === 'agent_text_delta') {
          const decision = reconcileText(String(frame.payload?.text ?? ''));
          if (decision.kind === 'skip') return;
          if (decision.kind === 'replace') {
            patchAssistant({ text: decision.text });
            return;
          }
          onFrame({ ...frame, payload: { ...frame.payload, text: decision.text } });
          return;
        }
        onFrame(frame);
      };
    };

    setRunning(true);
    setStatus('');
    void (async () => {
      let consecutiveFailures = 0;
      try {
        while (!controller.signal.aborted) {
          try {
            const onRecoveredFrame = recoveredFrameHandler();
            await resumeSessionRun(recoveryTarget.sessionId, recoveryTarget.runId, {
              onFrame: onRecoveredFrame,
              onCursor: (sequences) => Object.assign(cursor, sequences),
              onFinal: (summary: Record<string, unknown>) => {
                applyProtocolMessages(decodeAGenUIResult(summary.result));
              },
            }, { signal: controller.signal, cursor });
            if (controller.signal.aborted) return;
            const currentStatus = await sessionRunStatus(
              recoveryTarget.sessionId,
              recoveryTarget.runId,
              controller.signal,
            );
            if (currentStatus === 'completed') {
              await loadPackage(recoveryTarget.sessionId);
            }
            if (!isRecoverableRunStatus(currentStatus)) {
              break;
            }
            consecutiveFailures = 0;
          } catch (error) {
            if (controller.signal.aborted) return;
            consecutiveFailures += 1;
            if (consecutiveFailures >= 3) {
              throw error;
            }
          }
          await new Promise<void>((resolve) => window.setTimeout(resolve, 1000));
        }
      } catch (error) {
        if (!controller.signal.aborted) {
          patchAssistant({
            error: t('home.sseResumeFailed', { error: error instanceof Error ? error.message : String(error) }),
          });
        }
      } finally {
        if (!controller.signal.aborted) {
          setRunning(false);
          setRecoveryTarget((current) => (
            current?.sessionId === recoveryTarget.sessionId && current.runId === recoveryTarget.runId
              ? null
              : current
          ));
        }
      }
    })();

    return () => {
      controller.abort();
    };
  }, [recoveryTarget, onFrame, patchAssistant, loadPackage, applyProtocolMessages]);

  const run = useCallback(
    async (
      kind: 'generate' | 'edit' | 'apply' | 'template',
      payload: unknown,
      files: File[] = [],
    ) => {
      setRunning(true);
      setStatus('');
      const assistantLine = beginAssistant();
      let streamedSessionID = '';
      streamCursorRef.current = {};
      try {
        const handlers = {
          onFrame,
          onCursor: (sequences: RunCursorMap) => Object.assign(streamCursorRef.current, sequences),
          onFinal: (summary: Record<string, unknown>) => {
            // Source done.result is authoritative. Render it immediately;
            // the detail reload below remains the durable fallback.
            applyProtocolMessages(decodeAGenUIResult(summary.result));
          },
          onSession: (nextSessionID: string) => {
            streamedSessionID = nextSessionID;
            internalSessionRef.current = nextSessionID;
            setSessionId(nextSessionID);
            notifyRecentChatsChanged(nextSessionID);
            // 流式运行中只替换地址栏，不触发路由重挂；
            // 否则首轮拿到真实 session 后刷新会退回 /admin/home 丢失当前会话。
            const url = new URL(window.location.href);
            url.searchParams.set('session', nextSessionID);
            url.searchParams.delete('new');
            url.searchParams.delete('prompt');
            url.searchParams.delete('template');
            window.history.replaceState(null, '', url.toString());
          },
      };
        let outcome: StreamOutcome;
        if (kind === 'generate') {
          outcome = await sessionApi.generate(sessionId, payload as string, handlers, files);
        } else if (kind === 'edit') {
          outcome = await sessionApi.edit(sessionId, payload as string, handlers, files);
        } else if (kind === 'apply') {
          outcome = await sessionApi.applyProtocol(sessionId, payload as unknown[], handlers);
        } else {
          outcome = await sessionApi.openTemplate(sessionId, payload as string, handlers);
        }
        if (!outcome.interrupted && !outcome.failed) {
          await loadPackage(streamedSessionID || sessionId);
        }
      } catch (err) {
        // Live process updates have a single owner: the SSE stream above. Only
        // consult the durable transcript after a transport failure, where it is
        // a recovery source rather than a concurrent second event channel.
        await recoverProgressFromSession(streamedSessionID || sessionId, assistantLine).catch(() => undefined);
        patchAssistant({ error: String(err) });
      } finally {
        setRunning(false);
        notifyRecentChatsChanged(streamedSessionID || internalSessionRef.current || sessionId);
      }
    },
    [sessionId, beginAssistant, onFrame, loadPackage, patchAssistant, applyProtocolMessages, recoverProgressFromSession],
  );

  const answerControl = useCallback(
    async (line: ChatLine, answers: ControlAnswer[]) => {
      if (!line.control || line.controlAnswered || running) {
        return;
      }
      const { runId, id } = line.control;
      const normalizedAnswers = line.control.questions.flatMap((question) => {
        const answer = answers.find((item) => item.questionID === question.id);
        if (!answer) return [];
        const option = answer.optionID
          ? question.options.find((item) => item.id === answer.optionID)
          : undefined;
        const text = answer.text?.trim() || option?.label || '';
        return text ? [{ ...answer, text }] : [];
      });
      if (normalizedAnswers.length !== line.control.questions.length) {
        return;
      }
      const answerSummary = normalizedAnswers.map((answer) => {
        const question = line.control!.questions.find((item) => item.id === answer.questionID);
        return question?.header ? `${question.header}：${answer.text}` : answer.text;
      }).join('\n');
      const placeholder: ChatLine = {
        id: lineSeq++,
        role: 'assistant',
        chips: [],
        artifacts: [],
      };
      line.controlAnswered = true;
      answeredControlIDsRef.current.add(id);
      assistantRef.current = placeholder;
      setLines((prev) => [
        ...prev.map((item) => (item.id === line.id ? { ...line } : item)),
        { id: lineSeq++, role: 'user', text: answerSummary },
        placeholder,
      ]);
      setRunning(true);
      try {
        const outcome = await resolveControl(runId, id, normalizedAnswers, {
          onFrame,
          onCursor: (sequences: RunCursorMap) => Object.assign(streamCursorRef.current, sequences),
          onFinal: (summary: Record<string, unknown>) => {
            applyProtocolMessages(decodeAGenUIResult(summary.result));
          },
          onSession: (nextSessionID: string) => {
            internalSessionRef.current = nextSessionID;
            setSessionId(nextSessionID);
            notifyRecentChatsChanged(nextSessionID);
            const url = new URL(window.location.href);
            url.searchParams.set('session', nextSessionID);
            url.searchParams.delete('new');
            url.searchParams.delete('prompt');
            url.searchParams.delete('template');
            window.history.replaceState(null, '', url.toString());
          },
        }, { cursor: streamCursorRef.current });
        if (outcome && !outcome.interrupted && !outcome.failed) {
          await loadPackage(sessionId);
        }
      } catch (err) {
        placeholder.error = err instanceof Error ? err.message : String(err);
        setLines((prev) => prev.map((item) => (
          item.id === placeholder.id ? { ...placeholder } : item
        )));
      } finally {
        setRunning(false);
        notifyRecentChatsChanged(internalSessionRef.current || sessionId);
      }
    },
    [loadPackage, onFrame, running, sessionId, applyProtocolMessages, t],
  );

  // 设计确认卡的续轮格式以源 prompt 为准：`[AGENUI_DESIGN_CONFIRMATION]\naction=<id>\n<文本>`，
  // 作为新一轮 generate 发送；界面只展示人类可读标签。
  const answerConfirmation = useCallback(
    (line: ChatLine, option: { id: string; label: string; type: string }, text?: string) => {
      if (!line.confirmation || running) {
        return;
      }
      const feedback = (text ?? '').trim();
      if (option.type === 'input' && !feedback) {
        return;
      }
      const answerSummary = feedback || option.label;
      line.confirmation = undefined;

      // Source ChatPanel keeps the current design locally for save_design_only;
      // it does not dispatch another Agent turn.
      if (option.id === 'save_design_only') {
        setLines((prev) => [
          ...prev.map((item) => (item.id === line.id ? { ...line } : item)),
          { id: lineSeq++, role: 'user', text: answerSummary },
          { id: lineSeq++, role: 'assistant', text: t('home.styleKept') },
        ]);
        return;
      }

      setLines((prev) => [
        ...prev.map((item) => (item.id === line.id ? { ...line } : item)),
        { id: lineSeq++, role: 'user', text: answerSummary },
      ]);
      const query = [
        '[AGENUI_DESIGN_CONFIRMATION]',
        `action=${option.id}`,
        feedback ? `feedback=${feedback}` : '',
      ].filter(Boolean).join('\n');
      run('generate', query);
    },
    [running, run, t],
  );

  const send = useCallback(() => {
    const text = input.trim();
    if ((!text && attachments.length === 0) || running) {
      return;
    }
    const prompt = text || '请参考上传的图片生成界面。';
    const files = [...attachments];
    const attachmentViews = files.map((file): ChatAttachmentView => {
      const previewURL = URL.createObjectURL(file);
      attachmentPreviewURLsRef.current.add(previewURL);
      return { name: file.name, mimeType: file.type, type: 'image', previewURL };
    });
    setInput('');
    setAttachments([]);
    if (hasPackage) {
      setLines((prev) => [...prev, {
        id: lineSeq++,
        role: 'user',
        text: prompt,
        attachments: attachmentViews,
      }]);
      run('edit', prompt, files);
    } else {
      setLines((prev) => [...prev, {
        id: lineSeq++,
        role: 'user',
        text: prompt,
        attachments: attachmentViews,
      }]);
      run('generate', prompt, files);
    }
  }, [input, attachments, running, hasPackage, run]);

  const chooseNextStep = useCallback((prompt: string) => {
    if (running) return;
    setInput(prompt);
    window.requestAnimationFrame(() => {
      const inputElement = composerInputRef.current;
      if (!inputElement) return;
      inputElement.focus();
      inputElement.setSelectionRange(prompt.length, prompt.length);
    });
  }, [running]);

  const publishPackage = useCallback(async () => {
    if (!sessionId || publishing) return;
    const latestTarget = await loadPublicationTarget();
    if (latestTarget && (!latestTarget.configured || !latestTarget.enabled)) {
      setStatus(t('home.publishNoTarget'));
      setStatusLink({ href: '/admin/settings/publication', label: t('home.publishGoConfigure') });
      setPublicationGuideOpen(true);
      return;
    }
    setPublishing(true);
    setStatusLink(null);
    setStatus(t('home.publishing'));
    try {
      const response = await fetch(`/api/v1/agenui/agent/sessions/${encodeURIComponent(sessionId)}/publish`, {
        method: 'POST',
      });
      const body = await response.json().catch(() => ({})) as { deliveryId?: string; error?: string };
      if (!response.ok) {
        if (body.error?.includes('no enabled target is configured')) {
          setPublicationTarget({ configured: false, enabled: false });
          setStatus(t('home.publishNoTarget'));
          setStatusLink({ href: '/admin/settings/publication', label: t('home.publishGoConfigure') });
          setPublicationGuideOpen(true);
          return;
        }
        throw new Error(body.error || `HTTP ${response.status}`);
      }
      setStatus(`${t('home.publishSucceeded')}${body.deliveryId ? ` · ${body.deliveryId}` : ''}`);
    } catch (cause) {
      setStatus(t('home.publishFailed', { message: cause instanceof Error ? cause.message : String(cause) }));
    } finally {
      setPublishing(false);
    }
  }, [loadPublicationTarget, publishing, sessionId, t]);

  const addReferenceImages = useCallback((files: Iterable<File> | null) => {
    if (!files) return;
    const accepted = Array.from(files).filter(isSupportedReferenceImage);
    setAttachments((current) => [...current, ...accepted].slice(0, MAX_REFERENCE_IMAGES));
    if (attachmentInputRef.current) attachmentInputRef.current.value = '';
  }, []);

  const pasteReferenceImages = useCallback((event: ReactClipboardEvent<HTMLTextAreaElement>) => {
    if (running) return;
    const files = referenceImagesFromClipboard(event.clipboardData.items);
    if (files.length === 0) return;
    event.preventDefault();
    addReferenceImages(files);
  }, [addReferenceImages, running]);

  // Boot: auto-run prompt from /create, open a template, or replay the
  // persisted session (refresh keeps the conversation).
  useEffect(() => {
    const prompt = params.get('prompt');
    const template = params.get('template');
    const newChat = params.get('new') === '1';
    const bootKey = `${requestedSessionID}\u0000${prompt ?? ''}\u0000${template ?? ''}\u0000${newChat}`;
    if (booted.current === bootKey) {
      return;
    }
    booted.current = bootKey;
    // Next.js observes native history.replaceState. When the stream writes the
    // newly allocated session into the URL, retain the live state instead of
    // replaying a half-written session over it. External navigation never sets
    // this marker and continues through the normal durable replay below.
    if (internalSessionRef.current === requestedSessionID) {
      internalSessionRef.current = '';
      return;
    }
    if (newChat) {
      setLines([]);
      setInput('');
      setAttachments([]);
      setHasPackage(false);
      setDraftPreview(false);
      setPkg(null);
      setRecoveryTarget(null);
      setStructureSource('');
      selectImplementationView('structure');
      assistantRef.current = null;
    } else if (prompt) {
      setLines([{ id: lineSeq++, role: 'user', text: prompt }]);
      run('generate', prompt);
    } else if (template) {
      run('template', template);
    } else {
      (async () => {
        const replay = await sessionReplay(requestedSessionID);
        const { frames, activeRun, cursor } = replay;
        streamCursorRef.current = { ...(cursor ?? {}) };
        if (frames.length === 0 && !activeRun) {
          await loadPackage(requestedSessionID);
          return;
        }
        const rebuilt: ChatLine[] = [];
        let assistant: ChatLine | null = null;
        const activeAnsweredControlIDs = new Set<string>();
        for (const frame of frames) {
          if (frame.event_type === 'user_message_received') {
            rebuilt.push({
              id: lineSeq++,
              role: 'user',
              text: String(frame.payload?.text ?? ''),
              attachments: attachmentsFromFrame(frame),
            });
            assistant = { id: lineSeq++, role: 'assistant', chips: [], artifacts: [] };
            rebuilt.push(assistant);
            continue;
          }
          if (frame.event_type === 'control_request_created' && frame.payload?.answered === true) {
            const controlID = String(frame.payload?.control_id ?? '');
            answeredControlIDsRef.current.add(controlID);
            if (activeRun && frame.run_id === activeRun.runId && controlID) {
              activeAnsweredControlIDs.add(controlID);
            }
          }
          if (!assistant) {
            assistant = { id: lineSeq++, role: 'assistant', chips: [], artifacts: [] };
            rebuilt.push(assistant);
          }
          applyFrameToLine(frame, assistant);
        }
        if (!assistant && activeRun) {
          assistant = { id: lineSeq++, role: 'assistant', chips: [], artifacts: [] };
          rebuilt.push(assistant);
        }
        assistantRef.current = assistant;
        setLines(rebuilt);
        await loadPackage(requestedSessionID);
        if (activeRun && assistant) {
          setRecoveryTarget({
            sessionId: requestedSessionID,
            runId: activeRun.runId,
            assistantLineId: assistant.id,
            answeredControlIDs: Array.from(activeAnsweredControlIDs),
            existingText: assistant.text ?? '',
          });
        }
      })();
    }
  }, [params, requestedSessionID, run, sessionId, loadPackage, applyProtocolMessages, selectImplementationView]);

  const latestAssistantError = [...lines].reverse().find((line) => line.role === 'assistant')?.error;
  const workbenchStatus = running
    ? {
        label: t(hasPackage ? 'home.workbenchStatusUpdating' : 'home.workbenchStatusGenerating'),
        view: hasPackage ? implementationView : null,
        dotClassName: 'bg-sky-500',
      }
    : !hasPackage
      ? {
          label: t('home.workbenchStatusWaiting'),
          view: null,
          dotClassName: 'bg-neutral-300',
        }
      : implementation.bundleReady
        ? {
            label: t('home.workbenchStatusReady'),
            view: 'runtime' as const,
            dotClassName: 'bg-emerald-500',
          }
        : {
            label: t('home.workbenchStatusPendingBinding'),
            view: 'binding' as const,
            dotClassName: 'bg-amber-500',
          };
  const workspaceColumns = protocolCollapsed
    ? `minmax(280px, ${panelSizes.chat}fr) 8px minmax(320px, ${panelSizes.preview + panelSizes.protocol}fr) 0px 52px`
    : `minmax(280px, ${panelSizes.chat}fr) 8px minmax(320px, ${panelSizes.preview}fr) 8px minmax(300px, ${panelSizes.protocol}fr)`;

  return (
    <div className="flex h-full flex-col bg-surface">
      {/* top bar */}
      <div className="flex h-12 shrink-0 items-center gap-2 border-b border-line bg-surface px-4">
        <span aria-hidden="true" className="h-5 w-1 rounded-full bg-inverse" />
        <span className="text-[13px] font-semibold tracking-[0.01em] text-neutral-800">{t('home.workbench')}</span>
        <button
          type="button"
          onClick={() => workbenchStatus.view && openImplementation(workbenchStatus.view)}
          disabled={!workbenchStatus.view}
          title={workbenchStatus.view ? t('home.workbenchStatusOpen') : undefined}
          className="ml-2 inline-flex h-7 min-w-0 max-w-[230px] items-center gap-2 rounded-full border border-line bg-surface-raised px-2.5 text-[10px] font-medium text-neutral-600 transition-colors hover:border-neutral-300 hover:bg-neutral-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400 disabled:cursor-default disabled:hover:border-line disabled:hover:bg-surface-raised sm:max-w-none"
        >
          <span aria-hidden="true" className={`h-1.5 w-1.5 shrink-0 rounded-full ${workbenchStatus.dotClassName}`} />
          <span className="truncate" role="status" aria-live="polite">{workbenchStatus.label}</span>
        </button>
        <div className="ml-auto flex items-center gap-2">
          {status && <span className="text-[11px] text-neutral-500">{status}</span>}
          {statusLink && (
            <Link href={statusLink.href} className="text-[11px] text-info hover:underline">
              {statusLink.label}
            </Link>
          )}
        </div>
      </div>

      <div aria-live="polite">
        {modelConfigured === false && (
          <div className="shrink-0 flex items-center gap-3 border-b border-amber-200 bg-amber-50 px-4 py-2.5 text-amber-950">
            <TriangleAlert aria-hidden="true" className="h-5 w-5 shrink-0 text-amber-600" />
            <div className="min-w-0">
              <p className="text-[13px] font-semibold">{t('home.modelUnconfigured')}</p>
              <p className="mt-0.5 text-[12px] text-amber-800">{t('home.modelUnconfiguredHint')}</p>
            </div>
            <Link
              href="/admin/settings"
              className="inline-flex h-8 shrink-0 items-center rounded-md border border-amber-300 bg-surface-raised px-3 text-[12px] font-medium text-amber-900 transition-colors hover:border-amber-400 hover:bg-amber-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-amber-500 focus-visible:ring-offset-2"
            >
              {t('home.goToSettings')}
            </Link>
          </div>
        )}
      </div>

      {/* three panes */}
      <div
        ref={workspaceRef}
        className="grid min-h-0 flex-1 overflow-x-auto transition-[grid-template-columns] duration-300 motion-reduce:transition-none"
        style={{ gridTemplateColumns: workspaceColumns }}
      >
        {/* chat */}
        <div className="flex min-h-0 flex-col bg-surface">
          <div className="flex h-12 shrink-0 items-center border-b border-line bg-surface px-5">
            <div>
              <div className="text-[13px] font-semibold text-neutral-800">{t('home.chatTitle')}</div>
              <div className="text-[10px] text-neutral-400">{t('home.chatSubtitle')}</div>
            </div>
          </div>
          <div className="flex-1 space-y-4 overflow-y-auto px-5 py-4" ref={chatRef}>
            {lines.length === 0 && (
              <div className="mx-auto mt-12 max-w-[310px] text-center">
                <div className="mx-auto inline-flex h-10 w-10 items-center justify-center rounded-xl bg-inverse text-inverse-fg shadow-sm">
                  <LayoutTemplate aria-hidden="true" className="h-[18px] w-[18px]" />
                </div>
                <p className="mt-4 text-[14px] font-semibold text-neutral-800">{t('home.emptyTitle')}</p>
                <p className="mt-1.5 text-[12px] leading-5 text-neutral-500">
                  {t('home.emptyHint')}
                </p>
              </div>
            )}
            {lines.map((line) =>
              line.role === 'user' ? (
                <div
                  key={line.id}
                  className="ml-auto max-w-[88%] whitespace-pre-wrap break-words rounded-2xl rounded-br-md bg-bubble px-4 py-3 text-[13px] leading-6 text-bubble-fg shadow-sm"
                >
                  {line.text}
                  {(line.attachments ?? []).length > 0 && (
                    <div className="mt-2 flex flex-wrap gap-1.5">
                      {line.attachments!.map((attachment, index) => {
                        const src = attachment.previewURL || (attachment.artifactRef
                          ? artifactContentURL(attachment.artifactRef)
                          : '');
                        return (
                          <div key={`${attachment.artifactRef ?? attachment.name}-${index}`} className="overflow-hidden rounded-lg bg-black/10">
                            {src && (attachment.type === 'image' || attachment.mimeType?.startsWith('image/')) ? (
                              // The source is either a local object URL or the same-origin Harness artifact proxy.
                              // eslint-disable-next-line @next/next/no-img-element
                              <img src={src} alt={attachment.name} className="max-h-40 max-w-full object-contain" />
                            ) : null}
                            <span className="flex items-center gap-1 px-2 py-1 text-[10px]">
                              <ImageIcon className="h-3 w-3" />{attachment.name}
                            </span>
                          </div>
                        );
                      })}
                    </div>
                  )}
                </div>
              ) : (
                <div
                  key={line.id}
                  className="max-w-[94%] rounded-2xl rounded-bl-md border border-neutral-200/80 bg-surface px-4 py-3.5 shadow-sm"
                >
                  {line.commentary && (
                    <div className="mb-3 flex items-start gap-2 text-[12px] leading-5 text-neutral-500">
                      <span aria-hidden="true" className="mt-[7px] h-1.5 w-1.5 shrink-0 rounded-full bg-info" />
                      <span className="min-w-0 whitespace-pre-wrap break-words">{line.commentary}</span>
                    </div>
                  )}
                  {line.text && <div className="whitespace-pre-wrap text-[13px] leading-6 text-neutral-800">{line.text}</div>}
                  {!line.text && !line.commentary && !line.progress && !line.nextSteps && (line.chips ?? []).length === 0 &&
                    (line.artifacts ?? []).length === 0 && !line.error && !line.control && !line.confirmation && (
                    running && assistantRef.current?.id === line.id ? (
                      <div className="flex items-start gap-2.5 py-0.5">
                        <Loader2 className="mt-0.5 h-4 w-4 shrink-0 animate-spin text-neutral-500" />
                        <div>
                          <p className="text-[12px] font-medium text-neutral-700">{t('home.generating')}</p>
                          <p className="mt-0.5 text-[10px] leading-4 text-neutral-400">{t('home.generatingHint')}</p>
                        </div>
                      </div>
                    ) : null
                  )}
                  {line.progress && (line.progress.activities.length > 0 || (line.progress.subagents?.length ?? 0) > 0) && (
                    <ProgressRows
                      timeline={line.progress}
                      rootRunActive={running && assistantRef.current?.id === line.id}
                    />
                  )}
                  {!line.progress && (line.chips ?? []).length > 0 && (
                    <div className="mt-2 space-y-2">
                      {(line.chips ?? []).map((chip) => {
                        const running = chip.state === 'run';
                        return (
                          <div key={chip.id}>
                            <div className="flex items-center gap-2 text-[12px]">
                              {chip.state === 'done' ? (
                                <Check className="w-3.5 h-3.5 shrink-0 text-emerald-600" />
                              ) : chip.state === 'err' ? (
                                <X className="w-3.5 h-3.5 shrink-0 text-red-500" />
                              ) : (
                                <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-progress" />
                              )}
                              <span className={running ? 'font-medium text-info' : 'text-neutral-800'}>
                                {chip.label}
                                {running ? ' ···' : ''}
                              </span>
                            </div>
                            {chip.detail && (
                              <div className="ml-[22px] mt-0.5 text-[10px] leading-4 text-neutral-500">
                                {chip.detail}
                              </div>
                            )}
                            {chip.warning && (
                              <div className="ml-[22px] mt-0.5 text-[10px] leading-4 text-amber-600">
                                {chip.warning}
                              </div>
                            )}
                          </div>
                        );
                      })}
                    </div>
                  )}
                  {line.nextSteps && !(running && assistantRef.current?.id === line.id) && (
                    <NextStepsCard plan={line.nextSteps} disabled={running} onSelect={chooseNextStep} />
                  )}
                  {(line.artifacts ?? []).length > 0 && (
                    <div className="mt-1.5 space-y-0.5">
                      {(line.artifacts ?? []).map((a, i) => (
                        <div key={i} className="font-mono text-[10px] text-neutral-400">
                          artifact_created · {a}
                        </div>
                      ))}
                    </div>
                  )}
                  {line.error && (
                    <div className="mt-1.5 rounded bg-red-50 text-red-600 px-2 py-1 text-[11px]">
                      {runFailureText(line.error, t)}
                    </div>
                  )}
                  {line.control && !line.controlAnswered && (
                    <AskUserCard
                      key={line.control.id}
                      control={line.control}
                      disabled={running}
                      onSubmit={(answers) => answerControl(line, answers)}
                    />
                  )}
                  {line.confirmation && (
                    <div className="mt-2 rounded-lg border border-sky-200 bg-sky-50 p-2.5">
                      <div className="text-[12px] leading-relaxed text-sky-800">{line.confirmation.question}</div>
                      <div className="mt-2 flex flex-wrap gap-2">
                        {line.confirmation.options.filter((opt) => opt.type !== 'input').map((opt) => (
                          <button
                            key={opt.id}
                            onClick={() => answerConfirmation(line, opt)}
                            className="h-7 rounded-md bg-info px-2.5 text-[11px] text-info-fg hover:bg-info-hover"
                          >
                            {opt.label}
                          </button>
                        ))}
                      </div>
                      {line.confirmation.options.filter((opt) => opt.type === 'input').map((opt) => (
                        <ControlFreeInput
                          key={opt.id}
                          placeholder={opt.placeholder ?? t('home.customAnswerPlaceholder')}
                          onSend={(text) => answerConfirmation(line, opt, text)}
                        />
                      ))}
                    </div>
                  )}
                  {line.turnCompleted && !line.error && (
                    <div className="mt-3 border-t border-neutral-100 pt-3 text-[12px] text-neutral-500">
                      {t('home.turnDone')}
                    </div>
                  )}
                </div>
              ),
            )}
          </div>
          <div className="shrink-0 border-t border-neutral-100 bg-neutral-50/70 p-4">
            <div className="rounded-2xl border border-line-strong bg-surface-raised p-2.5 shadow-sm transition-colors focus-within:border-neutral-400">
              {attachments.length > 0 && (
                <div className="mb-2 flex flex-wrap gap-1.5 px-1">
                  {attachments.map((file, index) => (
                    <span key={`${file.name}-${file.lastModified}`} className="inline-flex max-w-full items-center gap-1.5 rounded-lg bg-neutral-100 px-2 py-1 text-[10px] text-neutral-600">
                      <ImageIcon className="h-3 w-3 shrink-0" />
                      <span className="max-w-[180px] truncate">{file.name}</span>
                      <button
                        type="button"
                        onClick={() => setAttachments((current) => current.filter((_, itemIndex) => itemIndex !== index))}
                        aria-label={t('home.removeAttachment', { name: file.name })}
                        className="rounded p-0.5 hover:bg-neutral-200"
                      >
                        <X className="h-3 w-3" />
                      </button>
                    </span>
                  ))}
                </div>
              )}
              <textarea
                ref={composerInputRef}
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onPaste={pasteReferenceImages}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault();
                    send();
                  }
                }}
				placeholder={hasPackage ? t('home.inputPlaceholderEdit') : t('home.inputPlaceholderNew')}
                rows={3}
                aria-label={t('home.inputLabel')}
                className="block min-h-[72px] w-full resize-none border-0 bg-transparent px-1 py-0.5 text-[13px] leading-5 text-neutral-800 outline-none placeholder:text-neutral-500 focus-visible:!outline-none"
              />
              <div className="mt-1 flex items-center justify-between gap-3 pl-1">
                <div className="flex items-center gap-2">
                  <input
                    ref={attachmentInputRef}
                    type="file"
                    multiple
                    accept="image/png,image/jpeg,image/webp"
                    onChange={(event) => addReferenceImages(event.target.files)}
                    className="hidden"
                  />
                  <button
                    type="button"
                    onClick={() => attachmentInputRef.current?.click()}
                    disabled={running || attachments.length >= MAX_REFERENCE_IMAGES}
                    aria-label={t('home.addImage')}
                    title={locale === 'zh'
                      ? '上传或粘贴参考图片（PNG、JPEG、WebP，单张不超过 10MB）'
                      : 'Upload or paste reference images (PNG, JPEG, WebP, up to 10MB each)'}
                    className="inline-flex h-8 items-center gap-1 rounded-lg px-2 text-[10px] text-neutral-500 hover:bg-neutral-100 disabled:opacity-35"
                  >
                    <Paperclip className="h-3.5 w-3.5" />{t('home.referenceImage')}
                  </button>
                  <span className="text-[10px] text-neutral-500">
                    {locale === 'zh'
                      ? '可粘贴图片 · Enter 发送 · Shift + Enter 换行'
                      : 'Paste images · Enter to send · Shift + Enter for a new line'}
                  </span>
                </div>
                <button
                  type="button"
                  onClick={send}
                  disabled={running || (!input.trim() && attachments.length === 0)}
                  aria-label={t('home.sendLabel')}
                  className="inline-flex h-9 items-center gap-1.5 rounded-xl bg-inverse px-3 text-[12px] font-medium text-inverse-fg transition-colors hover:bg-inverse-hover disabled:cursor-not-allowed disabled:opacity-35"
                >
                  {running ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <SendHorizontal className="h-3.5 w-3.5" />}
                  {t('common.send')}
                </button>
              </div>
            </div>
          </div>
        </div>

        <PanelResizeHandle
          divider="chat-preview"
          position={panelSizes.chat}
          onPointerDown={beginPanelResize}
          onKeyDown={resizePanelsByKeyboard}
          onReset={resetPanelSizes}
        />

        {/* preview */}
        <div className="overflow-y-auto bg-surface-sunken p-4">
          <div className="sticky top-0 z-20 -mx-1 mb-2 flex items-center justify-between gap-3 bg-surface-sunken/95 px-1 pb-2 backdrop-blur-sm">
            <div className="min-w-0">
              <div className="text-[12px] font-semibold text-neutral-700">{t('home.preview')}</div>
              {hasPackage && (
                <div className="mt-0.5 text-[9px] text-neutral-400">{t('home.previewLive')}</div>
              )}
            </div>
            <label className="group relative flex h-8 min-w-0 max-w-[210px] items-center gap-1.5 rounded-lg border border-line bg-surface-raised px-2.5 text-[10px] text-neutral-600 shadow-sm transition-colors hover:border-neutral-300 hover:bg-surface">
              <Smartphone aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-neutral-500" />
              <span className="sr-only">{t('home.previewDevice')}</span>
              <select
                value={previewDeviceId}
                onChange={(event) => setPreviewDeviceId(event.target.value as PreviewDeviceID)}
                aria-label={t('home.previewDeviceSelect')}
                className="min-w-0 flex-1 cursor-pointer appearance-none bg-surface-raised pr-4 font-medium text-neutral-700 outline-none"
              >
                {PREVIEW_DEVICES.map((device) => (
                  <option key={device.id} value={device.id}>
                    {device.labelKey ? t(device.labelKey) : device.label} · {device.width}×{device.height}
                  </option>
                ))}
              </select>
              <ChevronDown aria-hidden="true" className="pointer-events-none absolute right-2 h-3 w-3 text-neutral-400" />
            </label>
          </div>
          <PhoneShellWrapper
            colorScheme={previewColorScheme}
            deviceWidth={previewDevice.width}
            deviceHeight={previewDevice.height}
            platform={previewDevice.platform}
          >
            <div
              className="min-h-full"
              data-agenui-preview-theme
              data-color-scheme={previewColorScheme}
              style={{ colorScheme: previewColorScheme }}
            >
              <A2uiPreview messages={previewMessages} colorScheme={previewColorScheme} />
              {running && !pkg && <PreviewSkeleton colorScheme={previewColorScheme} />}
            </div>
          </PhoneShellWrapper>
          {!pkg && (
            <div className="mt-3 text-center text-[11px] text-neutral-400">
              {t('home.previewEmpty')}
            </div>
          )}
        </div>

        <PanelResizeHandle
          divider="preview-protocol"
          position={panelSizes.chat + panelSizes.preview}
          onPointerDown={beginPanelResize}
          onKeyDown={resizePanelsByKeyboard}
          onReset={resetPanelSizes}
          hidden={protocolCollapsed}
        />

        {/* protocol editor */}
        <div className="flex min-h-0 flex-col bg-surface">
          {protocolCollapsed ? (
            <div className="flex min-h-0 flex-1 flex-col items-center gap-3 py-3">
              <button
                type="button"
                onClick={() => setProtocolCollapsed(false)}
                aria-label={t('home.expandDsl')}
                title={t('home.expandDsl')}
                className="inline-flex h-8 w-8 items-center justify-center rounded-lg text-neutral-500 transition-colors hover:bg-neutral-100 hover:text-neutral-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
              >
                <PanelRightOpen aria-hidden="true" className="h-4 w-4" />
              </button>
              <span className="text-[10px] font-medium tracking-wider text-neutral-400 [writing-mode:vertical-rl]">{t('home.implementationDetails')}</span>
            </div>
          ) : (
            <>
              <div className="flex h-10 shrink-0 items-center gap-2 border-b border-line bg-surface px-3">
                <Layers3 aria-hidden="true" className="h-4 w-4 text-neutral-500" />
                <span className="text-[12px] font-semibold text-neutral-700">{t('home.implementationDetails')}</span>
                {running && (
                  <span className="rounded-full bg-sky-50 px-2 py-0.5 text-[9px] font-medium text-sky-700">
                    {hasPackage ? t('home.newVersion') : t('home.inProgress')}
                  </span>
                )}
                {hasPackage && !draftPreview && sessionId !== 'demo-1' && sessionId !== 'new' && (
                  <div className="ml-auto flex items-center gap-1.5">
                    <button
                      type="button"
                      onClick={() => void publishPackage()}
                      disabled={publishing}
                      title={publicationTarget && (!publicationTarget.configured || !publicationTarget.enabled)
                        ? t('home.publishTitleUnconfigured')
                        : t('home.publishTitle')}
                      className="inline-flex h-7 items-center gap-1.5 rounded-md bg-inverse px-2 text-[10px] font-medium text-inverse-fg disabled:opacity-50"
                    >
                      {publishing ? <Loader2 aria-hidden="true" className="h-3.5 w-3.5 animate-spin" /> : <SendHorizontal aria-hidden="true" className="h-3.5 w-3.5" />}
                      {publicationTarget && (!publicationTarget.configured || !publicationTarget.enabled) ? t('home.publishConfigureAnd') : t('home.publish')}
                    </button>
                    <a
                      href={`/api/v1/agenui/agent/sessions/${encodeURIComponent(sessionId)}/package`}
                      download
                      title={t('home.downloadTitle')}
                      className="inline-flex h-7 items-center gap-1.5 rounded-md border border-neutral-200 px-2 text-[10px] font-medium text-neutral-600 transition-colors hover:bg-neutral-50 hover:text-neutral-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
                    >
                      <Download aria-hidden="true" className="h-3.5 w-3.5" />
                      {t('home.download')}
                    </a>
                  </div>
                )}
                <button
                  type="button"
                  onClick={() => setProtocolCollapsed(true)}
                  aria-label={t('home.collapseDsl')}
                  title={t('home.collapseDsl')}
                  className={`${hasPackage && sessionId !== 'demo-1' && sessionId !== 'new' ? '' : 'ml-auto'} inline-flex h-7 w-7 items-center justify-center rounded-md text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-neutral-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400`}
                >
                  <PanelRightClose aria-hidden="true" className="h-4 w-4" />
                </button>
              </div>

              {hasPackage ? (
                <div className="flex min-h-0 flex-1 flex-col">
                  {draftPreview && (
                    <div className="flex shrink-0 items-center gap-2 border-b border-amber-200 bg-amber-50 px-3 py-2 text-[10px] text-amber-800">
                      {t('home.draftNotice')}
                    </div>
                  )}
                  {running && (
                    <div className="flex shrink-0 items-center gap-2 border-b border-sky-200 bg-sky-50 px-3 py-2 text-[10px] text-sky-700">
                      <Loader2 aria-hidden="true" className="h-3 w-3 animate-spin motion-reduce:animate-none" />
                      {t('home.regenerating')}
                    </div>
                  )}
                  <div
                    role="tablist"
                    aria-label={t('home.implementationSections')}
                    className="grid h-10 shrink-0 grid-cols-3 gap-1 border-b border-line px-2 py-1"
                  >
                    {IMPLEMENTATION_SECTIONS.map((section) => (
                      <button
                        key={section}
                        id={`implementation-section-${section}`}
                        type="button"
                        role="tab"
                        aria-selected={implementationSection === section}
                        aria-controls="implementation-detail-panel"
                        tabIndex={implementationSection === section ? 0 : -1}
                        onClick={() => selectImplementationSection(section)}
                        onKeyDown={(event) => moveImplementationSection(event, section)}
                        title={t(IMPLEMENTATION_SECTION_META[section].descriptionKey)}
                        className={`min-w-0 rounded-md px-2 text-[10px] font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400 ${
                          implementationSection === section
                            ? 'bg-neutral-100 text-neutral-900'
                            : 'text-neutral-500 hover:bg-neutral-50 hover:text-neutral-800'
                        }`}
                      >
                        <span className="block truncate">{t(IMPLEMENTATION_SECTION_META[section].labelKey)}</span>
                      </button>
                    ))}
                  </div>
                  {implementationSection === 'dsl' && (
                    <div
                      role="tablist"
                      aria-label={t('home.dslSections')}
                      className="grid h-9 shrink-0 grid-cols-3 gap-1 border-b border-line bg-surface-sunken px-2 py-1"
                    >
                      {DSL_IMPLEMENTATION_VIEWS.map((view) => {
                        const count = view === 'structure'
                          ? implementation.componentCount
                          : view === 'data'
                            ? implementation.bindings.length
                            : implementation.actions.length;
                        return (
                          <button
                            key={view}
                            id={`implementation-view-${view}`}
                            type="button"
                            role="tab"
                            aria-selected={implementationView === view}
                            aria-controls="implementation-detail-panel"
                            tabIndex={implementationView === view ? 0 : -1}
                            onClick={() => openImplementation(view)}
                            onKeyDown={(event) => moveDSLView(event, view)}
                            title={t(IMPLEMENTATION_META[view].descriptionKey)}
                            className={`flex min-w-0 items-center justify-center gap-1.5 rounded-md px-1.5 text-[9px] font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400 ${
                              implementationView === view
                                ? 'bg-surface-raised text-neutral-800 shadow-sm'
                                : 'text-neutral-500 hover:bg-surface-raised/70 hover:text-neutral-700'
                            }`}
                          >
                            <span className="truncate">{t(IMPLEMENTATION_META[view].labelKey)}</span>
                            <span className="shrink-0 rounded-full bg-neutral-200/70 px-1.5 tabular-nums text-[8px] text-neutral-500">{count}</span>
                          </button>
                        );
                      })}
                    </div>
                  )}
                  <div
                    id="implementation-detail-panel"
                    role="tabpanel"
                    aria-labelledby={implementationSection === 'dsl'
                      ? `implementation-section-dsl implementation-view-${implementationView}`
                      : `implementation-section-${implementationSection}`}
                    className="min-h-0 flex-1"
                  >
                    {implementationView === 'structure' ? (
                      <div className="h-full min-h-0">
                        {structureSource ? (
                          <MonacoEditor
                            height="100%"
                            language="json"
                            theme={theme === 'dark' ? 'vs-dark' : 'light'}
                            value={structureSource}
                            options={{
                              readOnly: true,
                              domReadOnly: true,
                              minimap: { enabled: false },
                              fontSize: 12,
                              folding: true,
                              scrollBeyondLastLine: false,
                              stickyScroll: { enabled: true },
                            }}
                          />
                        ) : (
                          <div className="flex h-full items-center justify-center px-6 text-center text-[11px] text-neutral-400">
                            {t('home.structureEmpty')}
                          </div>
                        )}
                      </div>
                    ) : (
                      <ImplementationDetail
                        view={implementationView}
                        insight={implementation}
                        onDownloadDSL={downloadDSL}
                        onPackageRuntime={packageRuntime}
                      />
                    )}
                  </div>
                </div>
              ) : (
                <div className="min-h-0 flex-1">
                  <ProtocolPendingState running={running} error={latestAssistantError} />
                </div>
              )}
            </>
          )}
          <RuntimeDeliveryEndpoint
            hasPackage={hasPackage}
            draft={draftPreview}
            running={running}
            collapsed={protocolCollapsed}
            publicationTarget={publicationTarget}
          />
        </div>

      </div>
      {publicationGuideOpen && (
        <div
          role="dialog"
          aria-modal="true"
          aria-labelledby="publication-guide-title"
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4 backdrop-blur-[1px]"
          onMouseDown={(event) => {
            if (event.currentTarget === event.target) setPublicationGuideOpen(false);
          }}
        >
          <div className="w-full max-w-[420px] rounded-2xl border border-line bg-surface p-5 shadow-2xl">
            <div className="flex items-start gap-3">
              <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-amber-50 text-amber-700">
                <TriangleAlert aria-hidden="true" className="h-4 w-4" />
              </div>
              <div className="min-w-0">
                <h2 id="publication-guide-title" className="text-[14px] font-semibold text-neutral-900">{t('home.guideTitle')}</h2>
                <p className="mt-1 text-[11px] leading-5 text-neutral-500">
                  {t('home.guideBody')}
                </p>
              </div>
              <button
                type="button"
                onClick={() => setPublicationGuideOpen(false)}
                aria-label={t('home.guideClose')}
                className="ml-auto inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-lg text-neutral-400 hover:bg-neutral-100 hover:text-neutral-700"
              >
                <X aria-hidden="true" className="h-4 w-4" />
              </button>
            </div>
            <div className="mt-4 rounded-xl bg-surface-sunken px-3 py-2.5 text-[10px] leading-5 text-neutral-600">
              {t('home.guideHint')}
            </div>
            <div className="mt-5 flex flex-wrap justify-end gap-2">
              <a
                href={`/api/v1/agenui/agent/sessions/${encodeURIComponent(sessionId)}/package`}
                download
                onClick={() => setPublicationGuideOpen(false)}
                className="inline-flex h-9 items-center gap-1.5 rounded-lg border border-line bg-surface px-3 text-[11px] font-medium text-neutral-700 hover:bg-neutral-50"
              >
                <Download aria-hidden="true" className="h-3.5 w-3.5" />
                {t('home.guideDownloadOnly')}
              </a>
              <Link
                href="/admin/settings/publication"
                target="_blank"
                rel="noreferrer"
                className="inline-flex h-9 items-center gap-1.5 rounded-lg bg-inverse px-3 text-[11px] font-medium text-inverse-fg hover:bg-inverse-hover"
              >
                {t('home.guideConfigure')}
                <SendHorizontal aria-hidden="true" className="h-3.5 w-3.5" />
              </Link>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

export default function ComposerPage() {
  const { t } = useLocale();
  return (
    <Suspense fallback={<div className="p-6 text-[12px] text-neutral-400">{t('common.loading')}</div>}>
      <div className="h-full">
        <ComposerInner />
      </div>
    </Suspense>
  );
}
