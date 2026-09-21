/**
 * DOM renderer for the agenui-studio AGenUI subset. Renders a Surface into a
 * container element, resolving data bindings against the data model and
 * dispatching component actions through a callback.
 */
import type { Surface } from "./surface";
export interface RenderOptions {
    /** Receives component action events (e.g. button taps). */
    onAction?: (event: string, componentId: string) => void;
}
/** Render (full redraw) the surface into the container. */
export declare function renderSurface(container: HTMLElement, surface: Surface, options?: RenderOptions): void;
interface RenderContext {
    surface: Surface;
    options: RenderOptions;
    /** Current list-item data scope; undefined means card scope. */
    dataContext: Record<string, unknown> | undefined;
}
/** Resolve one property: path bindings (object form or "{/a/b}" template
 * strings tolerated) hit the data scope, scalars pass through. */
export declare function resolve(value: unknown, ctx: RenderContext): unknown;
/** Read a dot/slash path ("items", "/items", "data.items") from an object tree. */
export declare function resolvePathValue(data: unknown, path: string): unknown;
/** Map the catalog-declared AGenUI style extension onto CSS. */
export declare function applyStyles(el: HTMLElement, styles: unknown, options?: RenderOptions): void;
export {};
