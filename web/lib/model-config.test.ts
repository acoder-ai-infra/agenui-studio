import { describe, expect, it } from 'vitest';

import {
  hasModelConnectionChanged,
  modelConfigFromResponse,
  requiresModelAPIKey,
  switchModelProtocol,
  type ModelConfig,
} from './model-config';

const savedAnthropic: ModelConfig = {
  protocol: 'anthropic',
  baseUrl: 'https://gateway.example/anthropic',
  model: 'claude-example',
  apiKeyMasked: '****1234',
  configured: true,
};

describe('model protocol configuration', () => {
  it('loads the explicit saved protocol', () => {
    expect(modelConfigFromResponse({
      protocol: 'anthropic',
      baseUrl: savedAnthropic.baseUrl,
      model: savedAnthropic.model,
      apiKeyMasked: savedAnthropic.apiKeyMasked,
      configured: true,
    })).toEqual(savedAnthropic);
  });

  it('does not reuse Anthropic fields or credentials for an OpenAI draft', () => {
    const openAI = switchModelProtocol(savedAnthropic, 'openai_compatible');

    expect(openAI).toEqual({
      protocol: 'openai_compatible',
      baseUrl: '',
      model: '',
      apiKey: '',
      apiKeyMasked: '',
      configured: false,
    });
    expect(requiresModelAPIKey(openAI, savedAnthropic)).toBe(true);
  });

  it('restores the saved Anthropic fields when switching back', () => {
    const anthropic = switchModelProtocol(savedAnthropic, 'anthropic');

    expect(anthropic).toEqual({ ...savedAnthropic, apiKey: '' });
    expect(requiresModelAPIKey(anthropic, savedAnthropic)).toBe(false);
  });

  it('detects changes that may benefit from restarting the Agent service', () => {
    expect(hasModelConnectionChanged({ ...savedAnthropic, apiKey: '' }, savedAnthropic)).toBe(false);
    expect(hasModelConnectionChanged({ ...savedAnthropic, model: 'claude-next' }, savedAnthropic)).toBe(true);
    expect(hasModelConnectionChanged({ ...savedAnthropic, apiKey: 'sk-replaced' }, savedAnthropic)).toBe(true);
    expect(hasModelConnectionChanged({ ...savedAnthropic, baseUrl: ` ${savedAnthropic.baseUrl} ` }, savedAnthropic)).toBe(false);
  });
});
