export const REFERENCE_IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/webp'] as const;
export const MAX_REFERENCE_IMAGE_SIZE = 10 * 1024 * 1024;
export const MAX_REFERENCE_IMAGES = 8;

type ClipboardFileItem = Pick<DataTransferItem, 'kind' | 'type' | 'getAsFile'>;

export function isSupportedReferenceImage(file: File): boolean {
  return REFERENCE_IMAGE_TYPES.includes(file.type as (typeof REFERENCE_IMAGE_TYPES)[number])
    && file.size <= MAX_REFERENCE_IMAGE_SIZE;
}

export function referenceImagesFromClipboard(items: Iterable<ClipboardFileItem>): File[] {
  return Array.from(items).flatMap((item) => {
    if (item.kind !== 'file' || !item.type.startsWith('image/')) {
      return [];
    }
    const file = item.getAsFile();
    return file ? [file] : [];
  });
}
