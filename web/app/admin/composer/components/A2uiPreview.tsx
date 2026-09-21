'use client';

import { useMemo } from 'react';
import {
  A2uiSurface,
  MarkdownContext,
  baseComponentSpecConfig,
  minimalCatalog,
  type A2uiTheme,
  type ColorScheme,
} from '@agenui/react';
import { mergedBasicCatalog } from '@/lib/a2ui-catalog';
import { createA2uiRuntimeProcessor, getA2uiSurfaceIds } from '@/lib/a2ui-runtime';
import { buildRuntimeA2uiTheme } from '@/lib/a2ui-theme';

async function renderMarkdown(markdown: string): Promise<string> {
  return markdown
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/\n/g, '<br />');
}

/**
 * 对齐 factory composer PreviewPanel：
 * messages 原样进 processor（catalog 归一在 runtime）；
 * catalog 用 mergedBasicCatalog；theme 用 baseWhenBackend + Card 底色覆盖。
 */
export function A2uiPreview({
  messages,
  colorScheme = 'light',
}: {
  messages: Record<string, unknown>[];
  colorScheme?: ColorScheme;
}) {
  const { processor, error } = useMemo(() => {
    if (!messages || messages.length === 0) {
      return { processor: null, error: null as string | null };
    }
    try {
      return {
        processor: createA2uiRuntimeProcessor(
          [minimalCatalog, mergedBasicCatalog],
          messages,
          async (action: unknown) => {
            console.log('A2UI Action:', action);
          },
        ),
        error: null,
      };
    } catch (cause: unknown) {
      return {
        processor: null,
        error: cause instanceof Error ? cause.message : '渲染失败',
      };
    }
  }, [messages]);

  const runtimeTheme = useMemo<A2uiTheme>(() => {
    const base = buildRuntimeA2uiTheme({
      messages,
      colorScheme,
      isMobile: false,
      rootFontSize: 100,
      componentStrategy: 'baseWhenBackend',
    });
    type ComponentSpec = {
      styles?: { default?: Record<string, unknown> } & Record<string, unknown>;
      [key: string]: unknown;
    };
    const components = (base.components ?? baseComponentSpecConfig) as Record<string, ComponentSpec>;
    const cardSpec = components.Card ?? {};
    const cardStyles = cardSpec.styles ?? {};
    const cardDefault = (cardStyles.default ?? {}) as Record<string, unknown>;
    return {
      ...base,
      components: {
        ...components,
        Card: {
          ...cardSpec,
          styles: {
            ...cardStyles,
            default: {
              ...cardDefault,
              'background-color': colorScheme === 'dark' ? '#2A2A2A' : '#FFFFFF',
            },
          },
        },
      } as A2uiTheme['components'],
    };
  }, [messages, colorScheme]);

  if (error) {
    return <p className="px-3 py-4 text-center text-[11px] text-red-500">渲染失败：{error}</p>;
  }
  if (!processor) {
    return null;
  }

  const surfaces = getA2uiSurfaceIds(processor);
  if (surfaces.length === 0) {
    return <p className="px-3 py-4 text-center text-[11px] text-neutral-400">未找到可渲染的 Surface</p>;
  }

  type SurfaceProp = React.ComponentProps<typeof A2uiSurface>['surface'];

  return (
    <div className="min-h-full w-full">
      <MarkdownContext.Provider value={renderMarkdown}>
        {surfaces.map((surfaceId) => {
          const surface = processor.model.getSurface(surfaceId);
          if (!surface) {
            return null;
          }
          return (
            <A2uiSurface
              key={surfaceId}
              surface={surface as SurfaceProp}
              theme={runtimeTheme}
            />
          );
        })}
      </MarkdownContext.Provider>
    </div>
  );
}
