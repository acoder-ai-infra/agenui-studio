import { afterEach, describe, expect, it, vi } from 'vitest';

import { conversationApi } from './generate-api';

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

describe('conversationApi.list', () => {
  it('requests and maps the active root Run status', async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({
      sessions: [
        { id: 'session-1', title: 'latest chat', active_run_status: 'running' },
        { id: 'session-2', title: 'finished chat' },
      ],
    }), { status: 200 })) as typeof fetch;

    const result = await conversationApi.list();

    expect(globalThis.fetch).toHaveBeenCalledWith(
      expect.stringContaining('include_active_run=true'),
      { cache: 'no-store' },
    );
    expect(result.items).toEqual([
      {
        sessionId: 'session-1',
        title: 'latest chat',
        activeRunStatus: 'running',
        metadata: { title: 'latest chat' },
      },
      {
        sessionId: 'session-2',
        title: 'finished chat',
        activeRunStatus: undefined,
        metadata: { title: 'finished chat' },
      },
    ]);
  });
});
