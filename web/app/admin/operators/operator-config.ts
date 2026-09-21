export const OPERATOR_LANGUAGE = 'typescript' as const;
export const OPERATOR_ENTRY = 'run' as const;

export const DEFAULT_OPERATOR_CODE = `type OperatorParams = Record<string, unknown>;

function run(value: unknown, params: OperatorParams): unknown {
  // value：字段原始值；params：受约束参数
  return value;
}`;

export function operatorSavePayload(form: {
  operatorKey: string;
  name: string;
  description: string;
  usageScenario: string;
  code: string;
}) {
  return {
    operatorKey: form.operatorKey || undefined,
    name: form.name.trim(),
    description: form.description,
    usageScenario: form.usageScenario,
    language: OPERATOR_LANGUAGE,
    entry: OPERATOR_ENTRY,
    sourceCode: form.code,
  };
}
