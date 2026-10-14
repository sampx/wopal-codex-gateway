import test from 'node:test';
import assert from 'node:assert/strict';
import { fetchWopalModels } from '../src/wopal-api.js';

test('fetchWopalModels returns model ids for a valid token', async () => {
  const fakeFetch: typeof fetch = async (input, init) => {
    assert.equal(String(input), 'https://example.test/v1/models');
    assert.equal(new Headers(init?.headers).get('authorization'), 'Bearer sk-test');
    return new Response(JSON.stringify({ object: 'list', data: [{ id: 'glm-5.3-flash-free' }, { id: 'deepseek-v4-flash-free' }] }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    });
  };

  assert.deepEqual(await fetchWopalModels('https://example.test/v1', 'sk-test', fakeFetch), [
    'glm-5.3-flash-free',
    'deepseek-v4-flash-free',
  ]);
});

test('fetchWopalModels reports upstream auth failures without exposing the token', async () => {
  const fakeFetch: typeof fetch = async () => new Response(JSON.stringify({
    error: { message: 'Invalid token', type: 'new_api_error' },
  }), { status: 401, headers: { 'content-type': 'application/json' } });

  await assert.rejects(
    fetchWopalModels('https://example.test/v1/', 'sk-secret-value', fakeFetch),
    (error: unknown) => {
      assert.match(String(error), /HTTP 401/);
      assert.match(String(error), /Invalid token/);
      assert.doesNotMatch(String(error), /sk-secret-value/);
      return true;
    },
  );
});
