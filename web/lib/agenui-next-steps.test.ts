import { describe, expect, it } from "vitest";

import {
  isNextStepsProcess,
  nextStepsFromProcess,
  nextStepsSchemaVersion,
  nextStepsToolName,
  nextStepSupportingText,
} from "./agenui-next-steps";

function completedProcess(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  const planID = "next_0123456789abcdef01234567";
  return {
    schemaVersion: "harness.agent_chat_process.v1",
    category: "tool",
    status: "completed",
    agentId: "agenui_agent",
    activity: { key: nextStepsToolName, label: "整理下一步" },
    output: {
      schema_version: nextStepsSchemaVersion,
      plan_id: planID,
      title: "下一步建议",
      items: [{
        id: `${planID}_1`,
        label: "接入商品详情接口",
        description: "将当前示例数据替换为真实商品信息。",
        prompt: "请把当前商品卡接入商品详情接口，并保持现有样式不变。",
      }],
    },
    ...overrides,
  };
}

describe("AGenUI next-step process projection", () => {
  it("uses explicit descriptions and falls back for persisted v2.0 plans", () => {
    expect(nextStepSupportingText({
      id: "next_0123456789abcdef01234567_1",
      label: "继续数据绑定",
      description: "保留模型给出的原始说明",
      prompt: "请继续完成数据绑定",
    })).toBe("保留模型给出的原始说明");

    expect(nextStepSupportingText({
      id: "next_0123456789abcdef01234567_1",
      label: "继续数据绑定",
      prompt: "请继续完成数据绑定",
    })).toBe("请继续完成数据绑定");

    expect(nextStepSupportingText({
      id: "next_0123456789abcdef01234567_1",
      label: "继续数据绑定",
      prompt: "继续数据绑定",
    })).toBe("");
  });

  it("accepts a completed, Host-issued root Agent plan", () => {
    const process = completedProcess();

    expect(isNextStepsProcess(process)).toBe(true);
    expect(nextStepsFromProcess(process)).toEqual({
      schemaVersion: nextStepsSchemaVersion,
      planId: "next_0123456789abcdef01234567",
      items: [{
        id: "next_0123456789abcdef01234567_1",
        label: "接入商品详情接口",
        description: "将当前示例数据替换为真实商品信息。",
        prompt: "请把当前商品卡接入商品详情接口，并保持现有样式不变。",
      }],
    });
  });

  it("recognizes a running publisher but does not expose its input as a plan", () => {
    const process = completedProcess({
      status: "running",
      output: undefined,
      input: {
        schema_version: nextStepsSchemaVersion,
        items: [{ label: "不应提前展示" }],
      },
    });

    expect(isNextStepsProcess(process)).toBe(true);
    expect(nextStepsFromProcess(process)).toBeUndefined();
  });

  it("rebuilds the plan from bounded result presentation when output is not inline", () => {
    const process = completedProcess({
      title: "下一步",
      summary: "agenui.next_steps.v1:next_0123456789abcdef01234567",
      output: undefined,
      details: [{
        label: "recommended|接入商品详情接口",
        value: JSON.stringify({
          d: "将当前示例数据替换为真实商品信息。",
          p: "请把当前商品卡接入商品详情接口，并保持现有样式不变。",
        }),
      }],
    });

    expect(nextStepsFromProcess(process)).toMatchObject({
      planId: "next_0123456789abcdef01234567",
      items: [{
        id: "next_0123456789abcdef01234567_1",
        label: "接入商品详情接口",
        description: "将当前示例数据替换为真实商品信息。",
      }],
    });
  });

  it("rebuilds the v2 presentation with its description", () => {
    const process = completedProcess({
      title: "下一步建议",
      summary: "agenui.next_steps.v2:next_0123456789abcdef01234567",
      output: undefined,
      details: [{
        label: "继续数据绑定",
        value: JSON.stringify({
          d: "接入真实数据，同时保持当前样式不变。",
          p: "继续完成当前界面的数据绑定",
        }),
      }],
    });

    expect(nextStepsFromProcess(process)).toEqual({
      schemaVersion: nextStepsSchemaVersion,
      planId: "next_0123456789abcdef01234567",
      items: [{
        id: "next_0123456789abcdef01234567_1",
        label: "继续数据绑定",
        description: "接入真实数据，同时保持当前样式不变。",
        prompt: "继续完成当前界面的数据绑定",
      }],
    });
  });

  it("keeps the original plain-text v2 presentation replayable", () => {
    const process = completedProcess({
      title: "下一步",
      summary: "agenui.next_steps.v2:next_0123456789abcdef01234567",
      output: undefined,
      details: [{
        label: "继续数据绑定",
        value: "继续完成当前界面的数据绑定",
      }],
    });

    expect(nextStepsFromProcess(process)).toEqual({
      schemaVersion: nextStepsSchemaVersion,
      planId: "next_0123456789abcdef01234567",
      items: [{
        id: "next_0123456789abcdef01234567_1",
        label: "继续数据绑定",
        prompt: "继续完成当前界面的数据绑定",
      }],
    });
  });

  it("rejects an unrelated presentation title", () => {
    expect(nextStepsFromProcess(completedProcess({
      title: "推荐操作",
      output: undefined,
      summary: "agenui.next_steps.v2:next_0123456789abcdef01234567",
      details: [{ label: "把主标题字号调大", value: "请把主标题字号调大" }],
    }))).toBeUndefined();
  });

  it("keeps persisted v1 output replayable with its description", () => {
    const planID = "next_0123456789abcdef01234567";
    const process = completedProcess({
      output: {
        schema_version: "agenui.next_steps.v1",
        plan_id: planID,
        title: "下一步",
        items: [{
          id: `${planID}_1`,
          importance: "recommended",
          label: "接入商品详情接口",
          description: "历史说明继续展示",
          prompt: "请把当前商品卡接入商品详情接口。",
        }],
      },
    });

    expect(nextStepsFromProcess(process)).toMatchObject({
      schemaVersion: "agenui.next_steps.v1",
      items: [{
        label: "接入商品详情接口",
        description: "历史说明继续展示",
        prompt: "请把当前商品卡接入商品详情接口。",
      }],
    });
  });

  it("rejects child Agent publishers and malformed or oversized output", () => {
    expect(nextStepsFromProcess(completedProcess({
      agentId: "agenui_style",
      parentRunId: "run-root",
    }))).toBeUndefined();

    const malformed = completedProcess();
    const output = malformed.output as Record<string, unknown>;
    const items = output.items as Array<Record<string, unknown>>;
    items[0] = { ...items[0], prompt: "需".repeat(161) };
    expect(nextStepsFromProcess(malformed)).toBeUndefined();

    const oversizedDescription = completedProcess();
    const oversizedOutput = oversizedDescription.output as Record<string, unknown>;
    const oversizedItems = oversizedOutput.items as Array<Record<string, unknown>>;
    oversizedItems[0] = { ...oversizedItems[0], description: "长".repeat(161) };
    expect(nextStepsFromProcess(oversizedDescription)).toBeUndefined();

    expect(nextStepsFromProcess(completedProcess({
      output: { schema_version: "agenui.next_steps.v2" },
    }))).toBeUndefined();
  });
});
