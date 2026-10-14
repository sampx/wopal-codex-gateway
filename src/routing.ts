export type Provider = 'wopal-ai' | 'ollama' | 'chatgpt' | 'openai';

export interface RouteDecision {
  provider: Provider;
  upstreamModel: string;
}

interface HeaderReader {
  get(name: string): string | null;
}

export function routeModel(model: string, headers: HeaderReader): RouteDecision {
  if (model.startsWith('wopal-ai/')) {
    return { provider: 'wopal-ai', upstreamModel: model.slice('wopal-ai/'.length) };
  }
  if (model.startsWith('ollama/')) {
    return { provider: 'ollama', upstreamModel: model.slice('ollama/'.length) };
  }
  if (model.includes('/')) {
    const namespace = model.slice(0, model.indexOf('/')) || 'empty';
    throw new Error(`unsupported model namespace: ${namespace}`);
  }
  const accountId = headers.get('ChatGPT-Account-ID')?.trim();
  return {
    provider: accountId ? 'chatgpt' : 'openai',
    upstreamModel: model,
  };
}
