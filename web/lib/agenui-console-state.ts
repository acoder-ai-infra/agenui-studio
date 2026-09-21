export type ProgressStatus =
  | "in_progress"
  | "waiting"
  | "completed"
  | "failed"
  | "cancelled";

export interface ProgressDetail {
  label: string;
  value: string;
}

export interface ProgressActivity {
  activity_id: string;
  status: ProgressStatus;
  title: string;
  summary?: string;
  details: ProgressDetail[];
  category?: string;
  stage_id?: string;
  stage_label?: string;
  stage_order?: number;
  activity_key?: string;
  activity_label?: string;
  detail_level?: "primary" | "secondary" | "debug";
  run_id?: string;
  sequence?: number;
  output?: unknown;
  output_ref?: string;
}

export interface ProgressSubagentState {
  run_id: string;
  parent_run_id?: string;
  agent_id?: string;
  label: string;
  status: ProgressStatus;
}

export interface ProgressActivityGroup {
  activityKey: string;
  label: string;
  status: ProgressStatus;
  count: number;
  summary?: string;
  details: ProgressDetail[];
  invocations: ProgressActivity[];
}

export interface ProgressStageGroup {
  stageId: string;
  label: string;
  order: number;
  status: ProgressStatus;
  activities: ProgressActivityGroup[];
  subagents: ProgressSubagentState[];
}

export interface ProgressTimeline {
  timelineId: string;
  lastSequence: number;
  seenTransitionIds: string[];
  activities: ProgressActivity[];
  subagents: ProgressSubagentState[];
}

export type AssistantTextReplayDecision =
  | { kind: "skip" }
  | { kind: "append"; text: string }
  | { kind: "replace"; text: string };

/**
 * Reconcile a replay-from-zero Assistant text stream against text that is
 * already visible. Create a fresh guard for every physical SSE connection so
 * a second reconnect catches up against text appended by the first one.
 */
export function createAssistantTextReplayGuard(existingText: string) {
  let replayedText = "";
  let caughtUp = existingText.length === 0;

  return (delta: string): AssistantTextReplayDecision => {
    if (caughtUp) return delta ? { kind: "append", text: delta } : { kind: "skip" };

    replayedText += delta;
    if (existingText.startsWith(replayedText)) return { kind: "skip" };
    if (replayedText.startsWith(existingText)) {
      caughtUp = true;
      const suffix = replayedText.slice(existingText.length);
      return suffix ? { kind: "append", text: suffix } : { kind: "skip" };
    }

    // Persisted transcript text may have been normalized differently. Replace
    // it with the replayed source instead of appending a duplicate copy.
    caughtUp = true;
    return { kind: "replace", text: replayedText };
  };
}

export interface ConsoleFrame {
  schema_version: string;
  sequence: number;
  run_id?: string;
  session_id?: string;
  event_type: string;
  view_type: string;
  payload?: Record<string, unknown>;
  artifact_ref?: string;
  error?: { code: string; type: string; message?: string };
}

export interface HarnessProcessView {
  schemaVersion?: string;
  eventId?: string;
  eventType?: string;
  processId?: string;
  runId?: string;
  parentRunId?: string;
  agentId?: string;
  category?: string;
  status?: string;
  title?: string;
  label?: string;
  summary?: string;
  description?: string;
  action?: string;
  input?: unknown;
  output?: unknown;
  outputRef?: string;
  details?: Array<{ label?: string; value?: string }>;
  stage?: { id?: string; label?: string; order?: number };
  activity?: { key?: string; label?: string; detailLevel?: string };
}

/**
 * Convert the public Harness process projection into the console view model.
 * This intentionally consumes only the stable public projection. It never
 * forwards hook payloads, tool arguments or tool results to the workbench.
 */
export function projectHarnessProcess(
  source: HarnessProcessView,
  fallbackRunID = "",
): ConsoleFrame | undefined {
  if (source.schemaVersion && source.schemaVersion !== "harness.agent_chat_process.v1") {
    return undefined;
  }
  const runID = String(source.runId ?? fallbackRunID).trim();
  const processID = String(source.processId ?? "").trim();
  const eventID = String(source.eventId ?? "").trim();
  const category = String(source.category ?? "").trim();
  const status = String(source.status ?? "running").trim();
  const rawTitle = String(source.title ?? source.label ?? category ?? "").trim();
  if (!rawTitle && !processID && !eventID) return undefined;

  // Workspace is one tool with several meaningful public actions. Preserve a
  // published action without exposing the rest of the input object.
  const action = workspaceAction(source, rawTitle);
  // A projection without any title is malformed. Fall back to the public
  // identity instead of inventing copy: this layer has no locale, and an
  // identifier is more diagnosable than a generic placeholder. The guard above
  // guarantees one of these is present.
  const title = action ? `${rawTitle} · ${action}` : rawTitle || processID || eventID;
  const payload: Record<string, unknown> = {
    step: title,
    detail: String(source.summary ?? source.description ?? ""),
    category,
    process_id: processID,
    parent_run_id: String(source.parentRunId ?? ""),
  };
  if (action) payload.action = action;
  if (!processID) {
    // Do not guess an identity: keep enough public identity for an operator to
    // diagnose the malformed projection while avoiding raw event payloads.
    payload.projection_warning = `Harness process is missing processId${eventID ? ` (eventId: ${eventID})` : ""}`;
  }
  const eventType = status === "completed"
    ? "runtime_step_completed"
    : status === "failed" || status === "cancelled" || status === "expired"
      ? "runtime_step_failed"
      : "runtime_step_started";
  return {
    schema_version: "agenui.console.projection.v1",
    sequence: 0,
    event_type: eventType,
    view_type: "agenui",
    ...(runID ? { run_id: runID } : {}),
    payload,
  };
}

