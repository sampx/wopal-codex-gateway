import assert from 'node:assert/strict';
import test from 'node:test';
import { buildCatalog } from '../src/catalog.js';

const nativeCatalog = {
  models: [{
    slug: 'gpt-6.1-sol', display_name: 'GPT-6.1-Sol', description: 'Official',
    default_reasoning_level: 'low', supported_reasoning_levels: [], shell_type: 'unified_exec',
    visibility: 'list', supported_in_api: true, priority: 1, context_window: 500000,
    max_context_window: 872000, effective_context_window_percent: 95,
    input_modalities: ['text','image'], supports_parallel_tool_calls: true
  }]
};

test('catalog merges official, wopal-ai and ollama with explicit namespaces', () => {
  const catalog = buildCatalog(nativeCatalog, [{ name: 'qwen3-coder', contextWindow: 262144 }], [{ name: 'glm-5.3:cloud', contextWindow: 1048576, vision: true }]);
  const bySlug = new Map(catalog.models.map((m: any) => [m.slug, m]));
  assert.equal(bySlug.get('wopal-ai/qwen3-coder').supported_in_api, true);
  assert.equal(bySlug.get('ollama/glm-5.3:cloud').input_modalities.includes('image'), true);
  assert.equal(bySlug.get('gpt-6.1-sol').supported_in_api, false);
});

test('Ollama discovery excludes embedding-only models', async (t) => {
  const http = await import('node:http');
  const { once } = await import('node:events');
  const { discoverOllamaModels } = await import('../src/catalog.js');
  const server = http.createServer((_req, res) => {
    res.setHeader('content-type', 'application/json');
    res.end(JSON.stringify({ models: [
      { name: 'coder:latest', details: { context_length: 131072 }, capabilities: ['completion', 'tools'] },
      { name: 'embed:latest', details: { context_length: 8192, family: 'nomic-bert' }, capabilities: ['embedding'] },
      { name: 'bge-reranker:latest', details: { context_length: 8192, family: 'bert' }, capabilities: ['completion'] },
    ] }));
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => server.close());
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('missing address');
  const models = await discoverOllamaModels(`http://127.0.0.1:${address.port}`);
  assert.deepEqual(models.map((model) => model.name), ['coder:latest']);
});

test('catalog can omit Ollama models when integration is disabled', () => {
  const catalog = buildCatalog(nativeCatalog, [{ name: 'qwen3-coder' }], []);
  assert.equal(catalog.models.some((model: any) => String(model.slug).startsWith('ollama/')), false);
});

test('bundled native catalog comes from Codex CLI instead of mutable models_cache', async (t) => {
  const { mkdtemp, writeFile, chmod } = await import('node:fs/promises');
  const { tmpdir } = await import('node:os');
  const { join } = await import('node:path');
  const { loadBundledNativeCatalog } = await import('../src/catalog.js');
  const dir = await mkdtemp(join(tmpdir(), 'wopal-codex-bundled-'));
  t.after(async () => { const { rm } = await import('node:fs/promises'); await rm(dir, { recursive: true, force: true }); });
  const fakeCodex = join(dir, 'codex');
  await writeFile(fakeCodex, '#!/bin/sh\nprintf \'%s\\n\' \'{"models":[{"slug":"gpt-test-official","display_name":"Official"}]}\'\n');
  await chmod(fakeCodex, 0o755);
  const catalog = await loadBundledNativeCatalog(fakeCodex);
  assert.deepEqual(catalog.models.map((model: any) => model.slug), ['gpt-test-official']);
});

test('catalog preserves hidden visibility for Codex internal official models', () => {
  const catalog = buildCatalog({ models: [
    { slug: 'gpt-6.1-sol', visibility: 'list', supported_in_api: true },
    { slug: 'codex-auto-review', visibility: 'hide', supported_in_api: true },
  ] }, [], []);
  const bySlug = new Map(catalog.models.map((m: any) => [m.slug, m]));
  assert.equal(bySlug.get('gpt-6.1-sol').visibility, 'list');
  assert.equal(bySlug.get('codex-auto-review').visibility, 'hide');
});
