import { generateUUID } from './utils';

// Data source (API capability) adapter (open-source backend).

export interface FieldMeta {
  path: string;
  name: string;
  semantic: string;
  valueType: string;
  unit?: string;
  scope: string;
  entityKey?: string;
}

export interface DataSourceItem {
  id: string;
  name: string;
  description?: string;
  endpoint: string;
  method: string;
  itemsPath?: string;
  entityKey?: string;
  fields: FieldMeta[];
  params?: { name: string; in: string; template?: string }[];
  tags?: string[];
}

export interface RecallCandidate {
  apiId: string;
  apiName: string;
  score: number;
  provider: string;
  field: FieldMeta;
}

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const parsed = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(parsed.error ?? `HTTP ${res.status}`);
  }
  return (await res.json()) as T;
}

export const datasourceApi = {
  async list(): Promise<{ items: DataSourceItem[] }> {
    const source = await json<{ items: Array<Record<string, unknown>> }>(
      await fetch('/api/v1/agenui/admin/local/apis'),
    );
    return {
      items: (source.items ?? []).map((item) => ({
        id: String(item.id ?? ''),
        name: String(item.projectName || item.id || ''),
        description: String(item.description ?? ''),
        endpoint: String(item.path ?? ''),
        method: String(item.method ?? 'GET'),
        fields: Array.isArray(item.responseFields) ? item.responseFields as FieldMeta[] : [],
      })),
    };
  },
  async save(item: Partial<DataSourceItem>): Promise<void> {
    const name = item.name?.trim() ?? '';
    const description = item.description?.trim() || name;
    const id = item.id?.trim() || `local.${generateUUID()}`;
    await json(
      await fetch('/api/v1/agenui/admin/local/apis', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          id,
          searchText: [name, description, ...(item.fields ?? []).flatMap((field) => [field.name, field.semantic])]
            .filter(Boolean)
            .join(' '),
          path: item.endpoint,
          method: item.method,
          description,
          projectName: name,
          responseModel: {},
          responseExample: {},
          responseFields: item.fields ?? [],
        }),
      }),
    );
  },
  async recall(query: string): Promise<{ items: RecallCandidate[] }> {
    return json(await fetch(`/api/v1/agenui/admin/local/knowledge/search?q=${encodeURIComponent(query)}`));
  },
  async initDemo(): Promise<{ report: { apis: number; operators: number; rules: number } }> {
    return json(await fetch('/api/v1/agenui/admin/local/init-demo', { method: 'POST' }));
  },
};
