export type ModelProtocol = 'openai_compatible' | 'anthropic';

export interface ModelConfig {
  baseUrl?: string;
  model?: string;
  apiKey?: string;
  apiKeyMasked?: string;
  configured?: boolean;
  protocol: ModelProtocol;
}

export function modelConfigFromResponse(body: Record<string, unknown>): ModelConfig {
  return {
    baseUrl: typeof body.baseUrl === 'string' ? body.baseUrl : '',
    model: typeof body.model === 'string' ? body.model : '',
    protocol: body.protocol === 'anthropic' ? 'anthropic' : 'openai_compatible',
    apiKeyMasked: typeof body.apiKeyMasked === 'string' ? body.apiKeyMasked : '',
    configured: body.configured === true,
  };
}

export function switchModelProtocol(
  saved: ModelConfig | null,
  protocol: ModelProtocol,
): ModelConfig {
  if (saved?.protocol === protocol) {
    return { ...saved, apiKey: '' };
  }
  return {
    protocol,
    baseUrl: '',
    model: '',
    apiKey: '',
    apiKeyMasked: '',
    configured: false,
  };
}

export function requiresModelAPIKey(value: ModelConfig, saved: ModelConfig | null): boolean {
  return !saved?.configured || value.protocol !== saved.protocol;
}

export function hasModelConnectionChanged(value: ModelConfig, saved: ModelConfig | null): boolean {
  if (!saved) return true;
  return value.protocol !== saved.protocol
    || (value.baseUrl || '').trim() !== (saved.baseUrl || '').trim()
    || (value.model || '').trim() !== (saved.model || '').trim()
    || Boolean(value.apiKey?.trim());
}
