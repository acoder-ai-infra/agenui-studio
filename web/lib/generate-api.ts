import {
  projectAnsweredControls,
  projectPendingControl,
  type ConsoleFrame,
  type HarnessProcessView,
  type NativeEvent,
} from './agenui-console-state';

// The Studio consumes Harness Agent Chat directly. ConsoleFrame is only the
// React view model; it is derived in the browser and is not a public transport
// protocol or a second durable event stream.

const mainAgentID = 'agenui_agent';

export interface ConversationSession {
  sessionId: string;
  lastQuery?: string;
  hasPackage?: boolean;
  title?: string;
  activeRunStatus?: string;
  metadata?: { title?: string; query?: string } | null;
}

export interface SessionDetailResult {
  sessionId: string;
  runs: { runId: string; status: string }[];
}

export interface SessionPresentation {
  sessionId: string;
  result?: string;
  bindings?: string;
  apis?: string;
  bindingStatus?: string;
  generationID?: string;
  revision?: number;
  draft?: boolean;
  executable?: boolean;
  publishable?: boolean;
}

export interface RecoverableSessionRun {
  runId: string;
  status: string;
}

export interface SessionReplay {
  frames: CanonicalFrame[];
  activeRun?: RecoverableSessionRun;
  cursor?: RunCursorMap;
}

export function isSessionNotFoundResult(err: unknown): boolean {
  return err instanceof Error && err.message.includes('404');
}

export const conversationApi = {
  async list(): Promise<{ items: ConversationSession[] }> {
    const res = await fetch(`/api/v1/sessions?agent_id=${encodeURIComponent(mainAgentID)}&limit=20&include_active_run=true`, {
      cache: 'no-store',
    });
    if (!res.ok) {
      return { items: [] };
    }
    const data = (await res.json()) as {
      sessions?: Array<{
        id?: string;
        session_id?: string;
        title?: string;
        active_run_status?: string;
      }>;
    };
    return {
      items: (data.sessions ?? []).flatMap((s) => {
        const sessionId = String(s.id ?? s.session_id ?? '');
        if (!sessionId) return [];
        return [{
          sessionId,
          title: s.title,
          activeRunStatus: s.active_run_status,
          metadata: s.title ? { title: s.title } : null,
        }];
      }),
    };
  },
  // Stable console shape used by the sidebar.
  async sessions(_opts?: { tenantId?: number }): Promise<{
    data?: { list: ConversationSession[] };
  }> {
    const { items } = await conversationApi.list();
    return { data: { list: items } };
  },
};

export interface CanonicalFrame extends ConsoleFrame {}

export interface StreamHandlers {
  onSession?: (sessionId: string) => void;
  onFrame?: (frame: CanonicalFrame) => void;
  onStep?: (step: string, status: 'started' | 'completed' | 'failed') => void;
  onTool?: (tool: string, status: 'started' | 'completed' | 'failed') => void;
  onArtifact?: (kind: string, ref: string) => void;
  onText?: (text: string) => void;
  onFinal?: (summary: Record<string, unknown>) => void;
  onError?: (message: string) => void;
  onDone?: (runId?: string) => void;
  onCursor?: (sequences: Record<string, number>) => void;
}

export type RunCursorMap = Record<string, number>;

const agentChatCursorSchemaVersion = 'harness.agent_chat_cursor.v1';

export interface ControlAnswer {
  questionID: string;
  optionID?: string;
  text?: string;
}

export interface ControlOptionView {
  id: string;
  label: string;
  description?: string;
  value?: string;
  type?: 'select' | 'input';
  placeholder?: string;
}

export interface ControlQuestionView {
  id: string;
  header?: string;
  question: string;
  options: ControlOptionView[];
}

export interface StreamOutcome {
  interrupted: boolean;
  failed: boolean;
}

export interface ChatAttachmentView {
  artifactRef?: string;
  name: string;
  mimeType?: string;
  type?: string;
  previewURL?: string;
}

export function artifactContentURL(artifactRef: string): string {
  return `/api/v1/artifacts/content?ref=${encodeURIComponent(artifactRef)}`;
}

