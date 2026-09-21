// Streaming proxy for the studio backend.
//
// Next.js `rewrites` buffer proxied responses, which breaks SSE: with a slow
// real model the console would show nothing until the whole run finished.
// A route handler returning the upstream body stream flushes frames as they
// arrive, and keeps the console same-origin (no CORS needed).
import { NextRequest } from 'next/server';

// Keep the proxy default aligned with the formal AGenUI composition root.
const backend = process.env.AGENUI_BACKEND_URL || 'http://127.0.0.1:18081';
// The open-source console has one local workspace by default. A production
// deployment must set these from its authenticated reverse proxy/session;
// they are intentionally server-side proxy configuration, not browser state.
// The local Harness bootstraps its single active tenant as `public`. Keep this
// proxy default aligned with that tenant so canonical Harness replay endpoints
// (events, transcript, controls) receive the same authenticated identity as
// the Studio conversation projection.
const tenantID = process.env.AGENUI_CONSOLE_TENANT_ID || 'public';
const userID = process.env.AGENUI_CONSOLE_USER_ID || 'local';

async function proxy(req: NextRequest): Promise<Response> {
  const url = new URL(req.url);
  const target = `${backend}${url.pathname}${url.search}`;
  const headers = new Headers();
  const contentType = req.headers.get('content-type');
  if (contentType) {
    headers.set('content-type', contentType);
  }
  headers.set('X-AGenUI-Tenant-ID', tenantID);
  headers.set('X-AGenUI-User-ID', userID);
  const idempotencyKey = req.headers.get('idempotency-key');
  if (idempotencyKey) {
    headers.set('idempotency-key', idempotencyKey);
  }
  const init: RequestInit & { duplex?: string } = { method: req.method, headers };
  if (req.method !== 'GET' && req.method !== 'HEAD') {
    init.body = await req.arrayBuffer();
    init.duplex = 'half';
  }
  const upstream = await fetch(target, init);
  const respHeaders = new Headers(upstream.headers);
  // The body is re-streamed; encoding/length from the hop are invalid here.
  respHeaders.delete('content-encoding');
  respHeaders.delete('content-length');
  return new Response(upstream.body, { status: upstream.status, headers: respHeaders });
}

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const DELETE = proxy;
