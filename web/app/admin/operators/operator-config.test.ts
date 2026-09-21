import { describe, expect, it } from 'vitest';

import {
  DEFAULT_OPERATOR_CODE,
  OPERATOR_ENTRY,
  OPERATOR_LANGUAGE,
  operatorSavePayload,
} from './operator-config';

describe('operator editor TypeScript contract', () => {
  it('uses a typed run function as the default source', () => {
    expect(OPERATOR_LANGUAGE).toBe('typescript');
    expect(OPERATOR_ENTRY).toBe('run');
    expect(DEFAULT_OPERATOR_CODE).toContain('value: unknown');
    expect(DEFAULT_OPERATOR_CODE).toContain('params: OperatorParams');
  });

  it('persists new drafts as TypeScript operators', () => {
    expect(operatorSavePayload({
      operatorKey: '',
      name: ' meters_to_km ',
      description: 'distance conversion',
      usageScenario: 'distance fields',
      code: DEFAULT_OPERATOR_CODE,
    })).toEqual({
      operatorKey: undefined,
      name: 'meters_to_km',
      description: 'distance conversion',
      usageScenario: 'distance fields',
      language: 'typescript',
      entry: 'run',
      sourceCode: DEFAULT_OPERATOR_CODE,
    });
  });
});
