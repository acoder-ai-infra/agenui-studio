import { readFileSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const diagramDir = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(diagramDir, '..', '..');
const templatePath = join(diagramDir, 'product-overview.svg');
const screenshotPath = join(projectRoot, 'docs', 'assets', 'studio-workbench.png');
const outputPath = join(projectRoot, 'docs', 'assets', 'product-overview.png');

const template = readFileSync(templatePath, 'utf8');
const screenshot = readFileSync(screenshotPath).toString('base64');
const rendered = template.replace(
  '__WORKBENCH_DATA_URI__',
  `data:image/png;base64,${screenshot}`,
);

if (rendered === template) {
  throw new Error('product overview template is missing its screenshot placeholder');
}

const result = spawnSync('rsvg-convert', ['-o', outputPath, '-'], {
  input: rendered,
  maxBuffer: 8 * 1024 * 1024,
});

if (result.status !== 0) {
  throw new Error(result.stderr.toString() || 'rsvg-convert failed');
}

writeFileSync(1, `rendered ${outputPath}\n`);
