import assert from 'node:assert/strict';
import test from 'node:test';
import { normalizeOllamaRequest } from '../src/ollama-normalize.js';

test('ollama normalization drops opaque reasoning and maps developer role', () => {
  const body = Buffer.from(JSON.stringify({
    model: 'glm',
    input: [
      { type: 'reasoning', id: 'rs_native', encrypted_content: 'opaque' },
      { type: 'message', role: 'developer', content: [{ type: 'input_text', text: 'rules' }] },
      { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'hi' }] },
    ],
  }));
  const payload = JSON.parse(normalizeOllamaRequest(body).toString('utf8'));
  assert.equal(payload.input.length, 2);
  assert.equal(payload.input[0].role, 'system');
  assert.equal(payload.input[1].role, 'user');
});

test('ollama normalization converts Codex custom tool history to function calls', () => {
  const body = Buffer.from(JSON.stringify({ input: [
    { type: 'custom_tool_call', id: 'ctc_1', call_id: 'call_1', name: 'apply_patch', input: '*** Begin Patch' },
    { type: 'custom_tool_call_output', call_id: 'call_1', output: 'Success' },
  ] }));
  const payload = JSON.parse(normalizeOllamaRequest(body).toString('utf8'));
  assert.equal(payload.input[0].type, 'function_call');
  assert.equal(payload.input[0].name, 'apply_patch');
  assert.equal(JSON.parse(payload.input[0].arguments).input, '*** Begin Patch');
  assert.equal(payload.input[1].type, 'function_call_output');
});