/** Collapse transcript snapshots to one latest row per (runId, processId). */
export function projectHarnessProcesses(
  processByRun: Record<string, HarnessProcessView[]>,
): ConsoleFrame[] {
  const latest = new Map<string, ConsoleFrame>();
  const order: string[] = [];
  let anonymous = 0;
  for (const [fallbackRunID, processes] of Object.entries(processByRun)) {
    for (const process of processes ?? []) {
      const frame = projectHarnessProcess(process, fallbackRunID);
      if (!frame) continue;
      const processID = String(frame.payload?.process_id ?? "");
      const key = processID
        ? `${frame.run_id ?? fallbackRunID}\u0000${processID}`
        : `anonymous\u0000${anonymous++}`;
      if (!latest.has(key)) order.push(key);
      const previous = latest.get(key);
      if (previous) {
        const previousDetail = String(previous.payload?.detail ?? "");
        const nextDetail = String(frame.payload?.detail ?? "");
        const previousAction = String(previous.payload?.action ?? "");
        const nextAction = String(frame.payload?.action ?? "");
        frame.payload = {
          ...previous.payload,
          ...frame.payload,
          detail: nextDetail || previousDetail,
          action: nextAction || previousAction,
          step: nextAction || !previousAction
            ? frame.payload?.step
            : previous.payload?.step,
        };
      }
      latest.set(key, frame);
    }
  }
  return order.map((key) => latest.get(key)!);
}

function workspaceAction(source: HarnessProcessView, title: string): string {
  if (title !== "agenui_workspace") return "";
  const direct = String(source.action ?? "").trim();
  if (direct) return direct;
  if (!isRecord(source.input)) return "";
  return String(source.input.action ?? "").trim();
}

interface ProgressFrame {
  kind?: string;
  schema_version?: string;
  timeline_id?: string;
  last_sequence?: number;
  transition_id?: string;
  sequence?: number;
  activities?: ProgressActivity[];
  updates?: ProgressActivity[];
}

const progressSchema = "agenui.progress_activity.v1";
const validStatuses = new Set<ProgressStatus>([
  "in_progress",
  "waiting",
  "completed",
  "failed",
  "cancelled",
]);

/**
 * Merge one native Harness data-process part into the local React view state.
 * This is an in-memory UI reducer, not a transport projection or durable event.
 */
