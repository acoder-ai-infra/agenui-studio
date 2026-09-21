import { describe, expect, it } from 'vitest';

import { analyzeImplementation, buildRuntimePackage } from './agenui-implementation';

describe('analyzeImplementation', () => {
  it('summarizes the final protocol as structure, data bindings, actions, and runtime readiness', () => {
    const insight = analyzeImplementation([
      { version: 'v0.9', createSurface: { surfaceId: 'default' } },
      {
        version: 'v0.9',
        updateComponents: {
          surfaceId: 'default',
          components: [
            { id: 'title', component: 'Text', text: { path: '/title' } },
            { id: 'list', component: 'List', children: { path: '/items', componentId: 'row' } },
            { id: 'button', component: 'Button', action: { event: { name: 'item.open' } } },
          ],
        },
      },
      {
        version: 'v0.9',
        updateDataModel: {
          surfaceId: 'default',
          path: '/',
          value: { title: '推荐', items: [{ name: 'A', price: 10 }, { name: 'B', price: 20 }] },
        },
      },
    ], {
      bindings: JSON.stringify({
        mappingSchema: {
          dataSources: [{
            id: 'hotel-source',
            responseProcessSchema: {
              bindings: [
                { refKey: '/title', sourceKey: '$.data.title' },
                { refKey: '/items[*]/name', sourceKey: '$.computed.rankHotels[*].name', transform: [{ type: 'array_pick' }] },
              ],
            },
          }],
        },
      }),
      apis: JSON.stringify({ apis: [{ id: 'hotel-api', name: 'HotelSearch', path: '/v1/hotels' }] }),
    });

    expect(insight.version).toBe('v0.9');
    expect(insight.surfaceIds).toEqual(['default']);
    expect(insight.componentCount).toBe(3);
    expect(insight.dataFields).toEqual(['/title', '/items[]/name', '/items[]/price']);
    expect(insight.bindings).toEqual([
      { componentId: 'title', property: 'text', path: '/title' },
      { componentId: 'list', property: 'children', path: '/items' },
    ]);
    expect(insight.actions).toEqual([
      { componentId: 'button', componentType: 'Button', name: 'item.open' },
    ]);
    expect(insight.previewReady).toBe(true);
    expect(insight.bindingComplete).toBe(true);
    expect(insight.bundleReady).toBe(true);
    expect(insight.dataSources).toEqual([{ id: 'hotel-api', name: 'HotelSearch', path: '/v1/hotels' }]);
    expect(insight.operators).toEqual([
      { id: 'rankHotels', label: 'rankHotels' },
      { id: 'transform:array_pick', label: 'array_pick' },
    ]);
  });

  it('does not report an incomplete protocol as runnable', () => {
    const insight = analyzeImplementation([
      { version: 'v0.9', updateComponents: { components: [] } },
    ]);

    expect(insight.previewReady).toBe(false);
    expect(insight.bindingComplete).toBe(false);
    expect(insight.bundleReady).toBe(false);
    expect(insight.componentCount).toBe(0);
    expect(insight.dataFields).toEqual([]);
  });

  it('does not treat embedded DSL data as a completed real-data binding', () => {
    const insight = analyzeImplementation([
      { version: 'v0.9', createSurface: { surfaceId: 'default' } },
      { version: 'v0.9', updateComponents: { components: [{ id: 'title', component: 'Text', text: { path: '/title' } }] } },
      { version: 'v0.9', updateDataModel: { value: { title: '示例' } } },
    ]);

    expect(insight.previewReady).toBe(true);
    expect(insight.bindings).toHaveLength(1);
    expect(insight.bindingPlanPresent).toBe(false);
    expect(insight.bundleReady).toBe(false);
  });

  it('recognizes an accepted binding submission without a legacy APIs projection', () => {
    const insight = analyzeImplementation([
      { version: 'v0.9', createSurface: { surfaceId: 'default' } },
      { version: 'v0.9', updateComponents: { components: [{ id: 'price', component: 'Text', text: { path: '/food_price' } }] } },
      { version: 'v0.9', updateDataModel: { value: { food_price: '¥29.90' } } },
    ], {
      bindings: JSON.stringify({
        schema_version: 'agenui.binding-submission/v1',
        plan: {
          schema_version: 'agenui.executable-binding/v1',
          field_mappings: [{ refKey: '/food_price', sourceKey: '$.items[*].price_cents' }],
          action_mappings: [{ componentId: 'detail', actionSourceType: 'url', urlPath: '$.items[*].detail_url' }],
        },
        result: {
          schema_version: 'agenui.bind-result/v1',
          status: 'ready_with_operators',
          bindings: [
            {
              requirement_id: 'data.food_price',
              target_slot_ids: ['price'],
              source_id: 'demo.offers@v1',
              knowledge_id: 'demo.offers@sha256:test',
              field_path: '$.items[*].price_cents',
              ref_key: '/food_price',
              transforms: [{ operator_version_id: 1 }],
            },
            {
              requirement_id: 'action.detail',
              target_slot_ids: ['detail'],
              source_id: 'demo.offers@v1',
              knowledge_id: 'demo.offers@sha256:test',
              action_path: '$.items[*].detail_url',
              action_source_type: 'url',
              component_id: 'detail',
            },
          ],
          issues: [],
        },
      }),
    });

    expect(insight.bindingPlanPresent).toBe(true);
    expect(insight.bindingComplete).toBe(true);
    expect(insight.bundleReady).toBe(true);
    expect(insight.runtimeBindings).toEqual([{
      refKey: '/food_price',
      sourceKey: '$.items[*].price_cents',
      sourceId: 'demo.offers@v1',
      transforms: ['operator-version:1'],
    }]);
    expect(insight.dataSources).toEqual([{ id: 'demo.offers@v1', name: 'demo.offers@v1' }]);
    expect(insight.operators).toEqual([{ id: 'operator-version:1', label: 'operator-version:1' }]);
  });

  it('does not mark a blocked binding submission as complete', () => {
    const insight = analyzeImplementation([
      { version: 'v0.9', createSurface: { surfaceId: 'default' } },
      { version: 'v0.9', updateComponents: { components: [{ id: 'title', component: 'Text', text: { path: '/title' } }] } },
      { version: 'v0.9', updateDataModel: { value: { title: '示例' } } },
    ], {
      bindings: JSON.stringify({
        schema_version: 'agenui.binding-submission/v1',
        plan: { schema_version: 'agenui.executable-binding/v1', field_mappings: [], action_mappings: [] },
        result: { schema_version: 'agenui.bind-result/v1', status: 'blocked', bindings: [], issues: [] },
      }),
    });

    expect(insight.bindingPlanPresent).toBe(true);
    expect(insight.bindingComplete).toBe(false);
    expect(insight.bundleReady).toBe(false);
  });

  it('builds a parameterized runtime package only after binding is complete', () => {
    const protocol = [
      { version: 'v0.9', createSurface: { surfaceId: 'default' } },
      { version: 'v0.9', updateComponents: { components: [{ id: 'title', component: 'Text', text: { path: '/title' } }] } },
      { version: 'v0.9', updateDataModel: { value: { title: '示例' } } },
    ];
    const artifacts = {
      generationID: 'gen-1',
      revision: 2,
      bindings: JSON.stringify({ mappingSchema: { dataSources: [{ id: 'ds-0', responseProcessSchema: { bindings: [{ refKey: '/title', sourceKey: '$.data.title' }] } }] } }),
      apis: JSON.stringify([{ id: 'source-1', name: 'TitleAPI' }]),
    };

    const bundle = buildRuntimePackage(protocol, artifacts);
    expect(bundle.schemaVersion).toBe('agenui.runtime-package/v1');
    expect(bundle.manifest.invocation).toEqual({
      accepts: 'application/json',
      inputSchema: { type: 'object', additionalProperties: true },
    });
    expect(bundle.manifest.pipeline).toEqual(['dataSources', 'operators', 'bindings', 'dsl']);
    expect(bundle.dsl).toEqual(protocol);
  });
});
