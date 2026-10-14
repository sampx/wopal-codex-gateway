import type { ExternalModelConfig } from './types.js';

export interface WopalModelSyncResult {
  models: ExternalModelConfig[];
  skipped: string[];
  excluded: string[];
}

export function syncWopalModelConfigs(
  current: ExternalModelConfig[],
  availableIds: string[],
  excludedNames: string[] = [],
): WopalModelSyncResult {
  const existing = new Map(current.map((model) => [model.name, model]));
  const seen = new Set<string>();
  const models: ExternalModelConfig[] = [];
  const skipped: string[] = [];
  const excluded: string[] = [];
  const excludedSet = new Set(excludedNames.map(normalizeWopalModelName).filter(Boolean));

  for (const rawId of availableIds) {
    const name = rawId.trim();
    if (!name || seen.has(name)) continue;
    seen.add(name);
    if (excludedSet.has(name)) {
      excluded.push(name);
      continue;
    }
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

  return { models, skipped, excluded };
}

function isCodexUsableModel(name: string): boolean {
  return !/(?:embed(?:ding)?|rerank)/i.test(name);
}

export interface AddWopalModelsResult {
  models: ExternalModelConfig[];
  added: string[];
  existing: string[];
}

export function addWopalModelConfigs(
  current: ExternalModelConfig[],
  names: string[],
  defaults: Omit<ExternalModelConfig, 'name'> = {},
): AddWopalModelsResult {
  const models = current.map((model) => ({ ...model }));
  const known = new Set(models.map((model) => model.name));
  const added: string[] = [];
  const existing: string[] = [];

  for (const rawName of names) {
    const name = normalizeWopalModelName(rawName);
    if (!name) continue;
    if (known.has(name)) {
      if (!existing.includes(name)) existing.push(name);
      continue;
    }
    known.add(name);
    models.push({ name, ...defaults });
    added.push(name);
  }

  return { models, added, existing };
}

export interface RemoveWopalModelsResult {
  models: ExternalModelConfig[];
  removed: string[];
  missing: string[];
}

export function removeWopalModelConfigs(
  current: ExternalModelConfig[],
  names: string[],
): RemoveWopalModelsResult {
  const requested = [...new Set(names.map(normalizeWopalModelName).filter(Boolean))];
  const existing = new Set(current.map((model) => model.name));
  const remove = new Set(requested.filter((name) => existing.has(name)));
  return {
    models: current.filter((model) => !remove.has(model.name)),
    removed: requested.filter((name) => remove.has(name)),
    missing: requested.filter((name) => !existing.has(name)),
  };
}

function normalizeWopalModelName(rawName: string): string {
  const name = rawName.trim();
  return name.startsWith('wopal-ai/') ? name.slice('wopal-ai/'.length) : name;
}