export function applyHarnessProcess(
  current: ProgressTimeline | undefined,
  process: Record<string, unknown>,
): ProgressTimeline | undefined {
  if (process.schemaVersion !== "harness.agent_chat_process.v1") return current;
  const activityID = String(process.groupKey ?? process.processId ?? process.eventId ?? "").trim();
  const title = String(process.title ?? process.label ?? "").trim();
  const status = harnessProcessStatus(process.status);
  if (!activityID || !title || !status) return current;

  const eventID = String(process.eventId ?? "").trim();
  const sequence = numericSequence(process.sequence);
  const category = String(process.category ?? "").trim();
  const eventType = String(process.eventType ?? "").trim();
  const runID = String(process.runId ?? "").trim();
  const parentRunID = String(process.parentRunId ?? "").trim();
  // A child-run projection deliberately keeps one stable eventId for its whole
  // lifecycle (running -> completed/failed). Deduplicating on eventId alone
  // drops the terminal update and leaves the sub-agent spinner active forever.
  const transitionID = eventID
    ? `${eventID}\u0000${status}\u0000${sequence ?? ""}`
    : "";
  if (transitionID && current?.seenTransitionIds.includes(transitionID)) return current;
  const nextSequence = sequence ?? current?.lastSequence ?? 0;
  const seenTransitionIds = transitionID
    ? [...(current?.seenTransitionIds ?? []), transitionID].slice(-500)
    : current?.seenTransitionIds ?? [];

  // Child Run lifecycle is already part of the public SSE. Keep it as an
  // invisible state anchor; tools carrying the same runId remain the only
  // rendered child rows.
  const childRunLifecycle = category === "subagent"
    && eventType === "sub_agent_run"
    && Boolean(runID && parentRunID);
  if (childRunLifecycle) {
    const previousSubagents = current?.subagents ?? [];
    const index = previousSubagents.findIndex((subagent) => subagent.run_id === runID);
    const previousStatus = index >= 0 ? previousSubagents[index].status : undefined;
    if (isTerminalProgressStatus(previousStatus) && !isTerminalProgressStatus(status)) {
      return current;
    }
    const nextSubagent: ProgressSubagentState = {
      run_id: runID,
      parent_run_id: parentRunID,
      agent_id: String(process.agentId ?? "").trim() || undefined,
      label: title,
      status,
    };
    const subagents = [...previousSubagents];
    if (index >= 0) subagents[index] = nextSubagent;
    else subagents.push(nextSubagent);
    return {
      timelineId: current?.timelineId ?? `harness:${parentRunID}`,
      lastSequence: Math.max(current?.lastSequence ?? 0, nextSequence),
      seenTransitionIds,
      activities: current?.activities ?? [],
      subagents,
    };
  }

  const details = Array.isArray(process.details)
    ? process.details.flatMap((detail) => {
        if (!detail || typeof detail !== "object") return [];
        const item = detail as Record<string, unknown>;
        return typeof item.label === "string" && typeof item.value === "string" && item.label && item.value
          ? [{ label: item.label, value: item.value }]
          : [];
      }).slice(0, 12)
    : [];
  const summary = typeof process.summary === "string" ? process.summary.trim() : "";
  const hasOutput = Object.prototype.hasOwnProperty.call(process, "output")
    && process.output !== undefined
    && process.output !== null;
  const outputRef = String(process.outputRef ?? "").trim();
  const presentation = harnessProcessPresentation(process, title, activityID);
  const previousActivities = current?.activities ?? [];
  const index = previousActivities.findIndex((activity) => activity.activity_id === activityID);
  const previousStatus = index >= 0 ? previousActivities[index].status : undefined;
  if (isTerminalProgressStatus(previousStatus) && !isTerminalProgressStatus(status)) {
    // Transcript polling can replay an older running snapshot after the live
    // terminal update. Terminal child-run state must remain monotonic.
    return current;
  }
  const nextActivity: ProgressActivity = {
    activity_id: activityID,
    status,
    title,
    category,
    stage_id: presentation.stageId,
    stage_label: presentation.stageLabel,
    stage_order: presentation.stageOrder,
    activity_key: presentation.activityKey,
    activity_label: presentation.activityLabel,
    detail_level: presentation.detailLevel,
    ...(runID ? { run_id: runID } : index >= 0 && previousActivities[index].run_id
      ? { run_id: previousActivities[index].run_id }
      : {}),
    ...(sequence !== undefined ? { sequence } : index >= 0 && previousActivities[index].sequence !== undefined
      ? { sequence: previousActivities[index].sequence }
      : {}),
    ...(summary || index >= 0 && previousActivities[index].summary
      ? { summary: summary || previousActivities[index].summary }
      : {}),
    ...(hasOutput || index >= 0 && previousActivities[index].output !== undefined
      ? { output: hasOutput ? process.output : previousActivities[index].output }
      : {}),
    ...(outputRef || index >= 0 && previousActivities[index].output_ref
      ? { output_ref: outputRef || previousActivities[index].output_ref }
      : {}),
    details: details.length > 0 || index < 0 ? details : previousActivities[index].details,
  };
  const activities = previousActivities.map((activity) => ({ ...activity, details: [...activity.details] }));
  if (index >= 0) activities[index] = nextActivity;
  else activities.push(nextActivity);

  return {
    timelineId: current?.timelineId ?? `harness:${runID || "conversation"}`,
    lastSequence: Math.max(current?.lastSequence ?? 0, nextSequence),
    seenTransitionIds,
    activities,
    subagents: current?.subagents ?? [],
  };
}

export interface FormattedProgressOutput {
  text: string;
  truncated: boolean;
}

/**
 * Format a public, server-authorized tool result for the process disclosure.
 * JSON strings are decoded so MCP and function tools share one presentation;
 * the browser still bounds the rendered text even if a deployment raises its
 * SSE preview limit.
 */
export function formatProgressOutput(
  value: unknown,
  maxChars = 6000,
): FormattedProgressOutput | undefined {
  let normalized = value;
  for (let attempt = 0; attempt < 2 && typeof normalized === "string"; attempt += 1) {
    const candidate = normalized.trim();
    if (!candidate) return undefined;
    if (!candidate.startsWith("{") && !candidate.startsWith("[")) break;
    try {
      normalized = JSON.parse(candidate);
    } catch {
      const compact = compactTruncatedSearchOutput(candidate);
      if (compact) normalized = compact;
      break;
    }
  }
  normalized = compactProgressOutput(normalized);

  let text: string;
  if (typeof normalized === "string") {
    text = normalized.trim();
  } else {
    try {
      text = JSON.stringify(normalized, null, 2);
    } catch {
      text = String(normalized ?? "").trim();
    }
  }
  if (!text) return undefined;

  const limit = Number.isFinite(maxChars) && maxChars > 0 ? Math.floor(maxChars) : 6000;
  if (text.length <= limit) return { text, truncated: false };
  return { text: `${text.slice(0, limit).trimEnd()}\n…`, truncated: true };
}

