import { describe, expect, it } from "vitest";

import {
  applyHarnessProcess,
  formatProgressOutput,
  applyProgressFrame,
  createAssistantTextReplayGuard,
  finalizeProgressTimeline,
  groupProgressActivities,
  inferRunningProgressStage,
  projectAnsweredControls,
  projectConversationHistory,
  projectHarnessProcess,
  projectHarnessProcesses,
  projectPendingControl,
  runFailureCode,
} from "./agenui-console-state";

describe("AGenUI console state", () => {
  it("deduplicates Assistant text across repeated replay-from-zero reconnects", () => {
    const firstConnection = createAssistantTextReplayGuard("ABC");
    expect(firstConnection("A")).toEqual({ kind: "skip" });
    expect(firstConnection("BC")).toEqual({ kind: "skip" });
    expect(firstConnection("D")).toEqual({ kind: "append", text: "D" });

    // The next physical connection must use the latest visible text, including
    // the suffix appended by the previous connection.
    const secondConnection = createAssistantTextReplayGuard("ABCD");
    expect(secondConnection("AB")).toEqual({ kind: "skip" });
    expect(secondConnection("CD")).toEqual({ kind: "skip" });
    expect(secondConnection("E")).toEqual({ kind: "append", text: "E" });
  });

  it("merges public process lifecycle by run and process identity", () => {
    const frames = projectHarnessProcesses({
      "run-root": [
        {
          schemaVersion: "harness.agent_chat_process.v1",
          processId: "run-root:contract",
          runId: "run-root",
          category: "tool",
          status: "running",
          title: "agenui_submit_content_contract",
        },
        {
          schemaVersion: "harness.agent_chat_process.v1",
          processId: "run-root:contract",
          runId: "run-root",
          category: "tool",
          status: "completed",
          title: "agenui_submit_content_contract",
          summary: "内容契约已生成",
        },
      ],
    });

    expect(frames).toHaveLength(1);
    expect(frames[0]).toMatchObject({
      run_id: "run-root",
      event_type: "runtime_step_completed",
      payload: {
        process_id: "run-root:contract",
        step: "agenui_submit_content_contract",
        detail: "内容契约已生成",
      },
    });
  });

  it("preserves the public Workspace action without exposing its other input", () => {
    const frame = projectHarnessProcess({
      schemaVersion: "harness.agent_chat_process.v1",
      processId: "run-style:workspace-1",
      runId: "run-style",
      parentRunId: "run-root",
      category: "tool",
      status: "completed",
      title: "agenui_workspace",
      input: { action: "commit", document: { secret: "must-not-leak" } },
    });

    expect(frame?.payload).toMatchObject({
      step: "agenui_workspace · commit",
      action: "commit",
      parent_run_id: "run-root",
    });
    expect(JSON.stringify(frame)).not.toContain("must-not-leak");

    const terminal = projectHarnessProcesses({
      "run-style": [
        {
          schemaVersion: "harness.agent_chat_process.v1",
          processId: "run-style:workspace-1",
          status: "running",
          title: "agenui_workspace",
          input: { action: "commit" },
        },
        {
          schemaVersion: "harness.agent_chat_process.v1",
          processId: "run-style:workspace-1",
          status: "completed",
          title: "agenui_workspace",
        },
      ],
    });
    expect(terminal[0].payload?.step).toBe("agenui_workspace · commit");
    expect(terminal[0].event_type).toBe("runtime_step_completed");
  });

  it("keeps a visible diagnostic when a public process has no stable identity", () => {
    const frame = projectHarnessProcess({
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "event-unowned",
      runId: "run-root",
      category: "tool",
      status: "failed",
      title: "unknown_tool",
    });

    expect(frame?.payload?.projection_warning).toContain("event-unowned");
    expect(frame?.event_type).toBe("runtime_step_failed");
  });

  it("merges native Harness tool lifecycle parts without inventing progress events", () => {
    const started = applyHarnessProcess(undefined, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "event-started",
      processId: "run-1:tool:call-1",
      groupKey: "tool:run-1:tool:call-1",
      runId: "run-1",
      status: "running",
      title: "生成内容契约",
    });
    const completed = applyHarnessProcess(started, {
      schemaVersion: "harness.agent_chat_process.v1",
      // Child-run lifecycle projections intentionally reuse one stable event ID.
      eventId: "event-started",
      processId: "run-1:tool:call-1",
      groupKey: "tool:run-1:tool:call-1",
      runId: "run-1",
      status: "completed",
      title: "卡片内容契约已生成",
      summary: "生成酒店列表卡",
      details: [{ label: "卡片类型", value: "列表卡" }],
      output: { total: 2, items: [{ name: "A" }, { name: "B" }] },
    });

    expect(completed?.activities).toHaveLength(1);
    expect(completed?.activities[0]).toMatchObject({
      status: "completed",
      title: "卡片内容契约已生成",
      summary: "生成酒店列表卡",
      details: [{ label: "卡片类型", value: "列表卡" }],
      output: { total: 2, items: [{ name: "A" }, { name: "B" }] },
    });
    expect(applyHarnessProcess(completed, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "event-started",
      processId: "run-1:tool:call-1",
      groupKey: "tool:run-1:tool:call-1",
      status: "completed",
      title: "不应重复",
    })).toEqual(completed);

    expect(applyHarnessProcess(completed, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "event-started",
      processId: "run-1:tool:call-1",
      groupKey: "tool:run-1:tool:call-1",
      status: "running",
      title: "旧的运行中快照",
    })).toEqual(completed);
  });

  it("formats inline JSON tool results and bounds oversized output", () => {
    expect(formatProgressOutput('{"ok":true,"items":[1,2]}')).toEqual({
      text: '{\n  "ok": true,\n  "items": [\n    1,\n    2\n  ]\n}',
      truncated: false,
    });
    expect(formatProgressOutput("abcdef", 4)).toEqual({ text: "abcd\n…", truncated: true });
    expect(formatProgressOutput(JSON.stringify({
      query: "food",
      results: [{
        data_source_id: "demo.offers",
        method: "GET",
        path: "/demo/offers",
        description: "Offers",
        response_example: { items: [{ title: "large payload" }] },
      }],
    }))).toEqual({
      text: '{\n  "query": "food",\n  "total": 1,\n  "results": [\n    {\n      "data_source_id": "demo.offers",\n      "method": "GET",\n      "path": "/demo/offers",\n      "description": "Offers"\n    }\n  ]\n}',
      truncated: false,
    });
    expect(formatProgressOutput({
      ok: true,
      execution: { operator_id: 1, output: "¥29.90", output_hash: "hidden" },
    })).toEqual({
      text: '{\n  "ok": true,\n  "execution": {\n    "operator_id": 1,\n    "output": "¥29.90"\n  }\n}',
      truncated: false,
    });
    expect(formatProgressOutput(
      '{"query":"food","results":[{"data_source_id":"demo.offers","description":"Offers","method":"GET","path":"/demo/offers","response_model":{"large":true}},{"data_source_id":"demo.products","description":"Products","method":"GET","path":"/demo/products"}...',
    )).toEqual({
      text: '{\n  "query": "food",\n  "total": 2,\n  "results": [\n    {\n      "data_source_id": "demo.offers",\n      "method": "GET",\n      "path": "/demo/offers",\n      "description": "Offers"\n    },\n    {\n      "data_source_id": "demo.products",\n      "method": "GET",\n      "path": "/demo/products",\n      "description": "Products"\n    }\n  ]\n}',
      truncated: false,
    });
  });

  it("groups repeated tool calls by dynamic stage and activity metadata", () => {
    let timeline = undefined;
    for (let index = 0; index < 9; index += 1) {
      timeline = applyHarnessProcess(timeline, {
        schemaVersion: "harness.agent_chat_process.v1",
        eventId: `event-${index}`,
        processId: `run-style:tool:call-${index}`,
        groupKey: `tool:run-style:tool:call-${index}`,
        runId: "run-style",
        category: "tool",
        status: "completed",
        title: "一次生成结果",
        stage: { id: "ui_generation", label: "生成界面", order: 20 },
        activity: { key: "agenui_workspace", label: "生成界面结构", detailLevel: "secondary" },
      });
    }

    const stages = groupProgressActivities(timeline?.activities ?? []);
    expect(stages).toHaveLength(1);
    expect(stages[0]).toMatchObject({
      stageId: "ui_generation",
      label: "生成界面",
      status: "completed",
      activities: [{
        activityKey: "agenui_workspace",
        label: "生成界面结构",
        count: 9,
        status: "completed",
      }],
    });
    expect(stages[0].activities[0].invocations).toHaveLength(9);
  });

  it("uses child Run lifecycle to keep its latest tool stage active", () => {
    let timeline = applyHarnessProcess(undefined, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "run:run-style",
      processId: "run:run-style",
      groupKey: "subagent:run:run-style",
      eventType: "sub_agent_run",
      category: "subagent",
      status: "running",
      title: "agenui_style",
      runId: "run-style",
      parentRunId: "run-root",
      agentId: "agenui_style",
    });
    expect(timeline?.activities).toHaveLength(0);
    expect(timeline?.subagents).toEqual([expect.objectContaining({
      run_id: "run-style",
      status: "in_progress",
    })]);
    expect(groupProgressActivities(timeline?.activities ?? [], timeline?.subagents ?? [])).toEqual([
      expect.objectContaining({
        stageId: "execution",
        status: "in_progress",
        activities: [],
      }),
    ]);

    timeline = applyHarnessProcess(timeline, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "tool-started",
      processId: "run-style:tool:call-1",
      groupKey: "tool:run-style:tool:call-1",
      eventType: "tool_call_started",
      runId: "run-style",
      sequence: 3,
      category: "tool",
      status: "running",
      title: "生成界面结构",
      stage: { id: "ui_generation", label: "生成界面", order: 20 },
      activity: { key: "agenui_workspace", label: "生成界面结构", detailLevel: "secondary" },
    });
    timeline = applyHarnessProcess(timeline, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "tool-completed",
      processId: "run-style:tool:call-1",
      groupKey: "tool:run-style:tool:call-1",
      eventType: "tool_call_completed",
      runId: "run-style",
      sequence: 4,
      category: "tool",
      status: "completed",
      title: "卡片版式与内容结构已生成",
      summary: "界面结构已经校验并提交，可在预览区查看结果",
      stage: { id: "ui_generation", label: "生成界面", order: 20 },
      activity: { key: "agenui_workspace", label: "生成界面结构", detailLevel: "secondary" },
    });

    let stages = groupProgressActivities(timeline?.activities ?? [], timeline?.subagents ?? []);
    expect(stages).toHaveLength(1);
    expect(stages[0]).toMatchObject({
      stageId: "ui_generation",
      status: "in_progress",
      activities: [{ status: "completed", count: 1 }],
      subagents: [{ run_id: "run-style", status: "in_progress" }],
    });

    timeline = applyHarnessProcess(timeline, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "run:run-style",
      processId: "run:run-style",
      groupKey: "subagent:run:run-style",
      eventType: "sub_agent_run",
      category: "subagent",
      status: "completed",
      title: "agenui_style",
      runId: "run-style",
      parentRunId: "run-root",
      agentId: "agenui_style",
    });
    stages = groupProgressActivities(timeline?.activities ?? [], timeline?.subagents ?? []);
    expect(stages[0].status).toBe("completed");
    expect(stages[0].activities).toHaveLength(1);
  });

  it("orders arbitrary stages and keeps an unconfigured future tool visible", () => {
    const binding = applyHarnessProcess(undefined, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "binding",
      processId: "run:tool:binding",
      groupKey: "tool:run:tool:binding",
      category: "tool",
      status: "running",
      title: "绑定数据",
      stage: { id: "data_binding", label: "配置数据与动作", order: 30 },
      activity: { key: "bind", label: "绑定数据", detailLevel: "secondary" },
    });
    const generated = applyHarnessProcess(binding, {
      schemaVersion: "harness.agent_chat_process.v1",
      eventId: "future",
      processId: "run:tool:future",
      groupKey: "tool:run:tool:future",
      category: "tool",
      status: "completed",
      title: "执行工具",
      stage: { id: "future_stage", label: "未来阶段", order: 15 },
      activity: { key: "future_tool", label: "执行工具", detailLevel: "secondary" },
    });

    expect(groupProgressActivities(generated?.activities ?? []).map((stage) => stage.stageId)).toEqual([
      "future_stage",
      "data_binding",
    ]);
  });

  it("keeps the latest completed Agent stage visually active while its root Run continues", () => {
    const stages = groupProgressActivities([
      {
        activity_id: "contract",
        status: "completed",
        title: "生成内容契约",
        details: [],
        stage_id: "requirements",
        stage_label: "理解内容要求",
        stage_order: 10,
      },
      {
        activity_id: "surface",
        status: "completed",
        title: "生成界面结构",
        details: [],
        stage_id: "ui_generation",
        stage_label: "生成界面",
        stage_order: 20,
      },
    ]);

    expect(inferRunningProgressStage(stages, true)).toBe("ui_generation");
    expect(inferRunningProgressStage(stages, false)).toBeUndefined();
  });

  it("does not override native active or failed progress with the root Run fallback", () => {
    const active = groupProgressActivities([{
      activity_id: "surface",
      status: "in_progress",
      title: "生成界面结构",
      details: [],
      stage_id: "ui_generation",
      stage_label: "生成界面",
    }]);
    expect(inferRunningProgressStage(active, true)).toBeUndefined();

    const failed = groupProgressActivities([{
      activity_id: "surface",
      status: "failed",
      title: "生成界面结构",
      details: [],
      stage_id: "ui_generation",
      stage_label: "生成界面",
    }]);
    expect(inferRunningProgressStage(failed, true)).toBeUndefined();
  });

  it("merges progress_init and ordered progress_activity frames", () => {
    const initial = applyProgressFrame(undefined, {
      kind: "progress_init",
      schema_version: "agenui.progress_activity.v1",
      timeline_id: "timeline-1",
      last_sequence: 1,
      activities: [
        {
          activity_id: "data_source",
          status: "in_progress",
          title: "正在匹配数据",
          details: [],
        },
      ],
    });
    const advanced = applyProgressFrame(initial, {
      kind: "progress_activity",
      schema_version: "agenui.progress_activity.v1",
      timeline_id: "timeline-1",
      transition_id: "transition-2",
      sequence: 2,
      updates: [
        {
          activity_id: "data_source",
          status: "completed",
          title: "数据已匹配",
          details: [{ label: "采用接口", value: "商品查询" }],
        },
        {
          activity_id: "template",
          status: "in_progress",
          title: "正在组织卡片",
          details: [],
        },
      ],
    });

    expect(advanced?.lastSequence).toBe(2);
    expect(advanced?.activities.map((item) => [item.activity_id, item.status])).toEqual([
      ["data_source", "completed"],
      ["template", "in_progress"],
    ]);
    // Idempotent replay must not duplicate or roll back state.
    expect(applyProgressFrame(advanced, {
      kind: "progress_activity",
      schema_version: "agenui.progress_activity.v1",
      timeline_id: "timeline-1",
      transition_id: "transition-2",
      sequence: 2,
      updates: [],
    })).toEqual(advanced);
  });

  it("finalizes an active progress row when the stream ends", () => {
    const current = applyProgressFrame(undefined, {
      kind: "progress_init",
      schema_version: "agenui.progress_activity.v1",
      timeline_id: "timeline-final",
      last_sequence: 2,
      activities: [{
        activity_id: "preview_check",
        status: "in_progress",
        title: "正在做最后检查",
        details: [],
      }],
    });

    expect(finalizeProgressTimeline(current, "completed")?.activities[0].status).toBe("completed");
    expect(finalizeProgressTimeline(current, "failed")?.activities[0].status).toBe("failed");
    expect(finalizeProgressTimeline(current, "interrupted")?.activities[0].status).toBe("waiting");
  });

  it("restores intent and progress from conversation history", () => {
    const frames = projectConversationHistory({
      code: 1,
      data: {
        messages: [
          { role: "user", content: "生成商品卡" },
          {
            role: "assistant",
            content: "商品卡已生成。",
            intentText: "明白，我会生成一张商品卡。",
            progress_init: {
              kind: "progress_init",
              schema_version: "agenui.progress_activity.v1",
              timeline_id: "timeline-1",
              last_sequence: 3,
              activities: [{
                activity_id: "template",
                status: "waiting",
                title: "还需要你确认样式",
                details: [],
              }],
            },
          },
        ],
      },
    });

    expect(frames.map((frame) => frame.event_type)).toEqual([
      "user_message_received",
      "agent_text_delta",
      "progress_init",
    ]);
    expect(frames[1].payload?.text).toBe("明白，我会生成一张商品卡。");
    expect(JSON.stringify(frames)).not.toContain("createSurface");
  });

  it("restores answered ask_user controls and user answer bubbles from the durable transcript", () => {
    const frames = projectAnsweredControls({
      "run-1": [{
        requestId: "control-1",
        status: "answered",
        answerText: "多个",
        questions: [{ question: "展示一个还是多个商品？", options: [{ label: "单个" }, { label: "多个" }] }],
      }],
    });

    expect(frames.map((frame) => frame.event_type)).toEqual([
      "control_request_created",
      "user_message_received",
    ]);
    expect(frames[0].run_id).toBe("run-1");
    expect(frames[0].payload).toMatchObject({
      control_id: "control-1",
      question: "展示一个还是多个商品？",
      answered: true,
    });
    expect(frames[1].payload?.text).toBe("多个");
    expect(frames[1].run_id).toBe("run-1");
  });

  it("restores every answered question from one structured ask_user response", () => {
    const frames = projectAnsweredControls({
      "run-1": [{
        requestId: "control-batch",
        status: "answered",
        answerText: "类型：单品\n操作：查看详情",
        answers: [
          { questionId: "q0", text: "单品" },
          { questionId: "q1", text: "查看详情" },
        ],
        questions: [
          {
            header: "类型",
            question: "展示单品还是列表？",
            options: [{ label: "单品", description: "展示一道菜" }],
          },
          {
            header: "操作",
            question: "提供什么操作？",
            options: [{ label: "查看详情", description: "跳转详情页" }],
          },
        ],
      }],
    });

    expect(frames[0].payload?.questions).toMatchObject([
      {
        id: "q0",
        header: "类型",
        question: "展示单品还是列表？",
        options: [
          { label: "单品", description: "展示一道菜" },
          { id: "q0_other", type: "input" },
        ],
      },
      {
        id: "q1",
        header: "操作",
        question: "提供什么操作？",
        options: [
          { label: "查看详情", description: "跳转详情页" },
          { id: "q1_other", type: "input" },
        ],
      },
    ]);
    expect(frames[1].payload?.text).toBe("类型：单品\n操作：查看详情");
  });

  it("keeps a failed resume answer visible and projects a durable error", () => {
    const frames = projectAnsweredControls({
      "run-1": [{
        requestId: "control-failed",
        status: "resume_failed",
        answerText: "查看详情",
        questions: [{ question: "需要什么操作？", options: [{ label: "查看详情" }] }],
      }],
    });

    expect(frames.map((frame) => frame.event_type)).toEqual([
      "control_request_created",
      "user_message_received",
      "run_resume_failed",
    ]);
    expect(frames[0].payload).toMatchObject({ answered: true, resume_failed: true });
    expect(frames[1].payload?.text).toBe("查看详情");
    expect(frames[2].payload?.error).toBe("RESUME_FAILED");
  });

  it("projects an unanswered native control request into the existing ask_user card contract", () => {
    const frame = projectPendingControl("session-1", "run-1", [
      {
        event_id: "event-control",
        sequence: 9,
        run_id: "run-1",
        session_id: "session-1",
        event_type: "control_request_created",
        payload: {
          request_id: "control-1",
          checkpoint_id: "checkpoint-1",
          control_ticket: "ticket-1",
          preamble: "需要确认展示范围",
          interrupt_contexts: [{
            id: "context-1",
            info: {
              questions: [
                {
                  header: "范围",
                  question: "展示一个还是多个商品？",
                  options: [
                    { label: "单个|||single", description: "展示一个商品" },
                    { label: "多个|||list", description: "展示商品列表" },
                  ],
                },
                {
                  header: "操作",
                  question: "卡片需要什么操作？",
                  options: [{ label: "查看详情|||detail", description: "跳转到详情页" }],
                },
              ],
            },
          }],
        },
      },
    ]);

    expect(frame?.event_type).toBe("control_request_created");
    expect(frame?.payload?.question).toBe("展示一个还是多个商品？");
    expect(frame?.payload?.options).toEqual([
      { id: "q0_opt0", label: "单个" },
      { id: "q0_opt1", label: "多个" },
      // The projected free-text option carries no label: the console owns that
      // wording so the projection stays free of locale-specific copy.
      { id: "q0_other", label: "" },
    ]);
    expect(frame?.payload?.questions).toMatchObject([
      {
        id: "q0",
        header: "范围",
        question: "展示一个还是多个商品？",
        options: [
          { id: "q0_opt0", label: "单个", description: "展示一个商品", type: "select" },
          { id: "q0_opt1", label: "多个", description: "展示商品列表", type: "select" },
          { id: "q0_other", label: "", type: "input" },
        ],
      },
      {
        id: "q1",
        header: "操作",
        question: "卡片需要什么操作？",
        options: [
          { id: "q1_opt0", label: "查看详情", description: "跳转到详情页", type: "select" },
          { id: "q1_other", label: "", type: "input" },
        ],
      },
    ]);
    expect(String(frame?.payload?.control_id)).toMatch(/^sdk1\./);
  });

  it("recovers the stable failure code from server-localized failure text", () => {
    expect(runFailureCode("运行失败（RUNTIME_ADAPTER_FAILED）")).toBe("RUNTIME_ADAPTER_FAILED");
    expect(runFailureCode("模型服务请求失败（MODEL_PROVIDER_4XX）")).toBe("MODEL_PROVIDER_4XX");
    expect(runFailureCode("RESUME_FAILED")).toBe("RESUME_FAILED");
    // Harness omits the brackets when it has no code; the console then keeps the
    // server text instead of substituting copy of its own.
    expect(runFailureCode("运行失败")).toBeUndefined();
    // A trailing parenthetical that is not a code must not be mistaken for one.
    expect(runFailureCode("run failed (see log)")).toBeUndefined();
  });
});
