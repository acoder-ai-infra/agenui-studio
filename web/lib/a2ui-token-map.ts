import type { ColorScheme } from '@agenui/react';
import tokenSource from '../token/token.json';

type TokenValue = {
  light?: string;
  dark?: string;
};

type TokenGroups = Record<string, Record<string, TokenValue>>;

const PURE_COLOR_PATTERN =
  /^(?:transparent|#[0-9a-fA-F]{6}(?:[0-9a-fA-F]{2})?|rgba?\(\s*[\d.\s,]+\))$/;

function isPureColorToken(value: string): boolean {
  return PURE_COLOR_PATTERN.test(value.trim());
}

function buildTokenMapForScheme(colorScheme: ColorScheme): Record<string, string> {
  const tokenGroups = tokenSource as TokenGroups;
  const flatTokenMap: Record<string, string> = {};

  for (const [groupName, tokens] of Object.entries(tokenGroups)) {
    if (groupName === 'gradient') {
      continue;
    }
    for (const [tokenName, tokenValue] of Object.entries(tokens)) {
      const resolvedValue = tokenValue[colorScheme];
      if (typeof resolvedValue !== 'string' || !isPureColorToken(resolvedValue)) {
        continue;
      }
      flatTokenMap[`@${tokenName}`] = resolvedValue;
    }
  }

  return flatTokenMap;
}

export const lightTokenMap = buildTokenMapForScheme('light');
export const darkTokenMap = buildTokenMapForScheme('dark');

export function getA2uiTokenMap(colorScheme: ColorScheme = 'light'): Record<string, string> {
  return colorScheme === 'dark' ? darkTokenMap : lightTokenMap;
}
