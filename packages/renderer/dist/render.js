/**
 * DOM renderer for the agenui-studio AGenUI subset. Renders a Surface into a
 * container element, resolving data bindings against the data model and
 * dispatching component actions through a callback.
 */
import { isPathBinding, isTemplateChildren } from "./protocol";
/** Render (full redraw) the surface into the container. */
export function renderSurface(container, surface, options = {}) {
    container.innerHTML = "";
    container.classList.add("agenui-surface");
    const root = surface.root();
    if (!root) {
        const empty = document.createElement("div");
        empty.className = "agenui-empty";
        empty.textContent = "No components";
        container.appendChild(empty);
        return;
    }
    const ctx = { surface, options, dataContext: undefined };
    container.appendChild(renderComponent(root, ctx));
}
function renderComponent(entry, ctx) {
    switch (entry.component) {
        case "Text":
            return renderText(entry, ctx);
        case "Image":
            return renderImage(entry, ctx);
        case "Button":
            return renderButton(entry, ctx);
        case "Row":
            return renderFlex(entry, ctx, "row");
        case "Column":
            return renderFlex(entry, ctx, "column");
        case "Container": {
            // Tolerate non-canonical Container: map by flexDirection.
            const styles = (entry.styles ?? {});
            return renderFlex(entry, ctx, styles.flexDirection === "row" ? "row" : "column");
        }
        case "Card":
            return renderCard(entry, ctx);
        case "List":
            return renderList(entry, ctx);
        case "Tabs":
            return renderTabs(entry, ctx);
        case "Divider":
            return renderDivider(entry, ctx);
        case "TextField":
            return renderTextField(entry, ctx);
        case "CheckBox":
            return renderCheckBox(entry, ctx);
        case "Icon":
            return renderIcon(entry, ctx);
        case "Video":
            return renderVideo(entry, ctx);
        case "AudioPlayer":
            return renderAudioPlayer(entry, ctx);
        case "ChoicePicker":
            return renderChoicePicker(entry, ctx);
        case "Slider":
            return renderSlider(entry, ctx);
        case "DateTimeInput":
            return renderDateTimeInput(entry, ctx);
        case "Modal":
            return renderModal(entry, ctx);
        default:
            return renderFallback(entry, ctx);
    }
}
function withBase(el, entry, ctx) {
    el.dataset.componentId = entry.id;
    el.classList.add("agenui-component", `agenui-${entry.component.toLowerCase()}`);
    applyStyles(el, entry.styles, ctx.options);
    return el;
}
function renderText(entry, ctx) {
    const el = document.createElement("div");
    withBase(el, entry, ctx);
    el.textContent = String(resolve(entry.text, ctx) ?? "");
    // `variant` is the portable AGenUI typography contract.  `usageHint` was
    // used by an early Studio-only prototype, so keep it as a fallback only.
    // Ignoring `variant` made the renderer silently collapse h2/h3/caption into
    // the surface default and was particularly visible in compact list cards.
    const hint = String(entry.variant ?? entry.usageHint ?? "");
    if (hint) {
        el.classList.add(`agenui-hint-${hint}`);
    }
    return el;
}
function renderImage(entry, ctx) {
    const el = document.createElement("img");
    withBase(el, entry, ctx);
    el.src = String(resolve(entry.url ?? entry.src, ctx) ?? "");
    el.alt = String(resolve(entry.alt ?? entry.description, ctx) ?? "");
    el.loading = "lazy";
    return el;
}
function renderButton(entry, ctx) {
    const el = document.createElement("button");
    withBase(el, entry, ctx);
    el.type = "button";
    if (entry.variant === "borderless") {
        el.classList.add("agenui-button-borderless");
    }
    if (typeof entry.child === "string") {
        const child = ctx.surface.get(entry.child);
        if (child) {
            el.appendChild(renderComponent(child, ctx));
        }
    }
    else if (typeof entry.text === "string" || isPathBinding(entry.text)) {
        el.textContent = String(resolve(entry.text, ctx) ?? "");
    }
    else {
        el.textContent = String(resolve(entry.label, ctx) ?? "Button");
    }
    const action = entry.action;
    const eventName = typeof action?.event === "string"
        ? action.event
        : action?.event?.name ?? action?.name ?? "";
    el.addEventListener("click", () => {
        ctx.options.onAction?.(eventName || "tap", entry.id);
    });
    return el;
}
function renderFlex(entry, ctx, direction) {
    const el = document.createElement("div");
    withBase(el, entry, ctx);
    el.style.display = "flex";
    el.style.flexDirection = direction;
    const justify = String(entry.justify ?? "");
    const align = String(entry.align ?? "");
    if (justify) {
        el.style.justifyContent = flexAlignment(justify);
    }
    if (align) {
        el.style.alignItems = flexAlignment(align);
    }
    if (typeof entry.weight === "number") {
        el.style.flexGrow = String(entry.weight);
    }
    // Component properties establish the portable layout default. Presentation
    // styles are an explicit override and must win, otherwise an accepted
    // Workspace edit such as `styles.align-items` has no visual effect.
    applyStyles(el, entry.styles, ctx.options);
    appendChildren(el, entry, ctx);
    return el;
}
function renderCard(entry, ctx) {
    const el = document.createElement("div");
    withBase(el, entry, ctx);
    el.classList.add("agenui-card-shadow");
    appendChildren(el, entry, ctx);
    return el;
}
function renderList(entry, ctx) {
    const el = document.createElement("div");
    withBase(el, entry, ctx);
    el.classList.add("agenui-list");
    const children = entry.children;
    if (!isTemplateChildren(children)) {
        appendChildren(el, entry, ctx);
        return el;
    }
    const template = ctx.surface.get(children.componentId);
    if (!template) {
        return el;
    }
    const items = resolvePathValue(ctx.surface.data, children.path ?? "");
    if (!Array.isArray(items)) {
        return el;
    }
    for (const item of items) {
        const itemCtx = {
            ...ctx,
            dataContext: item ?? {},
        };
        el.appendChild(renderComponent(template, itemCtx));
    }
    // A template commonly carries its own item separator. A trailing separator
    // after the final item is almost never intended and produces a duplicate
    // line when the card also owns its footer divider. This is structural list
    // behavior, not a domain-specific layout rule: only a direct final Divider
    // in the repeated template is suppressed.
    if (items.length > 0) {
        removeTemplateTrailingDivider(el.lastElementChild, template, ctx.surface);
    }
    return el;
}
function removeTemplateTrailingDivider(rendered, template, surface) {
    if (!rendered || !Array.isArray(template.children) || template.children.length === 0) {
        return;
    }
    const lastChildID = template.children[template.children.length - 1];
    if (typeof lastChildID !== "string" || surface.get(lastChildID)?.component !== "Divider") {
        return;
    }
    const divider = rendered.querySelector(`:scope > [data-component-id="${cssSelectorValue(lastChildID)}"]`);
    divider?.remove();
}
function cssSelectorValue(value) {
    return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}