async function consumeHarnessSSE(
  res: Response,
  handlers: StreamHandlers,
  initialSessionID = '',
  initialRunID = '',
  synthesizeCompletion = true,
): Promise<StreamOutcome> {
  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let sessionID = res.headers.get('x-harness-session-id') ?? initialSessionID;
  let runID = res.headers.get('x-harness-run-id') ?? initialRunID;
  let interrupted = false;
  let failed = false;
  if (sessionID) handlers.onSession?.(sessionID);
  for (;;) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }
    buffer += decoder.decode(value, { stream: true });
    let sep: number;
    while ((sep = buffer.indexOf('\n\n')) >= 0) {
      const chunk = buffer.slice(0, sep);
      buffer = buffer.slice(sep + 2);
      const dataLine = chunk
        .split('\n')
        .filter((l) => l.startsWith('data: '))
        .map((l) => l.slice(6))
        .join('');
      if (!dataLine || dataLine === '[DONE]') {
        continue;
      }
      try {
        const part = JSON.parse(dataLine) as Record<string, unknown>;
        const type = String(part.type ?? '');
        if (type === 'start') {
          runID = String(part.messageId ?? runID);
        } else if (type === 'text-delta') {
          dispatch(viewEvent('agent_text_delta', { text: String(part.delta ?? '') }, sessionID, runID), handlers);
        } else if (type === 'data-cursor') {
          const data = asRecord(part.data);
          const sequences = asRecord(data.sequences);
          if (data.schemaVersion === agentChatCursorSchemaVersion) {
            const valid = Object.fromEntries(Object.entries(sequences).flatMap(([cursorRunID, value]) => (
              Number.isInteger(value) && Number(value) >= 0 ? [[cursorRunID, Number(value)]] : []
            )));
            handlers.onCursor?.(valid);
          }
        } else if (type === 'data-process') {
          const data = asRecord(part.data);
          dispatch(viewEvent('harness_process', data, sessionID, String(data.runId ?? runID)), handlers);
        } else if (type === 'data-commentary') {
          const data = asRecord(part.data);
          dispatch(viewEvent('agent_commentary', {
            text: String(data.text ?? ''),
            event_id: String(data.eventId ?? ''),
          }, sessionID, String(data.runId ?? runID)), handlers);
        } else if (type === 'tool-input-start') {
          dispatch(viewEvent('tool_call_started', { tool: String(part.toolName ?? '') }, sessionID, runID), handlers);
        } else if (type === 'tool-output-available') {
          dispatch(viewEvent('tool_call_completed', { tool: String(part.toolName ?? '') }, sessionID, runID), handlers);
        } else if (type === 'data-tool-error') {
          const data = asRecord(part.data);
          dispatch(viewEvent('tool_call_failed', {
            tool: String(asRecord(data.payload).tool_name ?? ''),
            error: String(asRecord(data.error).message ?? 'tool failed'),
          }, sessionID, String(data.run_id ?? runID)), handlers);
        } else if (type === 'data-artifact') {
          const data = asRecord(part.data);
          dispatch({
            ...viewEvent('artifact_created', {
              kind: String(asRecord(data.payload).artifact_type ?? ''),
            }, sessionID, String(data.run_id ?? runID)),
            artifact_ref: String(data.artifact_ref ?? asRecord(data.payload).artifact_ref ?? ''),
          }, handlers);
        } else if (type === 'data-control') {
          interrupted = true;
          dispatch(projectLiveControl(sessionID, runID, asRecord(part.data)), handlers);
        } else if (type === 'error') {
          failed = true;
          dispatch(viewEvent('run_failed', { error: String(part.errorText ?? 'run failed') }, sessionID, runID), handlers);
        }
      } catch {
        /* tolerate malformed frames */
      }
    }
  }
  if (synthesizeCompletion && !failed && !interrupted) {
    dispatch(viewEvent('run_completed', {}, sessionID, runID), handlers);
  }
  return { interrupted, failed };
}

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {};
}

function viewEvent(
  event_type: string,
  payload: Record<string, unknown>,
  session_id = '',
  run_id = '',
): CanonicalFrame {
  return {
    schema_version: 'agenui.console.projection.v1',
    sequence: 0,
    event_type,
    view_type: 'agenui',
    ...(session_id ? { session_id } : {}),
    ...(run_id ? { run_id } : {}),
    payload,
  };
}

