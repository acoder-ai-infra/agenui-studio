import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const webRoot = fileURLToPath(new URL('../../', import.meta.url));

const adminSurfaceFiles = [
  'app/admin/home/page.tsx',
  'app/admin/home/new/page.tsx',
  'app/admin/datasources/page.tsx',
  'app/admin/operators/page.tsx',
  'app/admin/rules/page.tsx',
  'app/admin/settings/page.tsx',
  'app/admin/settings/publication/page.tsx',
  'app/admin/components/ui/button.tsx',
  'app/admin/components/ui/input.tsx',
  'app/admin/components/ui/textarea.tsx',
];

function readWeb(relativePath: string): string {
  return readFileSync(`${webRoot}${relativePath}`, 'utf8');
}

describe('dark mode source contracts', () => {
  it('uses semantic colors for admin actions and information states', () => {
    for (const file of adminSurfaceFiles) {
      const source = readWeb(file);
      expect(source, file).not.toMatch(/\b(?:bg|text|border|ring|placeholder)-(?:blue|orange)-\d+\b/);
      expect(source, file).not.toMatch(/bg-neutral-900[^\n]*text-white|text-white[^\n]*bg-neutral-900/);
    }
  });

  it('keeps Monaco and live preview synchronized with the active theme', () => {
    const operators = readWeb('app/admin/operators/page.tsx');
    const workbench = readWeb('app/admin/home/page.tsx');
    expect(operators).toContain("theme={theme === 'dark' ? 'vs-dark' : 'light'}");
    expect(workbench).not.toContain('colorScheme="light"');
    expect(workbench).toContain('colorScheme={previewColorScheme}');
    expect(workbench).toContain('data-agenui-preview-theme');
    expect(workbench).toContain('style={{ colorScheme: previewColorScheme }}');
  });

  it('keeps the generated DSL viewer read-only', () => {
    const workbench = readWeb('app/admin/home/page.tsx');
    expect(workbench).toContain('readOnly: true');
    expect(workbench).toContain('domReadOnly: true');
    expect(workbench).not.toContain("onChange={(value) => setStructureSource(value ?? '')}");
  });

  it('isolates renderer-native controls at the Web preview boundary', () => {
    const webCSS = readWeb('app/globals.css');
    expect(webCSS).toContain('[data-agenui-preview-theme][data-color-scheme="dark"]');
    expect(webCSS).toContain('.agenui-textfield');
    expect(webCSS).toContain('.agenui-modal dialog');
  });
});
