import { describe, expect, it } from 'vitest';
import {
  isSupportedReferenceImage,
  referenceImagesFromClipboard,
} from './reference-images';

describe('reference images', () => {
  it('extracts image files from clipboard items', () => {
    const image = new File([new Uint8Array([1, 2, 3])], 'clipboard.png', { type: 'image/png' });
    const files = referenceImagesFromClipboard([
      { kind: 'string', type: 'text/plain', getAsFile: () => null },
      { kind: 'file', type: 'text/plain', getAsFile: () => new File(['text'], 'note.txt') },
      { kind: 'file', type: 'image/png', getAsFile: () => image },
      { kind: 'file', type: 'image/jpeg', getAsFile: () => null },
    ]);

    expect(files).toEqual([image]);
  });

  it('keeps the upload format and size constraints for pasted images', () => {
    expect(isSupportedReferenceImage(new File(['ok'], 'ok.webp', { type: 'image/webp' }))).toBe(true);
    expect(isSupportedReferenceImage(new File(['gif'], 'no.gif', { type: 'image/gif' }))).toBe(false);
    expect(isSupportedReferenceImage({
      name: 'large.png',
      type: 'image/png',
      size: 10 * 1024 * 1024 + 1,
    } as File)).toBe(false);
  });
});