const compactResultItemKeys = [
  "name", "title", "id", "data_source_id", "operator_id", "operator_key",
  "method", "path", "summary", "description", "output", "status", "ok", "replayed",
] as const;

/** Keep the useful result identity while omitting large schemas/examples. */
function compactProgressOutput(value: unknown): unknown {
  if (!isRecord(value)) return value;
  if (Array.isArray(value.results)) {
    return {
      ...(typeof value.query === "string" ? { query: value.query } : {}),
      total: typeof value.total === "number" ? value.total : value.results.length,
      results: value.results.slice(0, 10).map(compactProgressResultItem),
    };
  }
  if (Array.isArray(value.items) && value.items.every(isRecord)) {
    return {
      total: typeof value.total === "number" ? value.total : value.items.length,
      items: value.items.slice(0, 10).map(compactProgressResultItem),
    };
  }
  if (typeof value.ok === "boolean" && isRecord(value.execution)) {
    return { ok: value.ok, execution: compactProgressResultItem(value.execution) };
  }
  return value;
}

function compactProgressResultItem(value: unknown): unknown {
  if (!isRecord(value)) return value;
  const compact: Record<string, unknown> = {};
  for (const key of compactResultItemKeys) {
    const item = value[key];
    if (item !== undefined && item !== null && item !== "") compact[key] = item;
  }
  return Object.keys(compact).length > 0 ? compact : value;
}

/** Recover the useful identities from a bounded JSON preview cut mid-result. */
function compactTruncatedSearchOutput(value: string): Record<string, unknown> | undefined {
  if (!value.includes('"results":[') || !value.includes('"data_source_id":')) return undefined;
  const matches = Array.from(value.matchAll(/"data_source_id":"((?:\\.|[^"\\])*)"/g));
  if (matches.length === 0) return undefined;
  const query = extractJSONStringField(value, "query");
  const results = matches.slice(0, 10).map((match, index) => {
    const start = match.index ?? 0;
    const end = matches[index + 1]?.index ?? value.length;
    const segment = value.slice(start, end);
    return compactProgressResultItem({
      data_source_id: decodeJSONStringFragment(match[1]),
      method: extractJSONStringField(segment, "method"),
      path: extractJSONStringField(segment, "path"),
      description: extractJSONStringField(segment, "description"),
    });
  });
  return { ...(query ? { query } : {}), total: results.length, results };
}

function extractJSONStringField(value: string, key: string): string | undefined {
  const match = value.match(new RegExp(`"${key}":"((?:\\\\.|[^"\\\\])*)"`));
  return match ? decodeJSONStringFragment(match[1]) : undefined;
}

function decodeJSONStringFragment(value: string): string {
  try {
    return JSON.parse(`"${value}"`) as string;
  } catch {
    return value;
  }
}

function harnessProcessPresentation(
  process: Record<string, unknown>,
  title: string,
  activityID: string,
): {
  stageId: string;
  stageLabel: string;
  stageOrder: number;
  activityKey: string;
  activityLabel: string;
  detailLevel: "primary" | "secondary" | "debug";
} {
  const stage = isRecord(process.stage) ? process.stage : {};
  const activity = isRecord(process.activity) ? process.activity : {};
  const category = String(process.category ?? "").trim();
  // Identities only. A frame without a stage is still groupable, and the
  // console renders the caption for these ids; this layer has no locale.
  const fallbackStage = category === "reasoning"
    ? { id: "reasoning", label: "", order: 90 }
    : { id: "execution", label: "", order: 500 };
  const rawDetailLevel = String(activity.detailLevel ?? "").trim();
  const detailLevel = rawDetailLevel === "primary" || rawDetailLevel === "debug"
    ? rawDetailLevel
    : category === "subagent" && !activity.key
      ? "debug"
      : "secondary";
  return {
    stageId: String(stage.id ?? fallbackStage.id).trim() || fallbackStage.id,
    stageLabel: String(stage.label ?? fallbackStage.label).trim() || fallbackStage.label,
    stageOrder: typeof stage.order === "number" && Number.isFinite(stage.order)
      ? stage.order
      : fallbackStage.order,
    activityKey: String(activity.key ?? (category === "reasoning" ? "answer_summary" : title || activityID)).trim() || activityID,
    activityLabel: String(activity.label ?? title).trim(),
    detailLevel,
  };
}

