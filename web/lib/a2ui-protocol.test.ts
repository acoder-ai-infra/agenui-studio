import { describe, expect, it } from 'vitest';
import { BASIC_CATALOG_ID, ensureCreateSurface } from './a2ui-protocol';

describe('ensureCreateSurface', () => {
  it('rewrites an unknown catalogId to the basic catalog', () => {
    const messages = [
      { version: 'v0.9', createSurface: { surfaceId: 'card', catalogId: 'custom-catalog' } },
    ];
    expect(ensureCreateSurface(messages)).toEqual([
      { version: 'v0.9', createSurface: { surfaceId: 'card', catalogId: BASIC_CATALOG_ID } },
    ]);
  });

  it('inserts createSurface when only updateComponents is present', () => {
    const messages = [
      { version: 'v0.9', updateComponents: { surfaceId: 'card', components: [] } },
    ];
    expect(ensureCreateSurface(messages)[0]).toEqual({
      version: 'v0.9',
      createSurface: { surfaceId: 'card', catalogId: BASIC_CATALOG_ID },
    });
  });
});
