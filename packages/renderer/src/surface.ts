/**
 * Surface holds the applied AGenUI state: the component table and the data
 * model. Applying messages is incremental and idempotent per component id,
 * which keeps streaming re-renders cheap and predictable.
 */

import type { AGenUIMessage, ComponentEntry } from "./protocol";

export class Surface {
  surfaceId = "";
  catalogId = "";
  private components = new Map<string, ComponentEntry>();
  data: Record<string, unknown> = {};

  /** Apply a batch of protocol messages in order. */
  apply(messages: AGenUIMessage[]): void {
    for (const message of messages) {
      if (message.createSurface) {
        this.surfaceId = message.createSurface.surfaceId;
        this.catalogId = message.createSurface.catalogId ?? "";
      }
      if (message.updateComponents) {
        for (const entry of message.updateComponents.components) {
          if (!entry?.id) {
            continue;
          }
          this.components.set(entry.id, entry);
        }
      }
      if (message.updateDataModel) {
        const update = message.updateDataModel;
        this.data = applyDataModelUpdate(this.data, update.path, update.value);
      }
      if (message.deleteComponents) {
        for (const id of message.deleteComponents.componentIds) {
          this.components.delete(id);
        }
      }
    }
  }

  /** Replace state entirely (used when a fresh package is loaded). */
  reset(messages: AGenUIMessage[]): void {
    this.surfaceId = "";
    this.catalogId = "";
    this.components = new Map();
    this.data = {};
    this.apply(messages);
  }

  get(id: string): ComponentEntry | undefined {
    return this.components.get(id);
  }

  allComponents(): ComponentEntry[] {
    return Array.from(this.components.values());
  }

  /**
   * Resolve the render root: an entry explicitly named "root", otherwise the
   * first component never referenced as a child.
   */
  root(): ComponentEntry | undefined {
    if (this.components.has("root")) {
      return this.components.get("root");
    }
    const referenced = new Set<string>();
    for (const entry of this.components.values()) {
      collectChildIds(entry, referenced);
    }
    for (const entry of this.components.values()) {
      if (!referenced.has(entry.id)) {
        return entry;
      }
    }
    return undefined;
  }
}

/** Apply the canonical AGenUI v0.9 updateDataModel JSON Pointer operation. */
function applyDataModelUpdate(
  current: Record<string, unknown>,
  path: string | undefined,
  value: unknown,
): Record<string, unknown> {
  const segments = jsonPointerSegments(path ?? "/");
  if (segments.length === 0) {
    return value && typeof value === "object" && !Array.isArray(value)
      ? { ...(value as Record<string, unknown>) }
      : {};
  }

  const root = structuredClone(current);
  let cursor: Record<string, unknown> = root;
  for (let i = 0; i < segments.length - 1; i += 1) {
    const segment = segments[i];
    const next = cursor[segment];
    if (!next || typeof next !== "object" || Array.isArray(next)) {
      cursor[segment] = {};
    }
    cursor = cursor[segment] as Record<string, unknown>;
  }
  const leaf = segments[segments.length - 1];
  if (value === undefined) {
    delete cursor[leaf];
  } else {
    cursor[leaf] = value;
  }
  return root;
}

function jsonPointerSegments(path: string): string[] {
  if (!path || path === "/") {
    return [];
  }
  return path
    .replace(/^\//, "")
    .split("/")
    .filter(Boolean)
    .map((segment) => segment.replace(/~1/g, "/").replace(/~0/g, "~"));
}

function collectChildIds(entry: ComponentEntry, out: Set<string>): void {
  const children = entry.children;
  if (Array.isArray(children)) {
    for (const child of children) {
      if (typeof child === "string") {
        out.add(child);
      } else if (child && typeof child === "object" && typeof (child as { componentId?: unknown }).componentId === "string") {
        out.add((child as { componentId: string }).componentId);
      }
    }
  } else if (children && typeof children === "object" && typeof (children as { componentId?: unknown }).componentId === "string") {
    out.add((children as { componentId: string }).componentId);
  }
  if (typeof entry.child === "string") {
    out.add(entry.child);
  }
}
