/**
 * Surface holds the applied AGenUI state: the component table and the data
 * model. Applying messages is incremental and idempotent per component id,
 * which keeps streaming re-renders cheap and predictable.
 */
import type { AGenUIMessage, ComponentEntry } from "./protocol";
export declare class Surface {
    surfaceId: string;
    catalogId: string;
    private components;
    data: Record<string, unknown>;
    /** Apply a batch of protocol messages in order. */
    apply(messages: AGenUIMessage[]): void;
    /** Replace state entirely (used when a fresh package is loaded). */
    reset(messages: AGenUIMessage[]): void;
    get(id: string): ComponentEntry | undefined;
    allComponents(): ComponentEntry[];
    /**
     * Resolve the render root: an entry explicitly named "root", otherwise the
     * first component never referenced as a child.
     */
    root(): ComponentEntry | undefined;
}
