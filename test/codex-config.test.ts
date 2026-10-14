import assert from 'node:assert/strict';
import test from 'node:test';
import { applyGatewayConfig, captureRestoreState, restoreGatewayConfig } from '../src/codex-config.js';

const clean = `model = "gpt-6.1-sol"\nmodel_reasoning_effort = "medium"\n\n[features]\nmemories = true\n`;

test('install adds only gateway-owned root values and preserves existing config', () => {
  const next = applyGatewayConfig(clean, '/Users/sam/.codex/wopal-codex-gateway-models.json', 'http://127.0.0.1:11435/v1');
  assert.match(next, /model = "gpt-6\.1-sol"/);
  assert.match(next, /model_catalog_json = "\/Users\/sam\/\.codex\/wopal-codex-gateway-models\.json"/);
  assert.match(next, /openai_base_url = "http:\/\/127\.0\.0\.1:11435\/v1"/);
  assert.match(next, /\[features\]\nmemories = true/);
});

test('restore removes gateway values when they did not exist before install', () => {
  const state = captureRestoreState(clean);
  const installed = applyGatewayConfig(clean, '/tmp/catalog.json', 'http://127.0.0.1:11435/v1');
  assert.equal(restoreGatewayConfig(installed, state), clean);
});

test('restore puts pre-existing gateway-related values back exactly', () => {
  const original = `model = "gpt-6.1-sol"\nmodel_catalog_json = "/old/catalog.json"\nopenai_base_url = "https://old.example/v1"\n\n[features]\nmemories = true\n`;
  const state = captureRestoreState(original);
  const installed = applyGatewayConfig(original, '/new/catalog.json', 'http://127.0.0.1:11435/v1');
  assert.equal(restoreGatewayConfig(installed, state), original);
});

test('restore resets model if the user selected a gateway model after install', () => {
  const state = captureRestoreState(clean);
  const installed = applyGatewayConfig(clean, '/tmp/catalog.json', 'http://127.0.0.1:11435/v1');
  const switched = installed.replace('model = "gpt-6.1-sol"', 'model = "wopal-ai/qwen3-coder"');
  const restored = restoreGatewayConfig(switched, state);
  assert.match(restored, /^model = "gpt-6\.1-sol"/);
  assert.doesNotMatch(restored, /wopal-ai\/qwen3-coder/);
});