function projectLiveControl(sessionID: string, runID: string, data: Record<string, unknown>): CanonicalFrame {
  const questions: ControlQuestionView[] = (Array.isArray(data.questions) ? data.questions.map(asRecord) : [])
    .flatMap((question, questionIndex) => {
      const body = String(question.question ?? '').trim();
      if (!body) return [];
      const options: ControlOptionView[] = (Array.isArray(question.options) ? question.options.map(asRecord) : [])
        .flatMap((option, optionIndex) => {
          const [label, value] = splitControlOption(String(option.label ?? option.value ?? ''));
          if (!label || ['其他', '其它', 'other', '自定义'].includes(label.toLowerCase())) return [];
          return [{
            id: `q${questionIndex}_opt${optionIndex}`,
            label,
            description: String(option.description ?? '').trim() || undefined,
            type: 'select' as const,
            ...(value ? { value } : {}),
          }];
        });
      options.push({ id: `q${questionIndex}_other`, label: '', type: 'input' });
      return [{
        id: `q${questionIndex}`,
        header: String(question.header ?? '').trim() || undefined,
        question: body,
        options,
      }];
    });
  const first = questions[0];
  const binding = {
    v: 1,
    s: sessionID,
    r: runID,
    q: String(data.requestId ?? ''),
    t: String(data.controlTicket ?? ''),
    contexts: Array.isArray(data.resumeTargets)
      ? data.resumeTargets.map(String).filter(Boolean)
      : [],
    questions: questions.map((question) => ({
      id: question.id,
      title: question.header,
      body: question.question,
      options: question.options,
    })),
  };
  return viewEvent('control_request_created', {
    control_id: `sdk1.${base64URL(JSON.stringify(binding))}`,
    request_id: String(data.requestId ?? ''),
    question_id: first?.id ?? '',
    question: first?.question ?? String(data.prompt ?? data.title ?? ''),
    options: first?.options.map(({ id, label }) => ({ id, label })) ?? [],
    questions,
  }, sessionID, runID);
}

function splitControlOption(value: string): [string, string] {
  const [display, machine] = value.split('|||', 2).map((part) => part.trim());
  return [display, machine || display];
}

function dispatch(frame: CanonicalFrame, h: StreamHandlers): void {
  h.onFrame?.(frame);
  const step = String(frame.payload?.step ?? '');
  const tool = String(frame.payload?.tool ?? '');
  const message = String(frame.payload?.error ?? frame.error?.message ?? '');
  switch (frame.event_type) {
    case 'runtime_step_started':
      h.onStep?.(step, 'started');
      break;
    case 'runtime_step_completed':
      h.onStep?.(step, 'completed');
      break;
    case 'runtime_step_failed':
      h.onStep?.(step, 'failed');
      break;
    case 'tool_call_started':
      h.onTool?.(tool, 'started');
      break;
    case 'tool_call_completed':
      h.onTool?.(tool, 'completed');
      break;
    case 'tool_call_failed':
      h.onTool?.(tool, 'failed');
      break;
    case 'artifact_created':
      h.onArtifact?.(String(frame.payload?.kind ?? ''), frame.artifact_ref ?? '');
      break;
    case 'agent_text_delta':
      h.onText?.(String(frame.payload?.text ?? ''));
      break;
    case 'final_response':
      h.onFinal?.(frame.payload ?? {});
      break;
    case 'run_failed':
      h.onError?.(message || 'run failed');
      break;
    case 'run_completed':
      h.onDone?.(frame.run_id);
      break;
    default:
      break;
  }
}

export interface HarnessTranscript {
  messages?: Array<{ id?: string; role?: string; text?: string; runId?: string }>;
  runs?: Array<{
    runId?: string;
    parentRunId?: string;
    agentId?: string;
    status?: string;
    errorCode?: string;
    errorMessage?: string;
    startedAt?: string;
    endedAt?: string;
  }>;
  attachments?: Array<{
    artifactRef?: string;
    messageId?: string;
    name?: string;
    mimeType?: string;
    type?: string;
  }>;
  commentaryByRun?: Record<string, Array<{
    eventId?: string;
    runId?: string;
    text?: string;
    createdAt?: string;
  }>>;
  processByRun?: Record<string, HarnessProcessView[]>;
  controlsByRun?: Record<string, Array<{
    requestId?: string;
    createdAt?: string;
    status?: string;
    answerText?: string;
    questions?: Array<{ question?: string; options?: Array<{ label?: string }> }>;
  }>>;
}