function renderTabs(entry, ctx) {
    const wrap = document.createElement("div");
    withBase(wrap, entry, ctx);
    const tabs = Array.isArray(entry.tabs) ? entry.tabs : [];
    const headers = document.createElement("div");
    const panels = document.createElement("div");
    const select = (index) => {
        Array.from(headers.children).forEach((child, i) => child.toggleAttribute("data-active", i === index));
        Array.from(panels.children).forEach((child, i) => child.hidden = i !== index);
    };
    tabs.forEach((raw, index) => {
        if (!raw || typeof raw !== "object")
            return;
        const tab = raw;
        const button = document.createElement("button");
        button.type = "button";
        button.textContent = String(resolve(tab.title, ctx) ?? "Tab");
        button.addEventListener("click", () => select(index));
        headers.appendChild(button);
        const panel = document.createElement("div");
        if (typeof tab.child === "string") {
            const child = ctx.surface.get(tab.child);
            if (child)
                panel.appendChild(renderComponent(child, ctx));
        }
        panels.appendChild(panel);
    });
    wrap.append(headers, panels);
    select(0);
    return wrap;
}
function renderDivider(entry, ctx) {
    const el = document.createElement("hr");
    withBase(el, entry, ctx);
    return el;
}
function renderTextField(entry, ctx) {
    const el = document.createElement("input");
    withBase(el, entry, ctx);
    el.type = "text";
    el.placeholder = String(resolve(entry.placeholder ?? entry.label, ctx) ?? "");
    return el;
}
function renderCheckBox(entry, ctx) {
    const wrap = document.createElement("label");
    withBase(wrap, entry, ctx);
    const box = document.createElement("input");
    box.type = "checkbox";
    wrap.appendChild(box);
    const label = document.createElement("span");
    label.textContent = String(resolve(entry.label ?? entry.text, ctx) ?? "");
    wrap.appendChild(label);
    return wrap;
}
function renderIcon(entry, ctx) {
    const el = document.createElement("span");
    withBase(el, entry, ctx);
    el.setAttribute("role", "img");
    el.textContent = iconGlyph(String(resolve(entry.name, ctx) ?? ""));
    el.setAttribute("aria-label", String(resolve(entry.name, ctx) ?? "icon"));
    return el;
}
function renderVideo(entry, ctx) {
    const el = document.createElement("video");
    withBase(el, entry, ctx);
    el.controls = true;
    el.src = String(resolve(entry.url, ctx) ?? "");
    return el;
}
function renderAudioPlayer(entry, ctx) {
    const wrap = document.createElement("div");
    withBase(wrap, entry, ctx);
    const description = String(resolve(entry.description, ctx) ?? "");
    if (description) {
        const label = document.createElement("div");
        label.textContent = description;
        wrap.appendChild(label);
    }
    const audio = document.createElement("audio");
    audio.controls = true;
    audio.src = String(resolve(entry.url, ctx) ?? "");
    wrap.appendChild(audio);
    return wrap;
}
function renderChoicePicker(entry, ctx) {
    const wrap = document.createElement("fieldset");
    withBase(wrap, entry, ctx);
    const label = String(resolve(entry.label, ctx) ?? "");
    if (label) {
        const legend = document.createElement("legend");
        legend.textContent = label;
        wrap.appendChild(legend);
    }
    const selected = new Set(asStringList(resolve(entry.value, ctx)));
    const multiple = entry.variant === "multipleSelection";
    const options = Array.isArray(entry.options) ? entry.options : [];
    for (const raw of options) {
        if (!raw || typeof raw !== "object")
            continue;
        const option = raw;
        const optionLabel = document.createElement("label");
        const input = document.createElement("input");
        input.type = multiple ? "checkbox" : "radio";
        input.name = entry.id;
        input.value = String(option.value ?? "");
        input.checked = selected.has(input.value);
        optionLabel.appendChild(input);
        optionLabel.append(String(resolve(option.label, ctx) ?? input.value));
        wrap.appendChild(optionLabel);
    }
    return wrap;
}
function renderSlider(entry, ctx) {
    const wrap = document.createElement("label");
    withBase(wrap, entry, ctx);
    const label = String(resolve(entry.label, ctx) ?? "");
    if (label)
        wrap.append(label);
    const input = document.createElement("input");
    input.type = "range";
    input.min = String(resolve(entry.min, ctx) ?? 0);
    input.max = String(resolve(entry.max, ctx) ?? 100);
    input.value = String(resolve(entry.value, ctx) ?? input.min);
    wrap.appendChild(input);
    return wrap;
}
function renderDateTimeInput(entry, ctx) {
    const wrap = document.createElement("label");
    withBase(wrap, entry, ctx);
    const label = String(resolve(entry.label, ctx) ?? "");
    if (label)
        wrap.append(label);
    const input = document.createElement("input");
    input.type = entry.enableDate && entry.enableTime ? "datetime-local" : entry.enableTime ? "time" : "date";
    input.value = String(resolve(entry.value, ctx) ?? "");
    input.min = String(resolve(entry.min, ctx) ?? "");
    input.max = String(resolve(entry.max, ctx) ?? "");
    wrap.appendChild(input);
    return wrap;
}
function renderModal(entry, ctx) {
    const wrap = document.createElement("div");
    withBase(wrap, entry, ctx);
    const trigger = typeof entry.trigger === "string" ? ctx.surface.get(entry.trigger) : undefined;
    const content = typeof entry.content === "string" ? ctx.surface.get(entry.content) : undefined;
    const dialog = document.createElement("dialog");
    if (content)
        dialog.appendChild(renderComponent(content, ctx));
    if (trigger) {
        const triggerEl = renderComponent(trigger, ctx);
        triggerEl.addEventListener("click", () => dialog.setAttribute("open", ""));
        wrap.appendChild(triggerEl);
    }
    wrap.appendChild(dialog);
    return wrap;
}
function asStringList(value) {
    return Array.isArray(value) ? value.map(String) : [];
}
function iconGlyph(name) {
    const icons = { add: "+", close: "×", check: "✓", play: "▶", pause: "Ⅱ", search: "⌕", menu: "☰", favorite: "♥", star: "★", warning: "⚠", info: "ℹ", error: "!" };
    return icons[name] ?? "●";
}
function renderFallback(entry, ctx) {
    const el = document.createElement("div");
    withBase(el, entry, ctx);
    el.classList.add("agenui-fallback");
    el.textContent = `[${entry.component}]`;
    appendChildren(el, entry, ctx);
    return el;
}
function appendChildren(el, entry, ctx) {
    const children = entry.children;
    if (!Array.isArray(children)) {
        if (typeof entry.child === "string") {
            const child = ctx.surface.get(entry.child);
            if (child) {
                el.appendChild(renderComponent(child, ctx));
            }
        }
        return;
    }
    for (const ref of children) {
        const id = typeof ref === "string" ? ref : ref?.componentId;
        if (!id) {
            continue;
        }
        const child = ctx.surface.get(id);
        if (child) {
            el.appendChild(renderComponent(child, ctx));
        }
    }
}
/** Resolve one property: path bindings (object form or "{/a/b}" template
 * strings tolerated) hit the data scope, scalars pass through. */
