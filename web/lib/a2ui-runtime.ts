import { MessageProcessor } from '@agenui/web-core';
import { ensureCreateSurface } from './a2ui-protocol';

type ActionHandler = (action: unknown) => Promise<void> | void;

type RuntimeSurfaceDescriptor = {
  id: string;
};

type RuntimeSurfaceStore = {
  surfacesMap: ReadonlyMap<string, RuntimeSurfaceDescriptor>;
  getSurface: (surfaceId: string) => unknown | null;
};

export type A2uiRuntimeProcessor = {
  processMessages: (messages: Record<string, unknown>[]) => void;
  model: RuntimeSurfaceStore;
};

type RuntimeMessageProcessorConstructor = new (
  catalogs: unknown[],
  onAction: ActionHandler,
) => A2uiRuntimeProcessor;

const RuntimeMessageProcessor = MessageProcessor as unknown as RuntimeMessageProcessorConstructor;

export function createA2uiRuntimeProcessor(
  catalogs: unknown[],
  messages: Record<string, unknown>[],
  onAction: ActionHandler = async () => {},
): A2uiRuntimeProcessor {
  const processor = new RuntimeMessageProcessor(catalogs, onAction);
  processor.processMessages(ensureCreateSurface(messages));
  return processor;
}

export function getA2uiSurfaceIds(processor: A2uiRuntimeProcessor | null): string[] {
  if (!processor) {
    return [];
  }
  return Array.from(processor.model.surfacesMap.values(), (surface) => surface.id);
}