const recoverableRunStatuses = new Set(['created', 'running', 'resuming']);

export function isRecoverableRunStatus(status: string | null | undefined): boolean {
  return recoverableRunStatuses.has(String(status ?? ''));
}

export function findRecoverableSessionRun(
  runs: HarnessTranscript['runs'] = [],
): RecoverableSessionRun | undefined {
  for (let index = runs.length - 1; index >= 0; index -= 1) {
    const run = runs[index];
    const runId = String(run?.runId ?? '');
    const status = String(run?.status ?? '');
    if (runId && !run?.parentRunId && isRecoverableRunStatus(status)) {
      return { runId, status };
    }
  }
  return undefined;
}

export function projectHarnessTranscript(transcript: HarnessTranscript): CanonicalFrame[] {
  const frames: CanonicalFrame[] = [];
  const attachmentsByMessage = new Map<string, ChatAttachmentView[]>();
  for (const attachment of transcript.attachments ?? []) {
    const messageID = String(attachment.messageId ?? '');
    const artifactRef = String(attachment.artifactRef ?? '');
    if (!messageID || !artifactRef) continue;
    const grouped = attachmentsByMessage.get(messageID) ?? [];
    grouped.push({
      artifactRef,
      name: String(attachment.name ?? 'attachment'),
      mimeType: String(attachment.mimeType ?? ''),
      type: String(attachment.type ?? ''),
    });
    attachmentsByMessage.set(messageID, grouped);
  }
  const parentByRun = new Map<string, string>();
  for (const run of transcript.runs ?? []) {
    const runID = String(run.runId ?? '');
    if (runID) parentByRun.set(runID, String(run.parentRunId ?? ''));
  }
  const rootRun = (runID: string): string => {
    let current = runID;
    const seen = new Set<string>();
    while (current && !seen.has(current)) {
      seen.add(current);
      const parent = parentByRun.get(current) ?? '';
      if (!parent) return current;
      current = parent;
    }
    return runID;
  };
  const processFramesByRoot = new Map<string, CanonicalFrame[]>();
  const commentaryFramesByRoot = new Map<string, CanonicalFrame[]>();
  const terminalFramesByRoot = new Map<string, CanonicalFrame[]>();
  for (const [fallbackRunID, commentaries] of Object.entries(transcript.commentaryByRun ?? {})) {
    for (const commentary of commentaries ?? []) {
      const runID = String(commentary.runId ?? fallbackRunID);
      const owner = rootRun(runID);
      if (!owner) continue;
      const grouped = commentaryFramesByRoot.get(owner) ?? [];
      grouped.push(viewEvent('agent_commentary', {
        text: String(commentary.text ?? ''),
        event_id: String(commentary.eventId ?? ''),
      }, '', runID));
      commentaryFramesByRoot.set(owner, grouped);
    }
  }
  // The transcript already returns child Run lifecycle. Project it into the
  // same public process shape used by live SSE so refresh and reconnect share
  // one frontend-only reducer; no Harness projection changes are required.
  for (const run of transcript.runs ?? []) {
    const runID = String(run.runId ?? '');
    const parentRunID = String(run.parentRunId ?? '');
    if (!runID || !parentRunID) continue;
    const rawStatus = String(run.status ?? '');
    const status = rawStatus === 'completed' || rawStatus === 'failed'
      || rawStatus === 'cancelled' || rawStatus === 'expired'
      ? rawStatus
      : 'running';
    const owner = rootRun(parentRunID);
    const grouped = processFramesByRoot.get(owner) ?? [];
    grouped.push(viewEvent('harness_process', {
      schemaVersion: 'harness.agent_chat_process.v1',
      eventId: `run:${runID}`,
      processId: `run:${runID}`,
      groupKey: `subagent:run:${runID}`,
      eventType: 'sub_agent_run',
      category: 'subagent',
      title: String(run.agentId ?? '') || '子任务',
      label: String(run.agentId ?? '') || '子任务',
      status,
      runId: runID,
      parentRunId: parentRunID,
      agentId: String(run.agentId ?? ''),
    }, '', runID));
    processFramesByRoot.set(owner, grouped);
  }
  for (const run of transcript.runs ?? []) {
    const runID = String(run.runId ?? '');
    if (!runID || run.parentRunId || run.status !== 'failed') continue;
    terminalFramesByRoot.set(runID, [viewEvent('run_failed', {
      error: String(run.errorCode ?? run.errorMessage ?? '') || 'run failed',
    }, '', runID)]);
  }
  for (const [fallbackRunID, processes] of Object.entries(transcript.processByRun ?? {})) {
    for (const process of processes ?? []) {
      const runID = String(process.runId ?? fallbackRunID);
      const owner = rootRun(String(process.parentRunId ?? '') || runID);
      const grouped = processFramesByRoot.get(owner) ?? [];
      grouped.push(viewEvent('harness_process', process as Record<string, unknown>, '', runID));
      processFramesByRoot.set(owner, grouped);
    }
  }
  const controlBatchesByRoot = new Map<string, Array<{
    createdAt: string;
    frames: CanonicalFrame[];
  }>>();
  for (const [runID, controls] of Object.entries(transcript.controlsByRun ?? {})) {
    const owner = rootRun(runID);
    if (!owner) continue;
    const grouped = controlBatchesByRoot.get(owner) ?? [];
    for (const control of controls ?? []) {
      const controlFrames = projectAnsweredControls({ [runID]: [control] });
      if (controlFrames.length === 0) continue;
      grouped.push({ createdAt: String(control.createdAt ?? ''), frames: controlFrames });
    }
    controlBatchesByRoot.set(owner, grouped);
  }
  const controlFramesByRoot = new Map<string, CanonicalFrame[]>();
  for (const [owner, batches] of Array.from(controlBatchesByRoot.entries())) {
    batches.sort((left, right) => {
      if (!left.createdAt) return right.createdAt ? 1 : 0;
      if (!right.createdAt) return -1;
      return left.createdAt.localeCompare(right.createdAt);
    });
    controlFramesByRoot.set(owner, batches.flatMap((batch) => batch.frames));
  }
  const flushedRoots = new Set<string>();
  const flushRunFacts = (runID: string) => {
    const owner = rootRun(runID);
    if (!owner || flushedRoots.has(owner)) return;
    frames.push(...(commentaryFramesByRoot.get(owner) ?? []));
    frames.push(...(controlFramesByRoot.get(owner) ?? []));
    frames.push(...(processFramesByRoot.get(owner) ?? []));
    frames.push(...(terminalFramesByRoot.get(owner) ?? []));
    flushedRoots.add(owner);
  };
  let currentMessageRoot = '';
  for (const message of transcript.messages ?? []) {
    const text = String(message.text ?? '').trim();
    if (!text) continue;
    const runID = String(message.runId ?? '');
    const owner = rootRun(runID);
    if (message.role === 'user' && currentMessageRoot && owner && owner !== currentMessageRoot) {
      // A failed/interrupted turn may persist no Assistant message. Crossing
      // into the next root Run is therefore also a durable turn boundary: flush
      // the previous Run's controls/processes before rendering the next user.
      flushRunFacts(currentMessageRoot);
    }
    if (owner) currentMessageRoot = owner;
    // A root assistant message is the terminal public response for its turn;
    // place its answered controls and owned root/child processes immediately
    // before that response instead of appending them after the conversation.
    if (message.role !== 'user') flushRunFacts(runID);
    frames.push(viewEvent(
      message.role === 'user' ? 'user_message_received' : 'agent_text_delta',
      {
        text,
        ...(message.role === 'user'
          ? { attachments: attachmentsByMessage.get(String(message.id ?? '')) ?? [] }
          : {}),
      }, '', runID,
    ));
  }
  const remainingRoots = new Set([
    ...Array.from(commentaryFramesByRoot.keys()),
    ...Array.from(processFramesByRoot.keys()),
    ...Array.from(controlFramesByRoot.keys()),
    ...Array.from(terminalFramesByRoot.keys()),
  ]);
  for (const owner of Array.from(remainingRoots)) {
    if (!flushedRoots.has(owner)) flushRunFacts(owner);
  }
  return frames;
}