export function groupProgressActivities(
  activities: ProgressActivity[],
  subagents: ProgressSubagentState[] = [],
): ProgressStageGroup[] {
  const stages = new Map<string, ProgressStageGroup>();
  const order: string[] = [];
  const runStage = new Map<string, { stageId: string; sequence: number; position: number }>();
  const ensureStage = (stageId: string, label: string, stageOrder: number): ProgressStageGroup => {
    let stage = stages.get(stageId);
    if (!stage) {
      stage = {
        stageId,
        label,
        order: stageOrder,
        status: "completed",
        activities: [],
        subagents: [],
      };
      stages.set(stageId, stage);
      order.push(stageId);
    }
    return stage;
  };
  for (let position = 0; position < activities.length; position += 1) {
    const invocation = activities[position];
    if (invocation.detail_level === "debug") continue;
    const stageId = invocation.stage_id || "execution";
    const stage = ensureStage(stageId, invocation.stage_label ?? "", invocation.stage_order ?? 500);
    if (invocation.category === "tool" && invocation.run_id) {
      const candidate = { stageId, sequence: invocation.sequence ?? -1, position };
      const previous = runStage.get(invocation.run_id);
      if (!previous
        || candidate.sequence > previous.sequence
        || candidate.sequence === previous.sequence && candidate.position > previous.position) {
        runStage.set(invocation.run_id, candidate);
      }
    }
    const activityKey = invocation.activity_key || invocation.activity_id;
    let activity = stage.activities.find((candidate) => candidate.activityKey === activityKey);
    if (!activity) {
      activity = {
        activityKey,
        label: invocation.activity_label || invocation.title,
        status: invocation.status,
        count: 0,
        details: [],
        invocations: [],
      };
      stage.activities.push(activity);
    }
    activity.invocations.push(invocation);
    activity.count = activity.invocations.length;
    activity.status = aggregateProgressStatus(activity.invocations.map((item) => item.status));
    if (invocation.summary) activity.summary = invocation.summary;
    if (invocation.details.length > 0) activity.details = invocation.details;
  }
  for (const subagent of subagents) {
    const stageId = runStage.get(subagent.run_id)?.stageId ?? "execution";
    const stage = ensureStage(stageId, "", 500);
    stage.subagents.push(subagent);
  }
  for (const stageId of order) {
    const stage = stages.get(stageId)!;
    stage.status = aggregateStageStatus(stage);
  }
  return order
    .map((stageId, firstSeen) => ({ stage: stages.get(stageId)!, firstSeen }))
    .sort((left, right) => left.stage.order - right.stage.order || left.firstSeen - right.firstSeen)
    .map(({ stage }) => stage);
}

/**
 * Keep the latest visible Agent stage active while its root Run is still
 * alive, including the quiet gap after one tool completes and before the next
 * tool starts. Native tool/sub-agent lifecycle always takes precedence.
 */
export function inferRunningProgressStage(
  stages: ProgressStageGroup[],
  rootRunActive: boolean,
): string | undefined {
  if (!rootRunActive || stages.length === 0) return undefined;
  if (stages.some((stage) => stage.status === "in_progress" || stage.status === "waiting")) {
    return undefined;
  }
  const latest = stages[stages.length - 1];
  return latest.status === "completed" ? latest.stageId : undefined;
}

function aggregateProgressStatus(statuses: ProgressStatus[]): ProgressStatus {
  if (statuses.includes("in_progress")) return "in_progress";
  if (statuses.includes("waiting")) return "waiting";
  if (statuses.includes("failed")) return "failed";
  if (statuses.includes("cancelled")) return "cancelled";
  return "completed";
}

function aggregateStageStatus(stage: ProgressStageGroup): ProgressStatus {
  const activityStatuses = stage.activities.map((activity) => activity.status);
  const subagentStatuses = stage.subagents.map((subagent) => subagent.status);
  const activeStatus = aggregateProgressStatus([...activityStatuses, ...subagentStatuses]);
  if (activeStatus === "in_progress" || activeStatus === "waiting") return activeStatus;
  if (subagentStatuses.length > 0) {
    if (subagentStatuses.includes("failed")) return "failed";
    if (subagentStatuses.includes("cancelled")) return "cancelled";
    return "completed";
  }
  return aggregateProgressStatus(activityStatuses);
}

function isTerminalProgressStatus(status: ProgressStatus | undefined): boolean {
  return status === "completed" || status === "failed" || status === "cancelled";
}

function harnessProcessStatus(value: unknown): ProgressStatus | undefined {
  switch (String(value ?? "")) {
    case "running": return "in_progress";
    case "completed": return "completed";
    case "failed": return "failed";
    case "cancelled": return "cancelled";
    case "expired": return "cancelled";
    case "waiting": return "waiting";
    default: return undefined;
  }
}

/**
 * Apply a source AGenUI progress_init/progress_activity frame. Invalid or replayed
 * frames are ignored so a UI refresh cannot roll a newer timeline backwards.
 */
