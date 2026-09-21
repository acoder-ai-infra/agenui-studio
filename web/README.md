# AGenUI Studio Admin Console

The bundled Next.js console provides the AGenUI management workbench:

- `/admin/home` — streaming card generation, live preview, multi-turn editing
  and control-response handling.
- `/admin/datasources`, `/admin/operators`, `/admin/rules`, and
  `/admin/settings` manage the bundled local data plane.

`/admin/home` starts and resumes work through native Harness Chat, Run, event,
transcript and Control APIs. It uses AGenUI APIs only for final artifacts,
runtime packages and local resource management.

## Build

```sh
npm install
npm run build
npm run dev
```

The development server listens on port 3100. By default its API proxy targets
the AGenUI service at `http://127.0.0.1:18081`; configure
`AGENUI_BACKEND_URL` to override it, and configure the
AGenUI service separately; a successful frontend build is not evidence of a
real-model generation run.
