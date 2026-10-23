import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import test from 'node:test';

const execFileAsync = promisify(execFile);

interface Fixture {
  root: string;
  script: string;
  binary: string;
  log: string;
}

// The wrapper resolves the binary from its own location, so a temp checkout
// with a fake executable is enough to observe exactly what it forwards.
async function fixture(): Promise<Fixture> {
  const root = await mkdtemp(join(tmpdir(), 'gateway-script-test-'));
  const scriptDir = join(root, 'scripts');
  const binDir = join(root, 'runtime', 'bin');
  await mkdir(scriptDir, { recursive: true });
  await mkdir(binDir, { recursive: true });

  const script = join(scriptDir, 'gateway');
  await writeFile(script, await readFile(new URL('../scripts/gateway', import.meta.url), 'utf8'), { mode: 0o755 });

  const log = join(root, 'calls.log');
  const binary = join(binDir, 'wopal-codex-gateway');
  await writeFile(
    binary,
    `#!/bin/bash\nprintf '%s\\n' "$*" >> '${log}'\nexit 0\n`,
    { mode: 0o755 },
  );
  return { root, script, binary, log };
}

async function run(script: string, args: string[]) {
  try {
    const { stdout, stderr } = await execFileAsync('/bin/bash', [script, ...args], { timeout: 5_000 });
    return { code: 0, stdout, stderr };
  } catch (error) {
    const failure = error as Error & { code?: number; stdout?: string; stderr?: string };
    return { code: failure.code ?? 1, stdout: failure.stdout ?? '', stderr: failure.stderr ?? '' };
  }
}

async function calls(log: string): Promise<string[]> {
  try {
    return (await readFile(log, 'utf8')).trim().split('\n').filter(Boolean);
  } catch {
    return [];
  }
}

test('gateway script targets the Go binary without node or pnpm', async () => {
  const source = await readFile(new URL('../scripts/gateway', import.meta.url), 'utf8');
  assert.match(source, /^#!\/usr\/bin\/env bash/m);
  assert.match(source, /set -euo pipefail/);
  assert.match(source, /runtime\/bin\/wopal-codex-gateway/);
  assert.doesNotMatch(source, /\bnode\b/i);
  assert.doesNotMatch(source, /pnpm/);
  assert.doesNotMatch(source, /dist\/cli\.js/);
});

test('gateway script maps install to codex install', async () => {
  const { script, log } = await fixture();
  const result = await run(script, ['install', '--extra']);
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(await calls(log), ['codex install --extra']);
  assert.match(result.stdout, /Restart ChatGPT\/Codex Desktop manually/);
});

test('gateway script maps disable to codex restore', async () => {
  const { script, log } = await fixture();
  const result = await run(script, ['disable']);
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(await calls(log), ['codex restore']);
  assert.match(result.stdout, /Restart ChatGPT\/Codex Desktop manually/);
});

test('gateway script maps lifecycle verbs to service subcommands', async () => {
  for (const verb of ['start', 'stop', 'restart', 'status']) {
    const { script, log } = await fixture();
    const result = await run(script, [verb]);
    assert.equal(result.code, 0, `${verb}: ${result.stderr}`);
    assert.deepEqual(await calls(log), [`service ${verb}`], verb);
  }
});

test('gateway script forwards grouped commands unchanged', async () => {
  for (const args of [
    ['models', 'list'],
    ['service', 'install'],
    ['auth', 'status'],
    ['config', 'show'],
    ['catalog', 'refresh'],
  ]) {
    const { script, log } = await fixture();
    const result = await run(script, args);
    assert.equal(result.code, 0, `${args.join(' ')}: ${result.stderr}`);
    assert.deepEqual(await calls(log), [args.join(' ')], args.join(' '));
  }
});

test('gateway script reports a missing binary clearly', async () => {
  const { script, binary } = await fixture();
  await rm(binary);
  const result = await run(script, ['status']);
  assert.notEqual(result.code, 0);
  assert.match(
    result.stderr,
    /error: built gateway binary not found at .*runtime\/bin\/wopal-codex-gateway; run 'go build -o runtime\/bin\/wopal-codex-gateway \.\/cmd\/gateway'/,
  );
});

test('gateway script keeps help local', async () => {
  const { script, log } = await fixture();
  const result = await run(script, ['help']);
  assert.equal(result.code, 0, result.stderr);
  assert.match(result.stdout, /service install/);
  assert.deepEqual(await calls(log), []);
});
