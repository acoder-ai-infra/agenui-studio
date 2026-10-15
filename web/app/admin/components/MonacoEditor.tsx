"use client";

import Editor, { loader } from "@monaco-editor/react";
import { version } from "monaco-editor/package.json";

// The wrapper's default CDN version is independent of our lockfile. Load the
// installed version so dependency/security upgrades also reach the browser.
loader.config({
  paths: { vs: `https://cdn.jsdelivr.net/npm/monaco-editor@${version}/min/vs` },
});

export default Editor;