export function applyProgressFrame(
  current: ProgressTimeline | undefined,
  frame: ProgressFrame,
): ProgressTimeline | undefined {
  if (frame.schema_version !== progressSchema || !frame.timeline_id) {
    return current;
  }
  if (frame.kind === "progress_init") {
    const sequence = numericSequence(frame.last_sequence);
    const activities = validateActivities(frame.activities);
    if (sequence === undefined || activities === undefined) return current;
    if (current?.timelineId === frame.timeline_id && sequence <= current.lastSequence) return current;
    return {
      timelineId: frame.timeline_id,
      lastSequence: sequence,
      seenTransitionIds: [],
      activities,
      subagents: current?.subagents ?? [],
    };
  }
  if (frame.kind !== "progress_activity" || !current || current.timelineId !== frame.timeline_id) {
    return current;
  }
  const sequence = numericSequence(frame.sequence);
  const transitionID = String(frame.transition_id ?? "");
  const updates = validateActivities(frame.updates);
  if (sequence === undefined || !transitionID || updates === undefined) return current;
  if (sequence <= current.lastSequence || current.seenTransitionIds.includes(transitionID)) return current;
  if (sequence !== current.lastSequence + 1) return current;

  const next = current.activities.map((activity) => ({ ...activity, details: [...activity.details] }));
  for (const update of updates) {
    const index = next.findIndex((activity) => activity.activity_id === update.activity_id);
    if (index >= 0) next[index] = update;
    else next.push(update);
  }
  if (next.filter((activity) => activity.status === "in_progress" || activity.status === "waiting").length > 1) {
    return current;
  }
  return {
    timelineId: current.timelineId,
    lastSequence: sequence,
    seenTransitionIds: [...current.seenTransitionIds, transitionID],
    activities: next,
    subagents: current.subagents ?? [],
  };
}

// Source-compatible terminal cleanup: completed/failed streams must not leave
// an active spinner; an interrupt converts the active row to waiting.
export function finalizeProgressTimeline(
  current: ProgressTimeline | undefined,
  terminal: "completed" | "failed" | "cancelled" | "interrupted",
): ProgressTimeline | undefined {
  if (!current) return current;
  const nextStatus: ProgressStatus = terminal === "interrupted" ? "waiting" : terminal;
  let changed = false;
  const activities = current.activities.map((activity) => {
    if (activity.status !== "in_progress" && activity.status !== "waiting") return activity;
    if (terminal === "interrupted" && activity.status === "waiting") return activity;
    changed = true;
    return { ...activity, status: nextStatus };
  });
  const subagents = (current.subagents ?? []).map((subagent) => {
    if (subagent.status !== "in_progress" && subagent.status !== "waiting") return subagent;
    if (terminal === "interrupted" && subagent.status === "waiting") return subagent;
    changed = true;
    return { ...subagent, status: nextStatus };
  });
  return changed ? { ...current, activities, subagents } : current;
}

function numericSequence(value: unknown): number | undefined {
  return typeof value === "number" && Number.isInteger(value) && value >= 0 ? value : undefined;
}

function validateActivities(value: unknown): ProgressActivity[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const result: ProgressActivity[] = [];
  for (const item of value) {
    if (!item || typeof item !== "object") return undefined;
    const raw = item as Record<string, unknown>;
    const status = String(raw.status ?? "") as ProgressStatus;
    if (!raw.activity_id || !validStatuses.has(status) || typeof raw.title !== "string") return undefined;
    const details = Array.isArray(raw.details)
      ? raw.details.flatMap((detail) => {
          if (!detail || typeof detail !== "object") return [];
          const value = detail as Record<string, unknown>;
          return typeof value.label === "string" && typeof value.value === "string"
            ? [{ label: value.label, value: value.value }]
            : [];
        }).slice(0, 3)
      : [];
    result.push({
      activity_id: String(raw.activity_id),
      status,
      title: raw.title,
      ...(typeof raw.summary === "string" && raw.summary ? { summary: raw.summary } : {}),
      ...(typeof raw.category === "string" ? { category: raw.category } : {}),
      ...(typeof raw.stage_id === "string" ? { stage_id: raw.stage_id } : {}),
      ...(typeof raw.stage_label === "string" ? { stage_label: raw.stage_label } : {}),
      ...(typeof raw.stage_order === "number" && Number.isFinite(raw.stage_order) ? { stage_order: raw.stage_order } : {}),
      ...(typeof raw.activity_key === "string" ? { activity_key: raw.activity_key } : {}),
      ...(typeof raw.activity_label === "string" ? { activity_label: raw.activity_label } : {}),
      ...(raw.detail_level === "primary" || raw.detail_level === "secondary" || raw.detail_level === "debug"
        ? { detail_level: raw.detail_level }
        : {}),
      ...(typeof raw.run_id === "string" ? { run_id: raw.run_id } : {}),
      ...(typeof raw.sequence === "number" && Number.isInteger(raw.sequence) && raw.sequence >= 0
        ? { sequence: raw.sequence }
        : {}),
      details,
    });
  }
  return result;
}

