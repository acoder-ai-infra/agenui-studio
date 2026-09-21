export const nextStepsSchemaVersion = "agenui.next_steps.v2";
export const nextStepsToolName = "agenui_publish_next_steps";

const legacyNextStepsSchemaVersion = "agenui.next_steps.v1";
const nextStepsTitle = "下一步建议";
const legacyNextStepsTitle = "下一步";
const harnessProcessSchemaVersion = "harness.agent_chat_process.v1";
const rootAgentID = "agenui_agent";
const planIDPattern = /^next_[0-9a-f]{24}$/;
const itemIDPattern = /^next_[0-9a-f]{24}_[1-3]$/;
const legacyImportance = new Set(["required", "recommended", "optional"]);

type NextStepsSchemaVersion =
  | typeof nextStepsSchemaVersion
  | typeof legacyNextStepsSchemaVersion;

export interface NextStepItem {
  id: string;
  label: string;
  description?: string;
  prompt: string;
}

export interface NextStepsPlan {
  schemaVersion: NextStepsSchemaVersion;
  planId: string;
  items: NextStepItem[];
}

/** Supporting copy for both current plans and already-persisted v2.0 plans. */
export function nextStepSupportingText(item: NextStepItem): string {
  const description = item.description?.trim();
  if (description) return description;

  const label = item.label.trim();
  const prompt = item.prompt.trim();
  return prompt && prompt !== label ? prompt : "";
}

/**
 * Identify the semantic publisher across both its running and completed
 * process snapshots. The publisher stays out of the generic execution
 * timeline; only a validated completed result becomes user-facing UI.
 */
export function isNextStepsProcess(process: Record<string, unknown>): boolean {
  if (process.schemaVersion !== harnessProcessSchemaVersion) return false;
  if (process.category !== "tool") return false;
  if (process.agentId !== rootAgentID) return false;
  if (nonEmptyString(process.parentRunId)) return false;

  const activity = asRecord(process.activity);
  const input = asRecord(process.input);
  const output = asRecord(process.output);
  return activity.key === nextStepsToolName
    || isSupportedSchema(input.schema_version)
    || isSupportedSchema(output.schema_version);
}

/** Parse the Host-issued contract. Invalid or failed process data is inert. */
export function nextStepsFromProcess(
  process: Record<string, unknown>,
): NextStepsPlan | undefined {
  if (!isNextStepsProcess(process) || process.status !== "completed") return undefined;

  return nextStepsFromOutput(process.output) ?? nextStepsFromPresentation(process);
}

function nextStepsFromOutput(value: unknown): NextStepsPlan | undefined {
  const output = asRecord(value);
  if (!hasOnlyKeys(output, ["schema_version", "plan_id", "title", "items"])) return undefined;
  if (!isSupportedTitle(output.title)) return undefined;

  const schemaVersion = output.schema_version;
  if (schemaVersion === nextStepsSchemaVersion) return nextStepsV2FromOutput(output);
  if (schemaVersion === legacyNextStepsSchemaVersion) return nextStepsV1FromOutput(output);
  return undefined;
}

function nextStepsV2FromOutput(output: Record<string, unknown>): NextStepsPlan | undefined {
  const planId = validPlanID(output.plan_id);
  if (!planId || !validItemsArray(output.items)) return undefined;

  const items: NextStepItem[] = [];
  for (let index = 0; index < output.items.length; index += 1) {
    const source = asRecord(output.items[index]);
    if (!hasOnlyKeys(source, ["id", "label", "description", "prompt"])) return undefined;
    const item = parseItem(source, planId, index, 24, 160);
    if (!item) return undefined;
    items.push(item);
  }
  return { schemaVersion: nextStepsSchemaVersion, planId, items };
}

// Keep already-persisted v1 sessions replayable in the v2 UI. Legacy
// importance is discarded, while the user-facing description is preserved.
function nextStepsV1FromOutput(output: Record<string, unknown>): NextStepsPlan | undefined {
  const planId = validPlanID(output.plan_id);
  if (!planId || !validItemsArray(output.items)) return undefined;

  const items: NextStepItem[] = [];
  for (let index = 0; index < output.items.length; index += 1) {
    const source = asRecord(output.items[index]);
    if (!hasOnlyKeys(source, ["id", "importance", "label", "description", "prompt"])) return undefined;
    if (!legacyImportance.has(boundedString(source.importance, 11))) return undefined;
    const item = parseItem(source, planId, index, 40, 500);
    if (!item) return undefined;
    items.push(item);
  }
  return { schemaVersion: legacyNextStepsSchemaVersion, planId, items };
}

function parseItem(
  source: Record<string, unknown>,
  planId: string,
  index: number,
  maxLabelRunes: number,
  maxPromptRunes: number,
): NextStepItem | undefined {
  const id = boundedString(source.id, 31);
  const label = boundedString(source.label, maxLabelRunes);
  const description = optionalBoundedString(source.description, 160);
  const prompt = boundedString(source.prompt, maxPromptRunes);
  if (!id || !itemIDPattern.test(id) || id !== `${planId}_${index + 1}` || !label || description === null || !prompt) {
    return undefined;
  }
  return { id, label, ...(description ? { description } : {}), prompt };
}

