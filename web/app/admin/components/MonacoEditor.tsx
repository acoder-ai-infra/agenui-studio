"use client";

import Editor, { loader } from "@monaco-editor/react";

const version = process.env.NEXT_PUBLIC_MONACO_VERSION;

// The wrapper's default CDN version is independent of our lockfile. Load the
// installed version so dependency/security upgrades also reach the browser.
loader.config({
  paths: { vs: `https://cdn.jsdelivr.net/npm/monaco-editor@${version}/min/vs` },
});

export default Editor;