interface ConversationEnvelope {
  code?: number;
  data?: {
    messages?: Array<{
      role?: string;
      content?: string;
      intentText?: string;
      progress_init?: ProgressFrame;
    }>;
    metadata?: Record<string, unknown>;
  };
}

/** Project the durable conversation detail into the same reducer input as live SSE. */
export function projectConversationHistory(envelope: ConversationEnvelope): ConsoleFrame[] {
  if (envelope.code !== 1) return [];
  const frames: ConsoleFrame[] = [];
  let sequence = 0;
  for (const message of envelope.data?.messages ?? []) {
    if (message.role === "user") {
      const text = String(message.content ?? "").trim();
      if (text) frames.push(viewFrame(++sequence, "user_message_received", { text }));
      continue;
    }
    if (message.role !== "assistant") continue;
    const intent = String(message.intentText ?? "").trim();
    const content = String(message.content ?? "").trim();
    const prose = intent || content;
    if (prose) frames.push(viewFrame(++sequence, "agent_text_delta", { text: prose }));
    if (message.progress_init) {
      frames.push(viewFrame(++sequence, "progress_init", message.progress_init as Record<string, unknown>));
    }
  }
  const metadata = envelope.data?.metadata ?? {};
  if (frames.every((frame) => frame.event_type !== "progress_init") && isRecord(metadata.progress_init)) {
    frames.push(viewFrame(++sequence, "progress_init", metadata.progress_init));
  }
  return frames;
}

function viewFrame(sequence: number, eventType: string, payload: Record<string, unknown>): ConsoleFrame {
  return {
    schema_version: "agenui.console.projection.v1",
    sequence,
    event_type: eventType,
    view_type: "agenui",
    payload,
  };
}

export interface TranscriptControl {
  requestId?: string;
  createdAt?: string;
  status?: string;
  answerText?: string;
  answers?: Array<{
    questionId?: string;
    text?: string;
  }>;
  questions?: Array<{
    header?: string;
    question?: string;
    options?: Array<{ label?: string; description?: string }>;
  }>;
}

/** Restore answered HITL facts exposed by the source Harness transcript. */
export function projectAnsweredControls(
  controlsByRun: Record<string, TranscriptControl[]>,
): ConsoleFrame[] {
  const frames: ConsoleFrame[] = [];
  let sequence = 0;
  for (const runID of Object.keys(controlsByRun).sort()) {
    for (const control of controlsByRun[runID] ?? []) {
      const allQuestions = normalizeQuestions((control.questions ?? []).filter(isRecord));
      const structuredAnswers = (control.answers ?? []).flatMap((answer) => {
        const questionID = String(answer.questionId ?? "").trim();
        const text = String(answer.text ?? "").trim();
        const questionIndex = allQuestions.findIndex((question) => question.id === questionID);
        return questionID && text && questionIndex >= 0
          ? [{ question: allQuestions[questionIndex], text }]
          : [];
      });
      const answeredQuestions = structuredAnswers.length > 0
        ? structuredAnswers.map((answer) => answer.question)
        : allQuestions.slice(0, 1);
      const first = answeredQuestions[0];
      const answer = structuredAnswers.length > 0
        ? structuredAnswers.map(({ question, text }) => (
            question.title ? `${question.title}：${text}` : text
          )).join("\n")
        : String(control.answerText ?? "").trim();
      const resumeFailed = control.status === "resume_failed";
      if ((control.status !== "answered" && !resumeFailed) || !answer || !first?.body) continue;
      frames.push({
        ...viewFrame(++sequence, "control_request_created", {
          control_id: String(control.requestId ?? ""),
          question_id: first.id,
          question: first.body,
          options: first.options.map(({ id, label }) => ({ id, label })),
          questions: answeredQuestions.map(publicControlQuestion),
          answered: true,
          ...(resumeFailed ? { resume_failed: true } : {}),
        }),
        run_id: runID,
      });
      frames.push({
        ...viewFrame(++sequence, "user_message_received", { text: answer }),
        run_id: runID,
      });
      if (resumeFailed) {
        frames.push({
          ...viewFrame(++sequence, "run_resume_failed", {
            error: "RESUME_FAILED",
          }),
          run_id: runID,
        });
      }
    }
  }
  return frames;
}

export interface NativeEvent {
  event_id?: string;
  sequence?: number;
  run_id?: string;
  session_id?: string;
  event_type?: string;
  payload?: Record<string, unknown>;
  payload_preview?: Record<string, unknown>;
}

