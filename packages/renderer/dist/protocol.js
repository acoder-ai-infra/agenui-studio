/**
 * AGenUI v0.9 surface messages consumed by the agenui-studio web renderer.
 * Messages arrive as an ordered array; each message creates the surface,
 * upserts components, or updates the data model.
 */
export function isPathBinding(value) {
    return (typeof value === "object" &&
        value !== null &&
        typeof value.path === "string");
}
export function isTemplateChildren(value) {
    return (typeof value === "object" &&
        value !== null &&
        typeof value.componentId === "string");
}
