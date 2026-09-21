/**
 * AGenUI v0.9 surface messages consumed by the agenui-studio web renderer.
 * Messages arrive as an ordered array; each message creates the surface,
 * upserts components, or updates the data model.
 */
export interface CreateSurface {
    surfaceId: string;
    catalogId?: string;
}
export interface UpdateComponents {
    surfaceId: string;
    components: ComponentEntry[];
}
export interface UpdateDataModel {
    surfaceId: string;
    /** JSON Pointer destination; omitted or "/" replaces the root model. */
    path?: string;
    /** Canonical AGenUI v0.9 payload. Omission removes the value at path. */
    value?: unknown;
}
export interface DeleteComponents {
    surfaceId: string;
    componentIds: string[];
}
export interface AGenUIMessage {
    version?: string;
    createSurface?: CreateSurface;
    updateComponents?: UpdateComponents;
    updateDataModel?: UpdateDataModel;
    deleteComponents?: DeleteComponents;
}
/**
 * One component entry. The flat AGenUI v0.9 shape carries the component type
 * in `component` and all properties (text, url, children, styles, action,
 * usageHint, ...) as sibling keys.
 */
export interface ComponentEntry {
    id: string;
    component: string;
    [key: string]: unknown;
}
/** A property value bound to the data model: {"path": "title"}. */
export interface PathBinding {
    path: string;
}
export declare function isPathBinding(value: unknown): value is PathBinding;
/** List children declaration: {"componentId": "...", "path": "/items"}. */
export interface TemplateChildren {
    componentId: string;
    /** Canonical AGenUI v0.9 list path. */
    path?: string;
}
export declare function isTemplateChildren(value: unknown): value is TemplateChildren;
