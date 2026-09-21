export const BASIC_CATALOG_ID = 'https://a2ui.org/specification/v0_9/basic_catalog.json';

/** Agent 可能返回任意 catalog 版本 ID；预览侧只注册了 basic catalog。 */
export function ensureCreateSurface(list: Record<string, unknown>[]): Record<string, unknown>[] {
  const idx = list.findIndex((item) => 'createSurface' in item);
  if (idx !== -1) {
    const createSurface = (list[idx].createSurface ?? {}) as Record<string, unknown>;
    if (createSurface.catalogId === BASIC_CATALOG_ID) {
      return list;
    }
    const patched = [...list];
    patched[idx] = {
      ...list[idx],
      createSurface: { ...createSurface, catalogId: BASIC_CATALOG_ID },
    };
    return patched;
  }
  const componentItem = list.find((item) => 'updateComponents' in item);
  const component = (componentItem?.updateComponents ?? {}) as Record<string, unknown>;
  const surfaceId = (component.surfaceId as string) || 'preview-surface';
  return [{ version: 'v0.9', createSurface: { surfaceId, catalogId: BASIC_CATALOG_ID } }, ...list];
}
