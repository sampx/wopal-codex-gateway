import { readFile, writeFile } from 'node:fs/promises';
import type { ExternalModelConfig } from './types.js';

export interface ModelCatalog { models: Record<string, unknown>[] }

const reasoningLevels = [
  { effort: 'none', description: 'Turn reasoning off' },
  { effort: 'low', description: 'Fast responses with lighter reasoning' },
  { effort: 'medium', description: 'Balanced reasoning depth' },
  { effort: 'high', description: 'Greater reasoning depth for complex work' },
];

function externalEntry(namespace: 'wopal-ai' | 'ollama', model: ExternalModelConfig, priority: number): Record<string, unknown> {
  const contextWindow = model.contextWindow ?? 262_144;
  const supportsReasoning = model.reasoning !== false;
  const levels = !supportsReasoning ? [reasoningLevels[0]] : namespace === 'ollama' ? [reasoningLevels[0], reasoningLevels[3]] : reasoningLevels;
  return {
    additional_speed_tiers: [],
    apply_patch_tool_type: null,
    auto_compact_token_limit: null,
    availability_nux: null,
    base_instructions: 'You are Codex, a coding agent. You and the user share the same workspace and collaborate to achieve the user\'s goals.',
    context_window: contextWindow,
    default_reasoning_level: supportsReasoning ? 'high' : 'none',
    default_reasoning_summary: 'auto',
    default_service_tier: null,
    default_verbosity: null,
    description: namespace === 'wopal-ai' ? 'Wopal AI (New API) model' : 'Ollama model',
    display_name: `${namespace === 'wopal-ai' ? 'Wopal AI' : 'Ollama'} · ${model.name}`,
    effective_context_window_percent: 95,
    experimental_supported_tools: [],
    include_apps_usage_instructions: true,
    include_plugin_usage_instructions: true,
    include_skills_usage_instructions: true,
    input_modalities: model.vision ? ['text', 'image'] : ['text'],
    max_context_window: contextWindow,
    model_messages: null,
    priority,
    service_tiers: [],
    shell_type: 'unified_exec',
    slug: `${namespace}/${model.name}`,
    support_verbosity: false,
    supported_in_api: true,
    supported_reasoning_levels: levels,
    supports_image_detail_original: false,
    supports_parallel_tool_calls: true,
    supports_reasoning_summaries: false,
    supports_reasoning_summary_parameter: false,
    supports_search_tool: false,
    truncation_policy: { limit: 10_000, mode: 'tokens' },
    upgrade: null,
    visibility: 'list',
    web_search_tool_type: 'text',
  };
}

export function buildCatalog(nativeCatalog: ModelCatalog, wopalModels: ExternalModelConfig[], ollamaModels: ExternalModelConfig[]): ModelCatalog {
  const external = [
    ...wopalModels.map((model, index) => externalEntry('wopal-ai', model, -200 + index)),
    ...ollamaModels.map((model, index) => externalEntry('ollama', model, -100 + index)),
  ];
  const native = nativeCatalog.models.map((model) => ({ ...model, supported_in_api: false }));
  return { models: [...external, ...native] };
}

export async function loadNativeCatalog(path: string): Promise<ModelCatalog> {
  const parsed = JSON.parse(await readFile(path, 'utf8')) as { models?: Record<string, unknown>[] };
  if (!Array.isArray(parsed.models) || parsed.models.length === 0) throw new Error(`Codex model catalog is empty: ${path}`);
  return { models: parsed.models };
}

export async function discoverOllamaModels(baseUrl: string): Promise<ExternalModelConfig[]> {
  const response = await fetch(new URL('/api/tags', ensureTrailingSlash(baseUrl)));
  if (!response.ok) throw new Error(`Ollama /api/tags returned ${response.status}`);
  const payload = await response.json() as { models?: Array<{ name?: string; details?: { context_length?: number; family?: string }; capabilities?: string[] }> };
  return (payload.models ?? []).flatMap((model) => {
    if (!model.name) return [];
    const capabilities = model.capabilities ?? [];
    const family = model.details?.family?.toLowerCase() ?? '';
    if (!capabilities.includes('completion') || capabilities.includes('embedding') || family.includes('bert') || /(?:embed|rerank)/i.test(model.name)) return [];
    return [{
      name: model.name,
      contextWindow: model.details?.context_length ?? 262_144,
      vision: capabilities.includes('vision'),
      reasoning: capabilities.includes('thinking'),
    }];
  });
}

export async function writeCatalog(path: string, catalog: ModelCatalog): Promise<void> {
  await writeFile(path, `${JSON.stringify(catalog, null, 2)}\n`, { mode: 0o600 });
}

function ensureTrailingSlash(value: string): string {
  return value.endsWith('/') ? value : `${value}/`;
}
