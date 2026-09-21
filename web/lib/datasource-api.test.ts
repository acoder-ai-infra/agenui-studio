import { afterEach, describe, expect, it, vi } from 'vitest';

import { datasourceApi, type DataSourceItem } from './datasource-api';

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

describe('datasourceApi.save', () => {
  it('generates the backend-required ID and description for a new data source', async () => {
    let payload: Record<string, unknown> | undefined;
    globalThis.fetch = vi.fn(async (_input, init) => {
      payload = JSON.parse(String(init?.body)) as Record<string, unknown>;
      return new Response('{}', { status: 200 });
    }) as typeof fetch;

    await datasourceApi.save({
      name: '美食卡接口',
      description: '',
      endpoint: 'http://localhost:7001/delicious',
      method: 'GET',
      fields: [{
        path: 'name',
        name: '名称',
        semantic: '名称',
        valueType: 'string',
        scope: 'list_item',
      }],
    });

    expect(payload?.id).toMatch(/^local\.[0-9a-f-]{36}$/);
    expect(payload?.description).toBe('美食卡接口');
    expect(payload?.projectName).toBe('美食卡接口');
  });

  it('keeps the identity and description while editing an existing data source', async () => {
    let payload: Record<string, unknown> | undefined;
    globalThis.fetch = vi.fn(async (_input, init) => {
      payload = JSON.parse(String(init?.body)) as Record<string, unknown>;
      return new Response('{}', { status: 200 });
    }) as typeof fetch;

    await datasourceApi.save({
      id: 'demo.offer.list',
      name: '优惠列表',
      description: '返回当前可用优惠',
      endpoint: '/demo/offers',
      method: 'GET',
      fields: [],
    } satisfies Partial<DataSourceItem>);

    expect(payload?.id).toBe('demo.offer.list');
    expect(payload?.description).toBe('返回当前可用优惠');
  });
});