function nextStepsFromPresentation(process: Record<string, unknown>): NextStepsPlan | undefined {
  if (!isSupportedTitle(process.title) || typeof process.summary !== "string") return undefined;
  if (process.summary.startsWith(`${nextStepsSchemaVersion}:`)) {
    return nextStepsV2FromPresentation(process);
  }
  if (process.summary.startsWith(`${legacyNextStepsSchemaVersion}:`)) {
    return nextStepsV1FromPresentation(process);
  }
  return undefined;
}

function nextStepsV2FromPresentation(process: Record<string, unknown>): NextStepsPlan | undefined {
  const planId = validPlanID(String(process.summary).slice(`${nextStepsSchemaVersion}:`.length));
  if (!planId || !validDetailsArray(process.details)) return undefined;

  const items: NextStepItem[] = [];
  for (let index = 0; index < process.details.length; index += 1) {
    const detail = asRecord(process.details[index]);
    if (!hasOnlyKeys(detail, ["label", "value"])) return undefined;
    const label = boundedString(detail.label, 24);
    const value = parseV2PresentationValue(detail.value);
    if (!label || !value) return undefined;
    items.push({
      id: `${planId}_${index + 1}`,
      label,
      ...(value.description ? { description: value.description } : {}),
      prompt: value.prompt,
    });
  }
  return { schemaVersion: nextStepsSchemaVersion, planId, items };
}

function nextStepsV1FromPresentation(process: Record<string, unknown>): NextStepsPlan | undefined {
  const planId = validPlanID(String(process.summary).slice(`${legacyNextStepsSchemaVersion}:`.length));
  if (!planId || !validDetailsArray(process.details)) return undefined;

  const items: NextStepItem[] = [];
  for (let index = 0; index < process.details.length; index += 1) {
    const detail = asRecord(process.details[index]);
    if (!hasOnlyKeys(detail, ["label", "value"])) return undefined;
    const encodedLabel = boundedString(detail.label, 52);
    const separator = encodedLabel.indexOf("|");
    if (separator <= 0 || !legacyImportance.has(encodedLabel.slice(0, separator))) return undefined;
    const label = boundedString(encodedLabel.slice(separator + 1), 40);
    if (!label || typeof detail.value !== "string") return undefined;

    let compact: Record<string, unknown>;
    try {
      compact = asRecord(JSON.parse(detail.value));
    } catch {
      return undefined;
    }
    if (!hasOnlyKeys(compact, ["d", "p"])) return undefined;
    const description = optionalBoundedString(compact.d, 160);
    if (description === null) return undefined;
    const prompt = boundedString(compact.p, 500);
    if (!prompt) return undefined;
    items.push({
      id: `${planId}_${index + 1}`,
      label,
      ...(description ? { description } : {}),
      prompt,
    });
  }
  return { schemaVersion: legacyNextStepsSchemaVersion, planId, items };
}

function parseV2PresentationValue(value: unknown): { description?: string; prompt: string } | undefined {
  if (typeof value !== "string") return undefined;

  // v2 originally stored the prompt directly. New events use the compact
  // {d,p} envelope so descriptions survive output truncation; retain both.
  try {
    const compact = asRecord(JSON.parse(value));
    const hasCompactFields = Object.prototype.hasOwnProperty.call(compact, "d")
      || Object.prototype.hasOwnProperty.call(compact, "p");
    if (hasCompactFields) {
      if (!hasOnlyKeys(compact, ["d", "p"])) return undefined;
      const description = optionalBoundedString(compact.d, 160);
      const prompt = boundedString(compact.p, 160);
      if (description === null || !prompt) return undefined;
      return { ...(description ? { description } : {}), prompt };
    }
  } catch {
    // Plain text is the persisted v2.0 presentation format.
  }

  const prompt = boundedString(value, 160);
  return prompt ? { prompt } : undefined;
}

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {};
}

function hasOnlyKeys(source: Record<string, unknown>, allowed: string[]): boolean {
  const allowedKeys = new Set(allowed);
  return Object.keys(source).every((key) => allowedKeys.has(key));
}

function isSupportedSchema(value: unknown): value is NextStepsSchemaVersion {
  return value === nextStepsSchemaVersion || value === legacyNextStepsSchemaVersion;
}

function isSupportedTitle(value: unknown): boolean {
  return value === nextStepsTitle || value === legacyNextStepsTitle;
}

function validPlanID(value: unknown): string {
  const planId = boundedString(value, 29);
  return planIDPattern.test(planId) ? planId : "";
}

function validItemsArray(value: unknown): value is unknown[] {
  return Array.isArray(value) && value.length > 0 && value.length <= 3;
}

function validDetailsArray(value: unknown): value is unknown[] {
  return Array.isArray(value) && value.length > 0 && value.length <= 3;
}

function nonEmptyString(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function boundedString(value: unknown, maxRunes: number): string {
  if (typeof value !== "string") return "";
  const normalized = value.trim();
  if (!normalized || Array.from(normalized).length > maxRunes) return "";
  return normalized;
}

function optionalBoundedString(value: unknown, maxRunes: number): string | null {
  if (value === undefined) return "";
  if (typeof value !== "string") return null;
  const normalized = value.trim();
  return Array.from(normalized).length <= maxRunes ? normalized : null;
}