export async function sessionReplay(sessionId: string, signal?: AbortSignal): Promise<SessionReplay> {
  const res = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionId)}/chat-transcript?detail=verbose&limit=100`,
    { cache: 'no-store', signal },
  );
  if (!res.ok) return { frames: [] };
  const transcript = (await res.json()) as HarnessTranscript;
  const frames = projectHarnessTranscript(transcript);

  // Transcript messages are durable, but a successful Run has no standalone
  // assistant message for its terminal state. Re-apply the latest top-level
  // completion after replay so a refresh keeps the turn-complete affordance.
  const latestTopLevelRun = [...(transcript.runs ?? [])].reverse().find((run) => !run.parentRunId);
  if (latestTopLevelRun?.runId && latestTopLevelRun.status === 'completed') {
    frames.push(viewEvent('run_completed', {}, sessionId, latestTopLevelRun.runId));
  }

  const activeRun = findRecoverableSessionRun(transcript.runs);

  const latestRun = [...(transcript.runs ?? [])].reverse().find((run) => (
    !run.parentRunId && (run.status === 'waiting_control' || run.status === 'interrupted')
  ));
  if (!latestRun?.runId) return { frames, activeRun };
  const eventRes = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionId)}/runs/${encodeURIComponent(latestRun.runId)}/events?after_sequence=0&limit=500`,
    { cache: 'no-store', signal },
  );
  if (!eventRes.ok) return { frames, activeRun };
  const nativeEnvelope = (await eventRes.json()) as { events?: NativeEvent[] };
  const nativeEvents = nativeEnvelope.events ?? [];
  const lastSequence = nativeEvents.reduce((maximum, event) => (
    Math.max(maximum, Number(event.sequence ?? 0))
  ), 0);
  const pending = projectPendingControl(sessionId, latestRun.runId, nativeEvents);
  if (pending) frames.push(pending);
  return {
    frames,
    activeRun,
    ...(lastSequence > 0 ? { cursor: { [latestRun.runId]: lastSequence } } : {}),
  };
}

