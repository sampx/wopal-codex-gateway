import assert from 'node:assert/strict';
import test from 'node:test';
import { syncWopalModelConfigs } from '../src/wopal-models.js';

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
