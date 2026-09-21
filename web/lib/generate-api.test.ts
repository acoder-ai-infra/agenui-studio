import { afterEach, describe, expect, it, vi } from "vitest";

import { decodeAGenUIResult, findRecoverableSessionRun, projectHarnessTranscript, resolveControl, resumeSessionRun, sessionApi, sessionEvents, sessionPresentation, sessionProgress, sessionReplay, sessionRunStatus, type CanonicalFrame } from "./generate-api";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

describe("AGenUI control resume", () => {
  it("uses native Harness multipart chat when reference images are attached", async () => {
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode('data: {"type":"done","data":{}}\n\n'));
        controller.close();
      },
    });
    let request: RequestInit | undefined;
    globalThis.fetch = vi.fn(async (_input, init) => {
      request = init;
      return new Response(stream, { status: 200, headers: { "content-type": "text/event-stream" } });
    }) as typeof fetch;

    const image = new File([new Uint8Array([1, 2, 3])], "reference.png", { type: "image/png" });
    await sessionApi.generate("new", "参考图片生成商品卡", {}, [image]);

    expect(request?.headers).toBeUndefined();
    expect(request?.body).toBeInstanceOf(FormData);
    const form = request?.body as FormData;
    expect(form.get("prompt")).toBe("参考图片生成商品卡");
    expect(form.get("sessionId")).toBe("");
    expect((form.get("attachments") as File).name).toBe("reference.png");
  });

  it("projects every live ask_user question and its option descriptions", async () => {
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(
          'data: {"type":"data-control","data":{"requestId":"control-live","controlTicket":"ticket-live","resumeTargets":["interrupt-root"],"questions":[' +
          '{"header":"类型","question":"展示什么类型？","options":[{"label":"单品|||single","description":"展示一道菜"}]},' +
          '{"header":"操作","question":"提供什么操作？","options":[{"label":"查看详情|||detail","description":"跳转详情页"}]}' +
          ']}}\n\n' +
          'data: [DONE]\n\n',
        ));
        controller.close();
      },
    });
    globalThis.fetch = vi.fn(async () => new Response(stream, {
      status: 200,
      headers: {
        "content-type": "text/event-stream",
        "x-harness-session-id": "session-live",
        "x-harness-run-id": "run-live",
      },
    })) as typeof fetch;
    const frames: CanonicalFrame[] = [];

    const outcome = await sessionApi.generate("session-live", "生成卡片", {
      onFrame: (frame) => frames.push(frame),
    });

    expect(outcome).toEqual({ interrupted: true, failed: false });
    expect(frames).toHaveLength(1);
    expect(frames[0].event_type).toBe("control_request_created");
    expect(frames[0].run_id).toBe("run-live");
    expect((frames[0].payload as Record<string, unknown>).questions).toMatchObject([
      {
        id: "q0",
        header: "类型",
        question: "展示什么类型？",
        options: [
          { id: "q0_opt0", label: "单品", description: "展示一道菜", type: "select" },
          { id: "q0_other", type: "input" },
        ],
      },
      {
        id: "q1",
        header: "操作",
        question: "提供什么操作？",
        options: [
          { id: "q1_opt0", label: "查看详情", description: "跳转详情页", type: "select" },
          { id: "q1_other", type: "input" },
        ],
      },
    ]);
  });

  it("places child lifecycle and tool processes with their root turn", () => {
    const frames = projectHarnessTranscript({
      messages: [
        { id: "message-user", role: "user", text: "生成列表", runId: "run-root" },
        { role: "assistant", text: "已经生成。", runId: "run-root" },
      ],
      attachments: [{
        artifactRef: "artifact://tenants/public/sessions/session/runs/run-root/image",
        messageId: "message-user",
        name: "reference.png",
        mimeType: "image/png",
        type: "image",
      }],
      runs: [
        { runId: "run-root", status: "completed" },
        { runId: "run-style", parentRunId: "run-root", agentId: "agenui_style", status: "completed" },
      ],
      commentaryByRun: {
        "run-root": [{
          eventId: "ack-root",
          runId: "run-root",
          text: "已收到，我会先梳理内容，再生成界面。",
        }],
      },
      processByRun: {
        "run-style": [
          {
            schemaVersion: "harness.agent_chat_process.v1",
            processId: "run-style:workspace",
            runId: "run-style",
            parentRunId: "run-root",
            status: "running",
            category: "tool",
            title: "agenui_workspace",
            action: "commit",
          },
          {
            schemaVersion: "harness.agent_chat_process.v1",
            processId: "run-style:workspace",
            runId: "run-style",
            parentRunId: "run-root",
            status: "completed",
            category: "tool",
            title: "agenui_workspace",
            action: "commit",
          },
        ],
      },
    });

    expect(frames.map((frame) => frame.event_type)).toEqual([
      "user_message_received",
      "agent_commentary",
      "harness_process",
      "harness_process",
      "harness_process",
      "agent_text_delta",
    ]);
    expect(frames[0].payload?.attachments).toEqual([{
      artifactRef: "artifact://tenants/public/sessions/session/runs/run-root/image",
      name: "reference.png",
      mimeType: "image/png",
      type: "image",
    }]);
    expect(frames[1].payload).toMatchObject({
      text: "已收到，我会先梳理内容，再生成界面。",
      event_id: "ack-root",
    });
    expect(frames[2].payload).toMatchObject({
      eventType: "sub_agent_run",
      category: "subagent",
      runId: "run-style",
      parentRunId: "run-root",
      status: "completed",
    });
    expect(frames[3].payload).toMatchObject({
      title: "agenui_workspace",
      action: "commit",
      status: "running",
    });
    expect(frames[4].payload?.status).toBe("completed");
  });

  it("restores a failed root run as an assistant error after refresh", () => {
    const frames = projectHarnessTranscript({
      messages: [{ role: "user", text: "生成商品卡", runId: "run-failed" }],
      runs: [{
        runId: "run-failed",
        status: "failed",
        errorCode: "RESUME_FAILED",
        errorMessage: "frozen config is missing",
      }],
    });

    expect(frames.map((frame) => frame.event_type)).toEqual([
      "user_message_received",
      "run_failed",
    ]);
    expect(frames[1].payload?.error).toBe("RESUME_FAILED");
  });

  it("preserves root tool output in the shared live/replay process shape", () => {
    const output = {
      schema_version: "agenui.next_steps.v1",
      plan_id: "next_0123456789abcdef01234567",
      title: "下一步",
      items: [{
        id: "next_0123456789abcdef01234567_1",
        importance: "recommended",
        label: "接入商品详情接口",
        prompt: "请把当前商品卡接入商品详情接口。",
      }],
    };
    const frames = projectHarnessTranscript({
      messages: [
        { role: "user", text: "生成商品卡", runId: "run-root" },
        { role: "assistant", text: "已经完成", runId: "run-root" },
      ],
      runs: [
        { runId: "run-root", status: "completed" },
        { runId: "run-late", parentRunId: "run-root", status: "completed" },
        { runId: "run-early", parentRunId: "run-root", status: "completed" },
      ],
      controlsByRun: {
        "run-late": [{
          requestId: "control-late",
          createdAt: "2026-09-01T10:00:02Z",
          status: "answered",
          answerText: "保留",
          questions: [{ question: "是否保留按钮？" }],
        }],
        "run-early": [{
          requestId: "control-early",
          createdAt: "2026-09-01T10:00:01Z",
          status: "answered",
          answerText: "列表",
          questions: [{ question: "使用哪种结构？" }],
        }],
      },
    });

    expect(frames
      .filter((frame) => frame.event_type === "control_request_created")
      .map((frame) => frame.payload?.control_id))
      .toEqual(["control-early", "control-late"]);
  });

  it("decodes Final Artifact result in string, object and array forms", () => {
    const messages = [{ version: "v0.9", createSurface: { surfaceId: "default", catalogId: "catalog" } }];
    expect(decodeAGenUIResult(messages)).toEqual(messages);
    expect(decodeAGenUIResult(messages[0])).toEqual(messages);
    expect(decodeAGenUIResult(JSON.stringify(messages))).toEqual(messages);
  });

  it("restores the last successful AGenUI result after a later failed turn", async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({
      sessionId: "session-failed-edit",
      runId: "run-success",
      result: [{ version: "v0.9", createSurface: { surfaceId: "default", catalogId: "catalog" } }],
      bindings: "binding-v1",
    }), { status: 200, headers: { "content-type": "application/json" } })) as typeof fetch;

    const presentation = await sessionPresentation("session-failed-edit");
    expect(presentation?.result).toContain("createSurface");
    expect(presentation?.result).not.toContain("agenui-json");
    expect(presentation?.bindings).toBe("binding-v1");
  });

  it("treats a valid session without a design artifact as not ready", async () => {
    globalThis.fetch = vi.fn(async () => new Response(null, { status: 204 })) as typeof fetch;

    await expect(sessionPresentation("session-waiting-control")).resolves.toBeNull();
  });

  it("treats a blocked delivery as a non-executable draft", async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({
      sessionId: "session-blocked",
      runId: "run-blocked",
      result: [{ version: "v0.9", createSurface: { surfaceId: "default", catalogId: "catalog" } }],
      bindings: "binding-blocked",
      bindingStatus: "blocked",
      draft: false,
      executable: false,
      publishable: false,
    }), { status: 200, headers: { "content-type": "application/json" } })) as typeof fetch;

    const presentation = await sessionPresentation("session-blocked");
    expect(presentation).toMatchObject({
      bindingStatus: "blocked",
      draft: true,
      executable: false,
      publishable: false,
    });
  });

  it("restores durable ask_user answers from the Harness transcript", async () => {
    globalThis.fetch = vi.fn(async (input) => {
      const url = String(input);
      if (url.includes("conversation/detail")) {
        return new Response(JSON.stringify({
          code: 1,
          data: {
            messages: [{ role: "user", content: "生成商品卡" }],
            metadata: { run_id: "run-answered" },
          },
        }), { status: 200, headers: { "content-type": "application/json" } });
      }
      if (url.includes("chat-transcript")) {
        return new Response(JSON.stringify({
          messages: [{ role: "user", text: "生成商品卡", runId: "run-answered" }],
          controlsByRun: {
            "run-answered": [{
              requestId: "control-answered",
              status: "answered",
              answerText: "多个",
              questions: [{ question: "展示一个还是多个商品？", options: [{ label: "多个" }] }],
            }],
          },
        }), { status: 200, headers: { "content-type": "application/json" } });
      }
      return new Response(JSON.stringify({ events: [] }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    }) as typeof fetch;

    const frames = await sessionEvents("session-answered");
    expect(frames.map((frame) => frame.event_type)).toEqual([
      "user_message_received",
      "control_request_created",
      "user_message_received",
    ]);
    expect(frames[1].payload?.answered).toBe(true);
    expect(frames[2].payload?.text).toBe("多个");
  });

  it("reads the durable progress projection without replaying chat content", async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({
      processByRun: {
        "run-live": [{
          schemaVersion: "harness.agent_chat_process.v1",
          eventId: "event-contract",
          processId: "run-live:tool:contract",
          groupKey: "tool:run-live:tool:contract",
          status: "completed",
          title: "卡片内容契约已生成",
          summary: "商品名称、价格和标签",
        }],
      },
    }), { status: 200, headers: { "content-type": "application/json" } })) as typeof fetch;

    const frames = await sessionProgress("session-live");
    expect(frames).toHaveLength(1);
    expect(frames[0].event_type).toBe("harness_process");
    expect(frames[0].payload?.title).toBe("卡片内容契约已生成");
  });

  it("selects the latest recoverable top-level run and ignores child runs", () => {
    expect(findRecoverableSessionRun([
      { runId: "run-completed", status: "completed" },
      { runId: "run-child", parentRunId: "run-completed", status: "running" },
      { runId: "run-created", status: "created" },
      { runId: "run-live", status: "running" },
    ])).toEqual({ runId: "run-live", status: "running" });
  });

  it("returns the active run with replayed chat history", async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({
      messages: [{ role: "user", text: "生成商品卡", runId: "run-live" }],
      runs: [{ runId: "run-live", status: "running" }],
    }), { status: 200, headers: { "content-type": "application/json" } })) as typeof fetch;

    const replay = await sessionReplay("session-live");

    expect(replay.activeRun).toEqual({ runId: "run-live", status: "running" });
    expect(replay.frames.map((frame) => frame.event_type)).toEqual(["user_message_received"]);
  });

  it("restores the latest completed turn state after refresh", async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({
      messages: [
        { role: "user", text: "生成商品卡", runId: "run-completed" },
        { role: "assistant", text: "已经生成完成。", runId: "run-completed" },
      ],
      runs: [
        { runId: "run-older", status: "completed" },
        { runId: "run-completed", status: "completed" },
      ],
    }), { status: 200, headers: { "content-type": "application/json" } })) as typeof fetch;

    const replay = await sessionReplay("session-completed");

    expect(replay.activeRun).toBeUndefined();
    expect(replay.frames.map((frame) => frame.event_type)).toEqual([
      "user_message_received",
      "agent_text_delta",
      "run_completed",
    ]);
    expect(replay.frames.at(-1)?.run_id).toBe("run-completed");
  });

  it("restores a waiting_control prompt after refresh", async () => {
    globalThis.fetch = vi.fn(async (input) => {
      if (String(input).includes("chat-transcript")) {
        return new Response(JSON.stringify({
          messages: [{ role: "user", text: "生成商品卡", runId: "run-waiting" }],
          runs: [{ runId: "run-waiting", status: "waiting_control" }],
        }), { status: 200, headers: { "content-type": "application/json" } });
      }
      return new Response(JSON.stringify({
        events: [{
          sequence: 3,
          event_type: "control_request_created",
          payload: {
            request_id: "control-waiting",
            checkpoint_id: "checkpoint-waiting",
            control_ticket: "ticket-waiting",
            interrupt_contexts: [{
              id: "interrupt-root",
              info: {
                questions: [{
                  question: "展示一个还是多个商品？",
                  options: [{ label: "多个" }],
                }],
              },
            }],
          },
        }],
      }), { status: 200, headers: { "content-type": "application/json" } });
    }) as typeof fetch;

    const replay = await sessionReplay("session-waiting");

    expect(replay.activeRun).toBeUndefined();
    expect(replay.cursor).toEqual({ "run-waiting": 3 });
    expect(replay.frames.map((frame) => frame.event_type)).toEqual([
      "user_message_received",
      "control_request_created",
    ]);
  });


  it("re-attaches an existing run stream and reads its durable status", async () => {
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(
          'data: {"type":"start","messageId":"run-live"}\n\n' +
          'data: {"type":"data-commentary","data":{"schemaVersion":"harness.agent_chat_commentary.v1","eventId":"ack-live","runId":"run-live","text":"已收到，正在处理。"}}\n\n' +
          'data: {"type":"text-delta","delta":"完成"}\n\n' +
          'data: {"type":"data-cursor","data":{"schemaVersion":"harness.agent_chat_cursor.v1","sequences":{"run-live":5,"run-child":3}}}\n\n' +
          'data: [DONE]\n\n',
        ));
        controller.close();
      },
    });
    const abort = new AbortController();
    globalThis.fetch = vi.fn(async (input) => {
      if (String(input).includes("/chat-stream")) {
        return new Response(stream, {
          status: 200,
          headers: { "content-type": "text/event-stream" },
        });
      }
      return new Response(JSON.stringify({
        runs: [{ run_id: "run-live", status: "completed" }],
      }), { status: 200, headers: { "content-type": "application/json" } });
    }) as typeof fetch;
    const events: string[] = [];
    let cursor: Record<string, number> = {};

    await resumeSessionRun("session-live", "run-live", {
      onFrame: (frame) => events.push(frame.event_type),
      onCursor: (sequences) => { cursor = sequences; },
    }, { signal: abort.signal, cursor: { "run-live": 4, "run-child": 2 } });
    const status = await sessionRunStatus("session-live", "run-live", abort.signal);

    expect(events).toEqual(["agent_commentary", "agent_text_delta", "run_completed"]);
    expect(cursor).toEqual({ "run-live": 5, "run-child": 3 });
    expect(status).toBe("completed");
    const streamURL = String(vi.mocked(globalThis.fetch).mock.calls[0][0]);
    expect(decodeURIComponent(streamURL)).toContain('"sequences":{"run-live":4,"run-child":2}');
    expect(vi.mocked(globalThis.fetch).mock.calls[0][1]).toEqual(
      expect.objectContaining({ cache: "no-store", signal: abort.signal }),
    );
  });

  it("consumes the resume SSE stream with the same handlers as generate", async () => {
    const interruptID = interrupt("session-resume");
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(
          ': keepalive\n\n' +
          'data: {"type":"delta","data":{"kind":"progress_activity","schema_version":"agenui.progress_activity.v1","timeline_id":"t1","transition_id":"t2","sequence":2,"updates":[]}}\n\n' +
          ': keepalive\n\n' +
          'data: {"type":"done","data":{"session_id":"session-resume"}}\n\n',
        ));
        controller.close();
      },
    });
    let responseRequest: RequestInit | undefined;
    globalThis.fetch = vi.fn(async (input, init) => String(input).includes("/responses")
      ? (responseRequest = init, new Response(JSON.stringify({ request_id: "control-resume", status: "answered" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        }))
      : new Response(stream, {
          status: 200,
          headers: { "content-type": "text/event-stream" },
        })) as typeof fetch;

    const events: string[] = [];
    await resolveControl("run-resume", interruptID, {
      questionID: "q0",
      optionID: "q0_opt0",
      text: "内容/文章卡片",
    }, {
      onFrame: (frame) => events.push(frame.event_type),
    });

    expect(events).toEqual(["run_completed"]);
    expect(globalThis.fetch).toHaveBeenCalledWith(
      "/api/v1/control-requests/control-resume/responses",
      expect.objectContaining({ method: "POST" }),
    );
    expect(JSON.parse(String(responseRequest?.body))).toEqual(expect.objectContaining({
      decision: "answer",
      response_text: "内容/文章卡片",
      value: {
        question_id: "q0",
        option_id: "q0_opt0",
        text: "内容/文章卡片",
      },
      targets: {
        "interrupt-root": {
          answers: [{
            question_index: 0,
            question_id: "q0",
            selected_option: {
              id: "q0_opt0",
              label: "内容/文章卡片",
            },
          }],
        },
      },
    }));
  });

  it("submits every ask_user answer in one control response and one resume", async () => {
    const interruptID = batchInterrupt("session-resume");
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode('data: [DONE]\n\n'));
        controller.close();
      },
    });
    let responseRequest: RequestInit | undefined;
    globalThis.fetch = vi.fn(async (input, init) => {
      if (String(input).includes("/responses")) {
        responseRequest = init;
        return new Response(JSON.stringify({ request_id: "control-resume", status: "answered" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(stream, {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      });
    }) as typeof fetch;

    const outcome = await resolveControl("run-resume", interruptID, [
      { questionID: "q0", optionID: "q0_opt0", text: "单道菜品" },
      { questionID: "q1", optionID: "q1_opt0", text: "查看详情" },
    ], {}, { cursor: { "run-resume": 17, "run-child": 9 } });
    const body = JSON.parse(String(responseRequest?.body));

    expect(outcome).toEqual({ interrupted: false, failed: false });
    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
    const resumeURL = decodeURIComponent(String(vi.mocked(globalThis.fetch).mock.calls[1][0]));
    expect(resumeURL).toContain('"sequences":{"run-resume":17,"run-child":9}');
    expect(body.response_text).toBe("卡片类型：单道菜品\n交互动作：查看详情");
    expect(body.value).toEqual({
      answers: [
        { question_id: "q0", option_id: "q0_opt0", text: "单道菜品" },
        { question_id: "q1", option_id: "q1_opt0", text: "查看详情" },
      ],
    });
    expect(body.targets["interrupt-root"].answers).toEqual([
      {
        question_index: 0,
        question_id: "q0",
        selected_option: { id: "q0_opt0", label: "单道菜品", value: "single" },
      },
      {
        question_index: 1,
        question_id: "q1",
        selected_option: { id: "q1_opt0", label: "查看详情", value: "detail" },
      },
    ]);
  });

  it("does not resume a multi-question control with a partial answer", async () => {
    globalThis.fetch = vi.fn() as typeof fetch;

    await expect(resolveControl("run-resume", batchInterrupt("session-resume"), [
      { questionID: "q0", optionID: "q0_opt0", text: "单道菜品" },
    ])).rejects.toThrow("answer every Harness control question");
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });

  it("surfaces the Harness error message and stable code when a resume fails", async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({
      error_code: "CONTROL_ANSWER_FAILED",
      message: "registry config snapshot is missing",
    }), {
      status: 500,
      headers: { "content-type": "application/json" },
    })) as typeof fetch;

    await expect(resolveControl("run-resume", interrupt("session-resume"), {
      questionID: "q0",
      optionID: "q0_opt0",
      text: "内容/文章卡片",
    })).rejects.toThrow("registry config snapshot is missing (CONTROL_ANSWER_FAILED)");
  });
});

function interrupt(sessionID: string): string {
  const payload = JSON.stringify({
    s: sessionID,
    r: "run-resume",
    q: "control-resume",
    t: "ticket-resume",
    contexts: ["interrupt-root"],
    questions: [{
      id: "q0",
      body: "请选择卡片类型",
      options: [{ id: "q0_opt0", label: "内容/文章卡片" }],
    }],
  });
  return `sdk1.${Buffer.from(payload).toString("base64url")}`;
}

function batchInterrupt(sessionID: string): string {
  const payload = JSON.stringify({
    s: sessionID,
    r: "run-resume",
    q: "control-resume",
    t: "ticket-resume",
    contexts: ["interrupt-root"],
    questions: [
      {
        id: "q0",
        title: "卡片类型",
        body: "请选择卡片类型",
        options: [{ id: "q0_opt0", label: "单道菜品", value: "single" }],
      },
      {
        id: "q1",
        title: "交互动作",
        body: "请选择交互动作",
        options: [{ id: "q1_opt0", label: "查看详情", value: "detail" }],
      },
    ],
  });
  return `sdk1.${Buffer.from(payload).toString("base64url")}`;
}