export async function sessionEvents(sessionId: string): Promise<CanonicalFrame[]> {
  return (await sessionReplay(sessionId)).frames;
}

export async function sessionProgress(sessionId: string): Promise<CanonicalFrame[]> {
  if (!sessionId || sessionId === 'new' || sessionId === 'demo-1') return [];
  const res = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionId)}/chat-transcript?detail=verbose&limit=100`,
    { cache: 'no-store' },
  );
  if (!res.ok) return [];
  const transcript = (await res.json()) as HarnessTranscript;
  return projectHarnessTranscript({ runs: transcript.runs, processByRun: transcript.processByRun });
}

export async function sessionRunStatus(
  sessionId: string,
  runId: string,
  signal?: AbortSignal,
): Promise<string | null> {
  const res = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionId)}/runs`,
    { cache: 'no-store', signal },
  );
  if (res.status === 404) return null;
  if (!res.ok) throw await httpError(res);
  const body = (await res.json()) as {
    runs?: Array<{ run_id?: string; runId?: string; status?: string }>;
  };
  const run = (body.runs ?? []).find((candidate) => (
    String(candidate.run_id ?? candidate.runId ?? '') === runId
  ));
  return run ? String(run.status ?? '') : null;
}

export async function sessionPresentation(sessionId: string): Promise<SessionPresentation | null> {
  if (!sessionId || sessionId === 'new' || sessionId === 'demo-1') return null;
  const res = await fetch(
    `/api/v1/agenui/agent/sessions/${encodeURIComponent(sessionId)}/final`,
    { cache: 'no-store' },
  );
  if (res.status === 204) return null;
  if (!res.ok) return null;
  const artifact = (await res.json()) as {
    sessionId?: string;
    runId?: string;
    result?: unknown;
    bindings?: string;
    apis?: string;
    bindingStatus?: string;
    revision?: number;
    draft?: boolean;
    executable?: boolean;
    publishable?: boolean;
  };
  return {
    sessionId: artifact.sessionId ?? sessionId,
    result: typeof artifact.result === 'string' ? artifact.result : JSON.stringify(artifact.result ?? []),
    bindings: artifact.bindings,
    apis: artifact.apis,
    bindingStatus: artifact.bindingStatus,
    generationID: artifact.runId,
    revision: artifact.revision,
    draft: artifact.draft === true || artifact.executable === false,
    executable: artifact.executable === true,
    publishable: artifact.publishable === true,
  };
}

