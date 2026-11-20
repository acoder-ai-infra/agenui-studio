import { describe, expect, it } from "vitest";
import { resolve } from "node:path";
import postcss from "postcss";
import tailwind from "@tailwindcss/postcss";

// Compile the real stylesheet: theme tokens, alpha modifiers, dark-mode scope
// and the custom breakpoint must survive a Tailwind upgrade together.
describe("compiled application theme", () => {
  it("preserves scoped colors, class-based dark mode and the 3xl breakpoint", async () => {
    const result = await postcss([tailwind({ base: process.cwd(), optimize: false })]).process(
      '@import "./app/globals.css";\n@source inline("bg-surface bg-neutral-50/60 dark:bg-neutral-900 3xl:flex");',
      { from: resolve("theme-regression.css") },
    );
    const declarations = (selector: string) => {
      const values: string[] = [];
      result.root.walkRules(selector, (rule) => {
        rule.walkDecls("background-color", (decl) => { values.push(decl.value); });
      });
      return values;
    };
    expect(declarations(".bg-surface")).toContain("rgb(var(--c-surface))");
    expect(declarations(".bg-neutral-50\\/60").join(" ")).toContain("color-mix");
    expect(declarations(".bg-neutral-50\\/60").join(" ")).toContain("--c-neutral-50");
    expect(result.css).toContain(".dark\\:bg-neutral-900:is(.dark *)");
    expect(result.css).not.toContain("prefers-color-scheme");
    expect(result.css).toContain("(width >= 2000px)");
  });
});