/** Rebuild the latest still-pending ask_user card from native Harness facts. */
export function projectPendingControl(
  sessionID: string,
  runID: string,
  events: NativeEvent[],
): ConsoleFrame | undefined {
  let pending: NativeEvent | undefined;
  for (const event of events) {
    if (event.event_type === "control_request_created") pending = event;
    if (
      pending &&
      (event.event_type === "control_response_received" ||
        event.event_type === "run_completed" ||
        event.event_type === "run_failed" ||
        event.event_type === "run_cancelled")
    ) {
      pending = undefined;
    }
  }
  if (!pending) return undefined;
  const payload = pending.payload ?? pending.payload_preview ?? {};
  const contexts = Array.isArray(payload.interrupt_contexts) ? payload.interrupt_contexts : [];
  const questions = contexts.flatMap((context) => {
    if (!isRecord(context) || !isRecord(context.info) || !Array.isArray(context.info.questions)) return [];
    return context.info.questions.filter(isRecord);
  });
  const normalized = normalizeQuestions(questions);
  const requestID = String(payload.request_id ?? "");
  const checkpointID = String(payload.checkpoint_id ?? "");
  const ticket = String(payload.control_ticket ?? "");
  const contextIDs = contexts.flatMap((context) => isRecord(context) && context.id ? [String(context.id)] : []);
  if (!requestID || !checkpointID || !ticket || contextIDs.length === 0 || normalized.length === 0) return undefined;
  const binding = {
    v: 1,
    s: sessionID,
    r: runID,
    q: requestID,
    c: checkpointID,
    t: ticket,
    contexts: contextIDs,
    questions: normalized,
  };
  const first = normalized[0];
  return {
    schema_version: "agenui.console.projection.v1",
    sequence: Number(pending.sequence ?? 0),
    run_id: runID,
    session_id: sessionID,
    event_type: "control_request_created",
    view_type: "agenui",
    payload: {
      control_id: `sdk1.${base64URL(JSON.stringify(binding))}`,
      question_id: first.id,
      question: first.body || String(payload.preamble ?? payload.prompt ?? first.title ?? ""),
      options: first.options.map(({ id, label }) => ({ id, label })),
      questions: normalized.map(publicControlQuestion),
    },
  };
}

interface NormalizedQuestion {
  id: string;
  title?: string;
  body: string;
  options: Array<{
    id: string;
    label: string;
    description?: string;
    value?: string;
    type: "select" | "input";
    placeholder?: string;
  }>;
}

function publicControlQuestion(question: NormalizedQuestion): Record<string, unknown> {
  return {
    id: question.id,
    header: question.title,
    question: question.body,
    options: question.options.map((option) => ({
      id: option.id,
      label: option.label,
      description: option.description,
      type: option.type,
      placeholder: option.placeholder,
    })),
  };
}

function normalizeQuestions(source: Record<string, unknown>[]): NormalizedQuestion[] {
  return source.flatMap((question, questionIndex) => {
    const body = String(question.question ?? "").trim();
    if (!body) return [];
    const options: NormalizedQuestion["options"] = (Array.isArray(question.options) ? question.options : [])
      .filter(isRecord)
      .flatMap((option, optionIndex) => {
        const [display, machine] = splitOption(String(option.label ?? option.value ?? ""));
        if (!display || ["其他", "其它", "other", "自定义"].includes(display.toLowerCase())) return [];
        return [{
          id: `q${questionIndex}_opt${optionIndex}`,
          label: display,
          description: String(option.description ?? "").trim() || undefined,
          value: machine,
          type: "select" as const,
        }];
      });
    // A backend option always carries a readable display (see the guard above),
    // so an empty label marks the free-text option this projection adds. The
    // console owns its wording; this layer stays free of user-facing copy.
    options.push({
      id: `q${questionIndex}_other`,
      label: "",
      type: "input",
    });
    return [{
      id: `q${questionIndex}`,
      title: String(question.header ?? "").trim(),
      body,
      options,
    }];
  });
}

function splitOption(value: string): [string, string] {
  const [display, machine] = value.split("|||", 2).map((part) => part.trim());
  return [display, machine || display];
}

/**
 * Extract the stable error code from a Harness run failure text.
 *
 * Harness renders one sentence in its own locale and appends the code in
 * brackets (`agentChatFailureText`). The code is the only half that is a
 * contract; the sentence is server copy. Returning just the code lets the
 * console render the failure in the active locale while this layer stays free
 * of user-facing copy. Returns undefined when the text carries no code, so the
 * caller shows the original text rather than inventing a replacement.
 */
export function runFailureCode(text: string): string | undefined {
  const normalized = text.trim();
  if (/^[A-Z][A-Z0-9_]+$/.test(normalized)) return normalized;
  const match = /[（(]([A-Za-z0-9_]+)[)）]\s*$/.exec(normalized);
  return match ? match[1] : undefined;
}

function base64URL(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let binary = "";
  for (let i = 0; i < bytes.length; i += 1) binary += String.fromCharCode(bytes[i]);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value && typeof value === "object" && !Array.isArray(value));
}