/** Decode the typed Final Artifact result into protocol messages. */
export function decodeAGenUIResult(value: unknown): Record<string, unknown>[] {
  let parsed = value;
  if (typeof parsed === 'string') {
    const source = parsed.trim();
    if (!source) return [];
    try {
      parsed = JSON.parse(source);
    } catch {
      return [];
    }
  }
  if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
    return [parsed as Record<string, unknown>];
  }
  if (!Array.isArray(parsed)) return [];
  return parsed.filter((message): message is Record<string, unknown> =>
    Boolean(message && typeof message === 'object' && !Array.isArray(message)),
  );
}

// Decode the persisted result shape used by completed and resumed turns.

export async function resumeSessionRun(
  sessionId: string,
  runId: string,
  handlers: StreamHandlers,
  options: { signal?: AbortSignal; synthesizeCompletion?: boolean; cursor?: RunCursorMap } = {},
): Promise<StreamOutcome> {
  const cursorEntries = Object.entries(options.cursor ?? {}).filter(([, sequence]) => (
    Number.isInteger(sequence) && sequence >= 0
  ));
  const cursorQuery = cursorEntries.length > 0
    ? `?cursor=${encodeURIComponent(JSON.stringify({
        schemaVersion: agentChatCursorSchemaVersion,
        sequences: Object.fromEntries(cursorEntries),
      }))}`
    : '';
  const stream = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionId)}/runs/${encodeURIComponent(runId)}/chat-stream${cursorQuery}`,
    { cache: 'no-store', signal: options.signal },
  );
  if (!stream.ok || !stream.body) throw await httpError(stream);
  return consumeHarnessSSE(
    stream,
    handlers,
    sessionId,
    runId,
    options.synthesizeCompletion ?? true,
  );
}

// resolveControl answers an ask_user control interrupt.
export async function resolveControl(
  runId: string,
  id: string,
  answer: ControlAnswer | ControlAnswer[],
  handlers?: StreamHandlers,
  options: { cursor?: RunCursorMap } = {},
): Promise<StreamOutcome | undefined> {
  const binding = decodeControlBinding(id);
  if (!binding) throw new Error('invalid Harness control binding');
  const supplied = Array.isArray(answer) ? answer : [answer];
  const questions = binding.questions ?? [];
  if (questions.length === 0) throw new Error('Harness control has no questions');
  const uniqueAnswers = new Map(supplied.map((item) => [item.questionID, item]));
  if (uniqueAnswers.size !== supplied.length) throw new Error('duplicate Harness control answer');
  const normalized = questions.map((question, questionIndex) => {
    const questionID = String(question.id ?? `q${questionIndex}`);
    const item = uniqueAnswers.get(questionID);
    if (!item) throw new Error('answer every Harness control question before continuing');
    const selectedOption = item.optionID
      ? question.options?.find((option) => option.id === item.optionID)
      : undefined;
    if (item.optionID && !selectedOption) throw new Error('Harness control option does not match its question');
    const text = item.text?.trim() || selectedOption?.label?.trim() || '';
    if (!text) throw new Error('Harness control answer is empty');
    return { item, question, questionID, questionIndex, selectedOption, text };
  });
  if (uniqueAnswers.size !== questions.length) throw new Error('Harness control answer contains an unknown question');
  const targetedAnswer = {
    answers: normalized.map(({ item, questionID, questionIndex, selectedOption, text }) => ({
      question_index: questionIndex,
      question_id: questionID,
      ...(item.optionID
        ? {
          selected_option: {
            id: item.optionID,
            label: text,
            ...(selectedOption?.value ? { value: selectedOption.value } : {}),
          },
        }
        : { text }),
    })),
  };
  const targets = Object.fromEntries(
    (binding.contexts ?? []).filter(Boolean).map((contextID) => [contextID, targetedAnswer]),
  );
  const selected = normalized.map(({ item, questionID, text }) => ({
    question_id: questionID,
    ...(item.optionID ? { option_id: item.optionID } : {}),
    text,
  }));
  const responseText = normalized.length === 1
    ? normalized[0].text
    : normalized.map(({ question, text }) => `${question.title || question.body || question.id}：${text}`).join('\n');
  const res = await fetch(`/api/v1/control-requests/${encodeURIComponent(binding.q)}/responses`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      control_ticket: binding.t,
      client_event_id: `studio-${binding.q}`,
      decision: 'answer',
      response_text: responseText,
      value: selected.length === 1 ? selected[0] : { answers: selected },
      ...(Object.keys(targets).length > 0 ? { targets } : {}),
    }),
  });
  if (!res.ok) {
    throw await httpError(res);
  }
  if (handlers) {
    // Continue after the replay cursor captured with the control. This avoids
    // reconnecting to the already interrupted segment, so a clean stream EOF
    // now represents the terminal state of the resumed turn.
    return resumeSessionRun(binding.s, binding.r || runId, handlers, {
      synthesizeCompletion: true,
      cursor: options.cursor,
    });
  }
  return undefined;
}

export const sessionApi = {
  async generate(
    sessionId: string,
    query: string,
    handlers: StreamHandlers,
    attachments: File[] = [],
  ): Promise<StreamOutcome> {
    const target = sessionId === 'demo-1' || sessionId === 'new' ? '' : sessionId;
    let body: BodyInit;
    let headers: HeadersInit | undefined;
    if (attachments.length > 0) {
      const form = new FormData();
      form.set('sessionId', target);
      form.set('prompt', query);
      for (const file of attachments) form.append('attachments', file, file.name);
      body = form;
    } else {
      headers = { 'Content-Type': 'application/json' };
      body = JSON.stringify({ sessionId: target, prompt: query });
    }
    const res = await fetch(`/api/v1/agents/${encodeURIComponent(mainAgentID)}/chat`, {
      method: 'POST',
      headers,
      body,
    });
    if (!res.ok || !res.body) {
      throw await httpError(res);
    }
    return consumeHarnessSSE(res, handlers, target);
  },
  async edit(
    sessionId: string,
    instruction: string,
    handlers: StreamHandlers,
    attachments: File[] = [],
  ): Promise<StreamOutcome> {
    // Source multi-turn editing is another Generate on the durable session;
    // root-agent routing selects the protected edit path from its history.
    return this.generate(sessionId, instruction, handlers, attachments);
  },
  async applyProtocol(sessionId: string, protocol: unknown[], handlers: StreamHandlers): Promise<StreamOutcome> {
    return this.edit(sessionId, `应用以下 AGenUI 协议修改：${JSON.stringify(protocol)}`, handlers);
  },
  async openTemplate(sessionId: string, templateId: string, handlers: StreamHandlers): Promise<StreamOutcome> {
    return this.generate(sessionId, `基于模板 ${templateId} 创建卡片`, handlers);
  },
};

interface ControlBinding {
  s: string;
  r: string;
  q: string;
  t: string;
  contexts?: string[];
  questions?: Array<{
    id?: string;
    title?: string;
    body?: string;
    options?: Array<{
      id?: string;
      label?: string;
      description?: string;
      value?: string;
      type?: 'select' | 'input';
      placeholder?: string;
    }>;
  }>;
}

function decodeControlBinding(interruptID: string): ControlBinding | null {
  try {
    if (!interruptID.startsWith('sdk1.')) return null;
    const source = interruptID.slice('sdk1.'.length)
      .replace(/-/g, '+')
      .replace(/_/g, '/');
    const padded = source.padEnd(source.length + ((4 - source.length % 4) % 4), '=');
    const binary = atob(padded);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    const payload = JSON.parse(new TextDecoder().decode(bytes)) as Partial<ControlBinding>;
    return typeof payload.s === 'string' && payload.s &&
      typeof payload.r === 'string' && payload.r &&
      typeof payload.q === 'string' && payload.q &&
      typeof payload.t === 'string' && payload.t
      ? payload as ControlBinding
      : null;
  } catch {
    return null;
  }
}

function base64URL(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let binary = '';
  for (let index = 0; index < bytes.length; index += 1) binary += String.fromCharCode(bytes[index]);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '');
}

async function httpError(res: Response): Promise<Error> {
  try {
    const parsed = (await res.json()) as { error?: string; message?: string; error_code?: string };
    const message = parsed.error ?? parsed.message;
    if (message && parsed.error_code && !message.includes(parsed.error_code)) {
      return new Error(`${message} (${parsed.error_code})`);
    }
    return new Error(message ?? `HTTP ${res.status}`);
  } catch {
    return new Error(`HTTP ${res.status}`);
  }
}
