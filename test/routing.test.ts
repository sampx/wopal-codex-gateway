import assert from 'node:assert/strict';
import test from 'node:test';
import { routeModel } from '../src/routing.js';

test('wopal-ai namespace routes to New API and strips namespace', () => {
  assert.deepEqual(routeModel('wopal-ai/qwen3-coder', new Headers()), {
    provider: 'wopal-ai', upstreamModel: 'qwen3-coder'
  });
});

test('ollama namespace routes locally and strips namespace', () => {
  assert.deepEqual(routeModel('ollama/glm-5.3-flash:cloud', new Headers()), {
    provider: 'ollama', upstreamModel: 'glm-5.3-flash:cloud'
  });
});

test('native models preserve ChatGPT account routing', () => {
  const headers = new Headers({ 'ChatGPT-Account-ID': 'account' });
  assert.deepEqual(routeModel('gpt-6.1-sol', headers), {
    provider: 'chatgpt', upstreamModel: 'gpt-6.1-sol'
  });
});

test('native models without ChatGPT account header use OpenAI API', () => {
  assert.deepEqual(routeModel('gpt-6.1-sol', new Headers()), {
    provider: 'openai', upstreamModel: 'gpt-6.1-sol'
  });
});
