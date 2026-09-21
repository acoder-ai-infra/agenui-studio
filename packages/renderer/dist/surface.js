/**
 * Surface holds the applied AGenUI state: the component table and the data
 * model. Applying messages is incremental and idempotent per component id,
 * which keeps streaming re-renders cheap and predictable.
 */
export class Surface {
    constructor() {
        this.surfaceId = "";
        this.catalogId = "";
        this.components = new Map();
        this.data = {};
    }
    /** Apply a batch of protocol messages in order. */
    apply(messages) {
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
    reset(messages) {
        this.surfaceId = "";
        this.catalogId = "";
        this.components = new Map();
        this.data = {};
        this.apply(messages);
    }
    get(id) {
        return this.components.get(id);
    }
    allComponents() {
        return Array.from(this.components.values());
    }
    /**
     * Resolve the render root: an entry explicitly named "root", otherwise the
     * first component never referenced as a child.
     */
    root() {
        if (this.components.has("root")) {
            return this.components.get("root");
        }
        const referenced = new Set();
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
function applyDataModelUpdate(current, path, value) {
    const segments = jsonPointerSegments(path ?? "/");
    if (segments.length === 0) {
        return value && typeof value === "object" && !Array.isArray(value)
            ? { ...value }
            : {};
    }
    const root = structuredClone(current);
    let cursor = root;
    for (let i = 0; i < segments.length - 1; i += 1) {
        const segment = segments[i];
        const next = cursor[segment];
        if (!next || typeof next !== "object" || Array.isArray(next)) {
            cursor[segment] = {};
        }
        cursor = cursor[segment];
    }
    const leaf = segments[segments.length - 1];
    if (value === undefined) {
        delete cursor[leaf];
    }
    else {
        cursor[leaf] = value;
    }
    return root;
}
function jsonPointerSegments(path) {
    if (!path || path === "/") {
        return [];
    }
    return path
        .replace(/^\//, "")
        .split("/")
        .filter(Boolean)
        .map((segment) => segment.replace(/~1/g, "/").replace(/~0/g, "~"));
}
function collectChildIds(entry, out) {
    const children = entry.children;
    if (Array.isArray(children)) {
        for (const child of children) {
            if (typeof child === "string") {
                out.add(child);
            }
            else if (child && typeof child === "object" && typeof child.componentId === "string") {
                out.add(child.componentId);
            }
        }
    }
    else if (children && typeof children === "object" && typeof children.componentId === "string") {
        out.add(children.componentId);
    }
    if (typeof entry.child === "string") {
        out.add(entry.child);
    }
}
