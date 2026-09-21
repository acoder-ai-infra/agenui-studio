import { describe, expect, it } from 'vitest';
import { basicCatalog, minimalCatalog } from '@agenui/react';
import { createA2uiRuntimeProcessor, getA2uiSurfaceIds } from './a2ui-runtime';

describe('createA2uiRuntimeProcessor', () => {
  it('accepts Studio-style AGenUI v0.9 messages', () => {
    const processor = createA2uiRuntimeProcessor(
      [minimalCatalog, basicCatalog],
      [
        {
          version: 'v0.9',
          createSurface: {
            surfaceId: 'default',
            catalogId: 'https://agenui.org/specification/v0_9/catalog.json',
          },
        },
        {
          version: 'v0.9',
          updateComponents: {
            surfaceId: 'default',
            components: [
              { id: 'root', component: 'Column', children: ['title'] },
              { id: 'title', component: 'Text', text: '城市博物馆通票' },
            ],
          },
        },
      ],
    );

    expect(getA2uiSurfaceIds(processor)).toEqual(['default']);
    expect(processor.model.getSurface('default')).toBeTruthy();
  });
});
