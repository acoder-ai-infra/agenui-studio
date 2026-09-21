export interface DataBindingInsight {
  componentId: string;
  property: string;
  path: string;
}

export interface ActionInsight {
  componentId: string;
  componentType: string;
  name: string;
}

export interface RuntimeBindingInsight {
  refKey: string;
  sourceKey: string;
  sourceId: string;
  operatorRef?: string;
  transforms: string[];
}

export interface DataSourceInsight {
  id: string;
  name: string;
  path?: string;
}

export interface OperatorInsight {
  id: string;
  label: string;
}

export interface ImplementationArtifacts {
  bindings?: string;
  apis?: string;
  generationID?: string;
  revision?: number;
}

export interface ImplementationInsight {
  version: string;
  surfaceIds: string[];
  componentCount: number;
  dataFields: string[];
  bindings: DataBindingInsight[];
  actions: ActionInsight[];
  runtimeBindings: RuntimeBindingInsight[];
  dataSources: DataSourceInsight[];
  operators: OperatorInsight[];
  previewReady: boolean;
  bindingPlanPresent: boolean;
  bindingComplete: boolean;
  bundleReady: boolean;
}

export interface RuntimePackage {
  schemaVersion: 'agenui.runtime-package/v1';
  manifest: {
    generationID?: string;
    revision?: number;
    invocation: {
      accepts: 'application/json';
      inputSchema: { type: 'object'; additionalProperties: true };
    };
    pipeline: readonly ['dataSources', 'operators', 'bindings', 'dsl'];
  };
  dsl: Record<string, unknown>[];
  bindings: unknown;
  dataSources: unknown;
  operators: OperatorInsight[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function decodeArtifact(value: string | undefined): unknown {
  let decoded: unknown = value?.trim() ?? '';
  for (let attempt = 0; attempt < 4 && typeof decoded === 'string' && decoded.trim(); attempt += 1) {
    try {
      decoded = JSON.parse(decoded);
    } catch {
      break;
    }
  }
  return decoded;
}

function apiRecords(value: unknown): Record<string, unknown>[] {
  if (Array.isArray(value)) return value.filter(isRecord);
  if (!isRecord(value)) return [];
  for (const key of ['apis', 'results', 'dataSources']) {
    if (Array.isArray(value[key])) return value[key].filter(isRecord);
  }
  return Object.keys(value).some((key) => ['name', 'id', 'path', 'api_name'].includes(key)) ? [value] : [];
}

function sourceName(source: Record<string, unknown>, index: number): DataSourceInsight {
  const id = String(source.id ?? source.sourceId ?? source.data_source_id ?? `source-${index + 1}`);
  const name = String(source.name ?? source.api_name ?? source.tool_name ?? source.title ?? id);
  const path = source.path ?? source.url ?? source.endpoint;
  return { id, name, ...(typeof path === 'string' && path ? { path } : {}) };
}

function collectRuntimeBindings(bindingArtifact: unknown): {
  planPresent: boolean;
  bindings: RuntimeBindingInsight[];
  mappingSources: DataSourceInsight[];
  authoritativeComplete?: boolean;
} {
  if (!isRecord(bindingArtifact)) return { planPresent: false, bindings: [], mappingSources: [] };

  // Current Binder artifacts are admitted by the Host as a versioned
  // submission. The result is the runtime authority; the plan is retained for
  // traceability and must not be confused with the older mappingSchema shape.
  if (bindingArtifact.schema_version === 'agenui.binding-submission/v1') {
    const plan = isRecord(bindingArtifact.plan) ? bindingArtifact.plan : null;
    const result = isRecord(bindingArtifact.result) ? bindingArtifact.result : null;
    const rawBindings = result && Array.isArray(result.bindings)
      ? result.bindings.filter(isRecord)
      : [];
    const bindings: RuntimeBindingInsight[] = [];
    const sources = new Map<string, DataSourceInsight>();
    let executableBindingCount = 0;

    for (const binding of rawBindings) {
      const sourceID = typeof binding.source_id === 'string' ? binding.source_id.trim() : '';
      const fieldPath = typeof binding.field_path === 'string' ? binding.field_path.trim() : '';
      const refKey = typeof binding.ref_key === 'string' ? binding.ref_key.trim() : '';
      const actionPath = typeof binding.action_path === 'string' ? binding.action_path.trim() : '';
      if (sourceID) {
        sources.set(sourceID, { id: sourceID, name: sourceID });
      }
      if (sourceID && ((fieldPath && refKey) || actionPath)) {
        executableBindingCount += 1;
      }
      if (!sourceID || !fieldPath || !refKey) {
        continue;
      }

      const transforms = Array.isArray(binding.transforms)
        ? binding.transforms.filter(isRecord).flatMap((transform) => {
          const versionID = transform.operator_version_id ?? transform.operatorVersionId;
          if ((typeof versionID === 'number' && Number.isFinite(versionID) && versionID > 0) ||
              (typeof versionID === 'string' && versionID.trim())) {
            return [`operator-version:${String(versionID).trim()}`];
          }
          return [];
        })
        : [];
      bindings.push({
        refKey,
        sourceKey: fieldPath,
        sourceId: sourceID,
        transforms,
      });
    }

    const status = typeof result?.status === 'string' ? result.status : '';
    const accepted = status === 'ready' || status === 'ready_with_operators';
    return {
      planPresent: Boolean(
        plan && Array.isArray(plan.field_mappings) && Array.isArray(plan.action_mappings),
      ),
      bindings,
      mappingSources: Array.from(sources.values()),
      authoritativeComplete: accepted &&
        rawBindings.length > 0 &&
        executableBindingCount === rawBindings.length &&
        sources.size > 0,
    };
  }

  const mappingSchema = isRecord(bindingArtifact.mappingSchema) ? bindingArtifact.mappingSchema : null;
  const rawSources = mappingSchema && Array.isArray(mappingSchema.dataSources)
    ? mappingSchema.dataSources.filter(isRecord)
    : [];
  const bindings: RuntimeBindingInsight[] = [];
  for (let sourceIndex = 0; sourceIndex < rawSources.length; sourceIndex += 1) {
    const source = rawSources[sourceIndex];
    const sourceID = String(source.id ?? source.sourceId ?? `source-${sourceIndex + 1}`);
    const processSchema = isRecord(source.responseProcessSchema) ? source.responseProcessSchema : null;
    const mappings = processSchema && Array.isArray(processSchema.bindings)
      ? processSchema.bindings.filter(isRecord)
      : [];
    for (const mapping of mappings) {
      const transforms = Array.isArray(mapping.transform)
        ? mapping.transform.filter(isRecord).map((item) => String(item.type ?? item.call ?? 'transform'))
        : [];
      bindings.push({
        refKey: String(mapping.refKey ?? ''),
        sourceKey: String(mapping.sourceKey ?? ''),
        sourceId: sourceID,
        ...(typeof mapping.operatorRef === 'string' && mapping.operatorRef ? { operatorRef: mapping.operatorRef } : {}),
        transforms,
      });
    }
  }
  return {
    planPresent: Boolean(mappingSchema && Array.isArray(mappingSchema.dataSources)),
    bindings,
    mappingSources: rawSources.map(sourceName),
  };
}

function collectOperators(bindings: RuntimeBindingInsight[]): OperatorInsight[] {
  const operators = new Map<string, OperatorInsight>();
  for (const binding of bindings) {
    if (binding.operatorRef) {
      operators.set(binding.operatorRef, { id: binding.operatorRef, label: binding.operatorRef });
    }
    const computed = binding.sourceKey.match(/^\$\.computed\.([^.[\]]+)/);
    if (computed?.[1]) {
      operators.set(computed[1], { id: computed[1], label: computed[1] });
    }
    for (const transform of binding.transforms) {
      if (transform.startsWith('operator-version:')) {
        operators.set(transform, { id: transform, label: transform });
        continue;
      }
      const id = `transform:${transform}`;
      operators.set(id, { id, label: transform });
    }
  }
  return Array.from(operators.values());
}

function joinPath(parent: string, key: string): string {
  const normalizedKey = key.replaceAll('~', '~0').replaceAll('/', '~1');
  return parent === '/' ? `/${normalizedKey}` : `${parent}/${normalizedKey}`;
}

function collectLeafPaths(value: unknown, path: string, target: Set<string>): void {
  if (Array.isArray(value)) {
    if (value.length === 0) {
      target.add(`${path}[]`);
      return;
    }
    for (const item of value) {
      collectLeafPaths(item, `${path}[]`, target);
    }
    return;
  }
  if (isRecord(value)) {
    const entries = Object.entries(value);
    if (entries.length === 0) {
      target.add(path);
      return;
    }
    for (const [key, child] of entries) {
      collectLeafPaths(child, joinPath(path, key), target);
    }
    return;
  }
  target.add(path);
}

function collectBindings(
  value: unknown,
  componentId: string,
  property: string,
  target: Map<string, DataBindingInsight>,
): void {
  if (Array.isArray(value)) {
    for (const item of value) {
      collectBindings(item, componentId, property, target);
    }
    return;
  }
  if (!isRecord(value)) return;

  if (typeof value.path === 'string') {
    const binding = { componentId, property, path: value.path };
    target.set(`${componentId}:${property}:${value.path}`, binding);
  }
  for (const [key, child] of Object.entries(value)) {
    if (key !== 'action' && key !== 'styles') {
      collectBindings(child, componentId, property || key, target);
    }
  }
}

function actionName(action: Record<string, unknown>): string {
  if (isRecord(action.event) && typeof action.event.name === 'string') return action.event.name;
  if (isRecord(action.function) && typeof action.function.name === 'string') return action.function.name;
  if (typeof action.name === 'string') return action.name;
  if (typeof action.type === 'string') return action.type;
  return '未命名动作';
}

export function analyzeImplementation(
  protocol: Record<string, unknown>[],
  artifacts: ImplementationArtifacts = {},
): ImplementationInsight {
  const versions = new Set<string>();
  const surfaceIds = new Set<string>();
  const dataFields = new Set<string>();
  const bindings = new Map<string, DataBindingInsight>();
  const actions: ActionInsight[] = [];
  let componentCount = 0;
  let hasSurface = false;
  let hasComponents = false;
  let hasData = false;

  for (const message of protocol) {
    if (typeof message.version === 'string') versions.add(message.version);

    const surface = isRecord(message.createSurface) ? message.createSurface : null;
    if (surface) {
      hasSurface = true;
      if (typeof surface.surfaceId === 'string') surfaceIds.add(surface.surfaceId);
    }

    const updateComponents = isRecord(message.updateComponents) ? message.updateComponents : null;
    if (updateComponents) {
      if (typeof updateComponents.surfaceId === 'string') surfaceIds.add(updateComponents.surfaceId);
      const components = Array.isArray(updateComponents.components) ? updateComponents.components : [];
      hasComponents ||= components.length > 0;
      componentCount += components.length;
      for (const rawComponent of components) {
        if (!isRecord(rawComponent)) continue;
        const componentId = typeof rawComponent.id === 'string' ? rawComponent.id : 'unknown';
        const componentType = typeof rawComponent.component === 'string' ? rawComponent.component : 'Component';
        for (const [property, value] of Object.entries(rawComponent)) {
          collectBindings(value, componentId, property, bindings);
        }
        if (isRecord(rawComponent.action)) {
          actions.push({
            componentId,
            componentType,
            name: actionName(rawComponent.action),
          });
        }
      }
    }

    const updateDataModel = isRecord(message.updateDataModel) ? message.updateDataModel : null;
    if (updateDataModel) {
      if (typeof updateDataModel.surfaceId === 'string') surfaceIds.add(updateDataModel.surfaceId);
      if ('value' in updateDataModel) {
        hasData = true;
        collectLeafPaths(updateDataModel.value, '/', dataFields);
      }
    }
  }

  const bindingArtifact = decodeArtifact(artifacts.bindings);
  const apiArtifact = decodeArtifact(artifacts.apis);
  const runtime = collectRuntimeBindings(bindingArtifact);
  const concreteSources = apiRecords(apiArtifact).map(sourceName);
  const dataSources = concreteSources.length > 0 ? concreteSources : runtime.mappingSources;
  const operators = collectOperators(runtime.bindings);
  const previewReady = hasSurface && hasComponents && hasData;
  const bindingComplete = runtime.authoritativeComplete ?? (
    runtime.planPresent &&
    runtime.bindings.length > 0 &&
    runtime.bindings.every((binding) => binding.refKey !== '' && binding.sourceKey !== '') &&
    concreteSources.length > 0
  );

  return {
    version: Array.from(versions).join(', ') || 'unknown',
    surfaceIds: Array.from(surfaceIds),
    componentCount,
    dataFields: Array.from(dataFields),
    bindings: Array.from(bindings.values()),
    actions,
    runtimeBindings: runtime.bindings,
    dataSources,
    operators,
    previewReady,
    bindingPlanPresent: runtime.planPresent,
    bindingComplete,
    bundleReady: previewReady && bindingComplete,
  };
}

export function buildRuntimePackage(
  protocol: Record<string, unknown>[],
  artifacts: ImplementationArtifacts,
): RuntimePackage {
  const insight = analyzeImplementation(protocol, artifacts);
  if (!insight.bundleReady) {
    throw new Error('runtime package requires a complete data binding plan and concrete data source');
  }
  return {
    schemaVersion: 'agenui.runtime-package/v1',
    manifest: {
      ...(artifacts.generationID ? { generationID: artifacts.generationID } : {}),
      ...(typeof artifacts.revision === 'number' ? { revision: artifacts.revision } : {}),
      invocation: {
        accepts: 'application/json',
        inputSchema: { type: 'object', additionalProperties: true },
      },
      pipeline: ['dataSources', 'operators', 'bindings', 'dsl'],
    },
    dsl: protocol,
    bindings: decodeArtifact(artifacts.bindings),
    dataSources: decodeArtifact(artifacts.apis),
    operators: insight.operators,
  };
}