export function resolve(value, ctx) {
    if (isFunctionCall(value)) {
        return evaluateFunctionCall(value, ctx);
    }
    let path = null;
    if (isPathBinding(value)) {
        path = value.path;
    }
    else {
        path = templatePath(value);
    }
    if (path === null) {
        return value;
    }
    if (ctx.dataContext !== undefined) {
        const local = resolvePathValue(ctx.dataContext, path);
        if (local !== undefined) {
            return local;
        }
    }
    return resolvePathValue(ctx.surface.data, path);
}
/** "{/product/name}" / "{product.name}" template-string binding -> inner path. */
function templatePath(value) {
    if (typeof value !== "string") {
        return null;
    }
    const m = value.match(/^\{\/?([^}]+)\}$/);
    return m ? m[1] : null;
}
/** Read a dot/slash path ("items", "/items", "data.items") from an object tree. */
export function resolvePathValue(data, path) {
    if (!path) {
        return data;
    }
    const segments = path
        .replace(/^\//, "")
        .split(/[./]/)
        .filter(Boolean);
    let current = data;
    for (const seg of segments) {
        if (current && typeof current === "object" && !Array.isArray(current)) {
            current = current[seg];
        }
        else if (Array.isArray(current) && /^\d+$/.test(seg)) {
            current = current[Number(seg)];
        }
        else {
            return undefined;
        }
    }
    return current;
}
/** Map the catalog-declared AGenUI style extension onto CSS. */
export function applyStyles(el, styles, options = {}) {
    if (!styles || typeof styles !== "object") {
        return;
    }
    const s = styles;
    for (const [key, raw] of Object.entries(s)) {
        const value = cssValue(key, raw);
        if (value === undefined) {
            continue;
        }
        const canonicalKey = canonicalStyleKey(key);
        switch (canonicalKey) {
            case "width":
                el.style.width = value;
                break;
            case "height":
                el.style.height = value;
                break;
            case "padding":
                el.style.padding = value;
                break;
            case "margin":
                el.style.margin = value;
                break;
            case "gap":
                el.style.gap = value;
                break;
            case "borderRadius":
                el.style.borderRadius = value;
                break;
            case "border":
                el.style.border = value;
                break;
            case "borderWidth":
                el.style.borderWidth = value;
                break;
            case "borderColor":
                el.style.borderColor = value;
                break;
            case "borderStyle":
                el.style.borderStyle = value;
                break;
            case "backgroundColor":
                el.style.backgroundColor = value;
                break;
            case "color":
                el.style.color = value;
                break;
            case "fontSize":
                el.style.fontSize = value;
                break;
            case "fontWeight":
                el.style.fontWeight = String(raw);
                break;
            case "justifyContent":
                el.style.justifyContent = String(raw);
                break;
            case "alignItems":
                el.style.alignItems = String(raw);
                break;
            case "alignSelf":
                el.style.alignSelf = value;
                break;
            case "flex":
                el.style.flex = String(raw);
                break;
            case "lineHeight":
                el.style.lineHeight = value;
                break;
            case "textDecoration":
                el.style.textDecoration = value;
                break;
            case "textOverflow":
                el.style.textOverflow = value;
                break;
            case "whiteSpace":
                el.style.whiteSpace = value;
                break;
            case "overflow":
                el.style.overflow = value;
                break;
            case "marginTop":
                el.style.marginTop = value;
                break;
            case "marginRight":
                el.style.marginRight = value;
                break;
            case "marginBottom":
                el.style.marginBottom = value;
                break;
            case "marginLeft":
                el.style.marginLeft = value;
                break;
            case "paddingTop":
                el.style.paddingTop = value;
                break;
            case "paddingRight":
                el.style.paddingRight = value;
                break;
            case "paddingBottom":
                el.style.paddingBottom = value;
                break;
            case "paddingLeft":
                el.style.paddingLeft = value;
                break;
            case "minWidth":
                el.style.minWidth = value;
                break;
            case "maxWidth":
                el.style.maxWidth = value;
                break;
            case "minHeight":
                el.style.minHeight = value;
                break;
            case "maxHeight":
                el.style.maxHeight = value;
                break;
            case "lineClamp":
                el.style.display = "-webkit-box";
                el.style.webkitBoxOrient = "vertical";
                el.style.webkitLineClamp = value;
                el.style.overflow = "hidden";
                break;
            case "opacity":
                el.style.opacity = value;
                break;
            case "flexGrow":
                el.style.flexGrow = value;
                break;
            case "flexShrink":
                el.style.flexShrink = value;
                break;
            default:
                break;
        }
    }
}
const styleAliases = {
    "border-radius": "borderRadius",
    "border-width": "borderWidth",
    "border-color": "borderColor",
    "border-style": "borderStyle",
    "background-color": "backgroundColor",
    "font-size": "fontSize",
    "font-weight": "fontWeight",
    "line-height": "lineHeight",
    "text-decoration": "textDecoration",
    "text-overflow": "textOverflow",
    "white-space": "whiteSpace",
    "margin-top": "marginTop",
    "margin-right": "marginRight",
    "margin-bottom": "marginBottom",
    "margin-left": "marginLeft",
    "padding-top": "paddingTop",
    "padding-right": "paddingRight",
    "padding-bottom": "paddingBottom",
    "padding-left": "paddingLeft",
    "min-width": "minWidth",
    "max-width": "maxWidth",
    "min-height": "minHeight",
    "max-height": "maxHeight",
    "line-clamp": "lineClamp",
    "justify-content": "justifyContent",
    "align-items": "alignItems",
    "align-self": "alignSelf",
    "flex-grow": "flexGrow",
    "flex-shrink": "flexShrink",
};
function canonicalStyleKey(key) {
    return styleAliases[key] ?? key;
}
function cssValue(key, raw) {
    const evaluated = styleFunctionValue(raw);
    if (evaluated === undefined || evaluated === null) {
        return undefined;
    }
    if (typeof evaluated === "number") {
        return unitlessStyleKeys.has(canonicalStyleKey(key)) ? String(evaluated) : `${evaluated}px`;
    }
    if (typeof evaluated === "string") {
        return evaluated;
    }
    return undefined;
}
const unitlessStyleKeys = new Set(["flex", "fontWeight", "lineHeight", "opacity", "flexGrow", "flexShrink"]);
function styleFunctionValue(raw) {
    if (!isFunctionCall(raw)) {
        return raw;
    }
    return undefined;
}
function isFunctionCall(value) {
    return Boolean(value && typeof value === "object" && typeof value.call === "string");
}
function evaluateFunctionCall(call, ctx) {
    const args = call.args ?? {};
    const resolveArg = (name) => resolve(args[name], ctx);
    switch (call.call) {
        case "formatString": {
            const template = String(resolveArg("value") ?? resolveArg("template") ?? "");
            return template.replace(/\\\$\{|\$\{([^{}]+)\}/g, (match, expression) => {
                if (match.startsWith("\\")) {
                    return "${";
                }
                const value = resolve({ path: String(expression ?? "").trim() }, ctx);
                return value === undefined || value === null ? "" : String(value);
            });
        }
        case "formatNumber": {
            const value = Number(resolveArg("value"));
            return Number.isFinite(value) ? new Intl.NumberFormat().format(value) : "";
        }
        case "formatCurrency": {
            const value = Number(resolveArg("value"));
            const currency = String(resolveArg("currency") ?? "CNY");
            return Number.isFinite(value)
                ? new Intl.NumberFormat("zh-CN", { style: "currency", currency }).format(value)
                : "";
        }
        case "formatDate": {
            const value = resolveArg("value");
            const date = new Date(String(value ?? ""));
            return Number.isNaN(date.getTime()) ? "" : date.toLocaleDateString("zh-CN");
        }
        default:
            return "";
    }
}
function flexAlignment(value) {
    if (value === "spaceBetween")
        return "space-between";
    if (value === "spaceAround")
        return "space-around";
    if (value === "spaceEvenly")
        return "space-evenly";
    if (value === "start")
        return "flex-start";
    if (value === "end")
        return "flex-end";
    return value;
}
