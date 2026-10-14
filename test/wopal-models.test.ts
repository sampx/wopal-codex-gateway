import assert from 'node:assert/strict';
import test from 'node:test';
import { syncWopalModelConfigs, updateWopalModelConfigs } from '../src/wopal-models.js';

test('syncWopalModelConfigs imports all Codex-usable API models and skips embedding models', () => {
  const current = [
    { name: 'glm-5.3-flash-free', contextWindow: 524_288, vision: true, reasoning: false },
  ];
  const available = [
    'glm-5.3-flash-free',
    'deepseek-v4-flash-free',
    'nomic-embed-text-v2',
    'bge-reranker-v2-m3',
  ];

  const result = syncWopalModelConfigs(current, available);

  assert.deepEqual(result.models, [
    { name: 'glm-5.3-flash-free', contextWindow: 524_288, vision: true, reasoning: false },
    { name: 'deepseek-v4-flash-free', contextWindow: 262_144, vision: false, reasoning: true },
  ]);
  assert.deepEqual(result.skipped, ['nomic-embed-text-v2', 'bge-reranker-v2-m3']);
});

test('syncWopalModelConfigs de-duplicates model ids while preserving API order', () => {
  const result = syncWopalModelConfigs([], ['glm-5.3', 'glm-5.3', 'kimi-k3']);
  assert.deepEqual(result.models.map((model) => model.name), ['glm-5.3', 'kimi-k3']);
});

test('addWopalModelConfigs adds multiple models and preserves existing entries', async () => {
  const { addWopalModelConfigs } = await import('../src/wopal-models.js');
  const current = [
    { name: 'glm-5.3-flash-free', contextWindow: 524_288, vision: true, reasoning: false },
  ];
  const result = addWopalModelConfigs(current, ['glm-5.3-flash-free', 'deepseek-v4-flash-free', 'kimi-k3'], {
    contextWindow: 131_072,
    vision: false,
    reasoning: true,
  });

  assert.deepEqual(result.models, [
    { name: 'glm-5.3-flash-free', contextWindow: 524_288, vision: true, reasoning: false },
    { name: 'deepseek-v4-flash-free', contextWindow: 131_072, vision: false, reasoning: true },
    { name: 'kimi-k3', contextWindow: 131_072, vision: false, reasoning: true },
  ]);
  assert.deepEqual(result.added, ['deepseek-v4-flash-free', 'kimi-k3']);
  assert.deepEqual(result.existing, ['glm-5.3-flash-free']);
});

test('removeWopalModelConfigs removes multiple models and reports missing names', async () => {
  const { removeWopalModelConfigs } = await import('../src/wopal-models.js');
  const current = [
    { name: 'glm-5.3-flash-free' },
    { name: 'deepseek-v4-flash-free' },
    { name: 'kimi-k3' },
  ];
  const result = removeWopalModelConfigs(current, ['deepseek-v4-flash-free', 'missing-model']);

  assert.deepEqual(result.models.map((model) => model.name), ['glm-5.3-flash-free', 'kimi-k3']);
  assert.deepEqual(result.removed, ['deepseek-v4-flash-free']);
  assert.deepEqual(result.missing, ['missing-model']);
});

test('syncWopalModelConfigs keeps manually excluded models out of future syncs', () => {
  const current = [{ name: 'glm-5.3-flash-free' }];
  const result = syncWopalModelConfigs(current, ['glm-5.3-flash-free', 'broken-model', 'kimi-k3'], ['broken-model']);
  assert.deepEqual(result.models.map((model) => model.name), ['glm-5.3-flash-free', 'kimi-k3']);
  assert.deepEqual(result.excluded, ['broken-model']);
});


test('updateWopalModelConfigs overrides existing model metadata and sync preserves it', () => {
  const current = [
    { name: 'deepseek-v4.1-flash', contextWindow: 262_144, vision: false, reasoning: true },
    { name: 'kimi-k3', contextWindow: 262_144, vision: false, reasoning: true },
  ];

  const updated = updateWopalModelConfigs(current, ['wopal-ai/deepseek-v4.1-flash'], {
    contextWindow: 1_000_000,
  });

  assert.deepEqual(updated.updated, ['deepseek-v4.1-flash']);
  assert.deepEqual(updated.missing, []);
  assert.deepEqual(updated.models[0], {
    name: 'deepseek-v4.1-flash',
    contextWindow: 1_000_000,
    vision: false,
    reasoning: true,
  });
  assert.deepEqual(updated.models[1], current[1]);

  const synced = syncWopalModelConfigs(updated.models, ['deepseek-v4.1-flash', 'kimi-k3']);
  assert.equal(synced.models[0]?.contextWindow, 1_000_000);
});

test('updateWopalModelConfigs reports unknown models without creating them', () => {
  const current = [{ name: 'deepseek-v4.1-flash', contextWindow: 262_144 }];
  const updated = updateWopalModelConfigs(current, ['missing-model'], { contextWindow: 1_000_000 });

  assert.deepEqual(updated.updated, []);
  assert.deepEqual(updated.missing, ['missing-model']);
  assert.deepEqual(updated.models, current);
});
