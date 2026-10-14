import type { ExternalModelConfig } from './types.js';

export interface WopalModelSyncResult {
  models: ExternalModelConfig[];
  skipped: string[];
}

export function syncWopalModelConfigs(
  current: ExternalModelConfig[],
  availableIds: string[],
): WopalModelSyncResult {
  const existing = new Map(current.map((model) => [model.name, model]));
  const seen = new Set<string>();
  const models: ExternalModelConfig[] = [];
  const skipped: string[] = [];

  for (const rawId of availableIds) {
    const name = rawId.trim();
    if (!name || seen.has(name)) continue;
    seen.add(name);
    if (!isCodexUsableModel(name)) {
      skipped.push(name);
      continue;
    }
    models.push(existing.get(name) ?? {
      name,
      contextWindow: 262_144,
      vision: false,
      reasoning: true,
    });
  }

  return { models, skipped };
}

function isCodexUsableModel(name: string): boolean {
  return !/(?:embed(?:ding)?|rerank)/i.test(name);
}
