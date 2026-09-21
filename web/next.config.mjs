/** @type {import('next').NextConfig} */
// The AGenUI service listens on 18081 by default. Deployments
// may still override this without changing the management-console contract.
const backend = process.env.AGENUI_BACKEND_URL || "http://127.0.0.1:18081";

// NOTE: /api/* is proxied by app/api/[...path]/route.ts (a streaming route
// handler) instead of rewrites — rewrites buffer SSE and would stall the
// canonical event stream for slow real-model runs.
const nextConfig = {
  // Keep production builds separate from the development server cache. Running
  // `npm run build` while Studio is open must not invalidate its CSS/chunks.
  distDir: process.env.AGENUI_NEXT_DIST_DIR || ".next",
  transpilePackages: ["@agenui/react", "@agenui/web-core"],
  async rewrites() {
    return [
      { source: "/mock/:path*", destination: `${backend}/mock/:path*` },
    ];
  },
};

export default nextConfig;
