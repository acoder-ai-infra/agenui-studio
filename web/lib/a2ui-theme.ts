import {
  baseComponentSpecConfig,
  type A2uiTheme,
  type ColorScheme,
} from '@agenui/react';
import { defaultTheme } from './a2ui-default-theme';
import { getA2uiTokenMap } from './a2ui-token-map';

type BackendDesignToken = {
  type: string;
  light: string;
  dark: string;
};

export type RuntimeComponentStrategy = 'mergeDefaultTheme' | 'baseWhenBackend';

type BuildRuntimeA2uiThemeOptions = {
  messages?: Record<string, unknown>[];
  colorScheme?: ColorScheme;
  isMobile?: boolean;
  rootFontSize?: number;
  componentStrategy?: RuntimeComponentStrategy;
};

function extractSurfaceTheme(
  messages?: Record<string, unknown>[],
): Record<string, unknown> | undefined {
  const createSurfaceMessage = messages?.find((message) => 'createSurface' in message);
  if (!createSurfaceMessage) {
    return undefined;
  }
  const createSurface = (createSurfaceMessage.createSurface ?? {}) as Record<string, unknown>;
  const surfaceTheme = createSurface.theme;
  return surfaceTheme && typeof surfaceTheme === 'object'
    ? (surfaceTheme as Record<string, unknown>)
    : undefined;
}

function extractThemeParts(surfaceTheme?: Record<string, unknown>) {
  const properties =
    surfaceTheme?.properties && typeof surfaceTheme.properties === 'object'
      ? (surfaceTheme.properties as Record<string, unknown>)
      : {};
  const backendComponents =
    properties.components && typeof properties.components === 'object'
      ? (properties.components as Record<string, unknown>)
      : properties.spec && typeof properties.spec === 'object'
        ? (properties.spec as Record<string, unknown>)
        : {};
  const backendDesignTokens =
    properties.designTokens && typeof properties.designTokens === 'object'
      ? (properties.designTokens as Record<string, BackendDesignToken>)
      : {};
  return { backendComponents, backendDesignTokens };
}

/** 与 factory `buildRuntimeA2uiTheme` 同一套策略。 */
export function buildRuntimeA2uiTheme({
  messages,
  colorScheme = 'light',
  isMobile = false,
  rootFontSize = 100,
  componentStrategy = 'mergeDefaultTheme',
}: BuildRuntimeA2uiThemeOptions = {}): A2uiTheme {
  const surfaceTheme = extractSurfaceTheme(messages);
  const { backendComponents, backendDesignTokens } = extractThemeParts(surfaceTheme);

  const components =
    componentStrategy === 'baseWhenBackend'
      ? Object.keys(backendComponents).length > 0
        ? ({
            ...baseComponentSpecConfig,
            ...backendComponents,
          } as A2uiTheme['components'])
        : undefined
      : ({
          ...defaultTheme.components,
          ...backendComponents,
        } as A2uiTheme['components']);

  const tokenMap = { ...getA2uiTokenMap(colorScheme) };

  return {
    ...(components ? { components } : {}),
    designTokens: {
      ...defaultTheme.designTokens,
      ...backendDesignTokens,
    } as A2uiTheme['designTokens'],
    tokenMap,
    resolveTokenRegistry: { ...tokenMap },
    colorScheme,
    surfaceTheme,
    isMobile,
    rootFontSize,
    pointScale: 0.5,
  };
}
