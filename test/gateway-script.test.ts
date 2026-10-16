import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { mkdtemp, mkdir, readFile, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import test from 'node:test';

const execFileAsync = promisify(execFile);

async function runGatewayScript(scenario: string, loaded: boolean) {
  const root = await mkdtemp(join(tmpdir(), 'gateway-restart-test-'));
  const bin = join(root, 'bin');
  await mkdir(bin);
  await writeFile(join(root, 'agent.plist'), 'test fixture');
  if (loaded) await writeFile(join(root, 'loaded'), 'yes');
  const source = await readFile(new URL('../scripts/gateway', import.meta.url), 'utf8');
  const script = join(root, 'gateway');
  await writeFile(script, source.replace('PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"', `PLIST='${join(root, 'agent.plist')}'`));
  await writeFile(join(bin, 'launchctl'), `#!/bin/bash
printf '%s\\n' "$*" >> "$GATEWAY_TEST_ROOT/calls"
case "$1" in
  print) [[ -f "$GATEWAY_TEST_ROOT/loaded" ]] ;;
  bootout) /bin/mv "$GATEWAY_TEST_ROOT/loaded" "$GATEWAY_TEST_ROOT/stopped" ;;
  bootstrap)
    if [[ "$GATEWAY_TEST_SCENARIO" == bootstrap-fail ]]; then echo 'bootstrap denied' >&2; exit 5; fi
    if [[ "$GATEWAY_TEST_SCENARIO" == busy-once && ! -f "$GATEWAY_TEST_ROOT/busy-seen" ]]; then
      /usr/bin/touch "$GATEWAY_TEST_ROOT/busy-seen"; echo 'Operation already in progress' >&2; exit 37
    fi
    /usr/bin/touch "$GATEWAY_TEST_ROOT/loaded"
    ;;
  kickstart)
    if [[ "$GATEWAY_TEST_SCENARIO" == kickstart-fail ]]; then echo 'kickstart denied' >&2; exit 5; fi
    ;;
  *) exit 99 ;;
esac
`, { mode: 0o755 });
  await writeFile(join(bin, 'curl'), `#!/bin/bash
[[ "$GATEWAY_TEST_SCENARIO" != unhealthy ]]
`, { mode: 0o755 });
  await writeFile(join(bin, 'sleep'), '#!/bin/bash\nexit 0\n', { mode: 0o755 });
  let code = 0;
  let stdout = '';
  let stderr = '';
  try {
    ({ stdout, stderr } = await execFileAsync('/bin/bash', [script, 'restart'], {
      env: { ...process.env, PATH: `${bin}:/usr/bin:/bin`, GATEWAY_TEST_ROOT: root, GATEWAY_TEST_SCENARIO: scenario },
      timeout: 5_000,
    }));
  } catch (error) {
    const failure = error as Error & { code: number; stdout: string; stderr: string };
    code = failure.code;
    stdout = failure.stdout;
    stderr = failure.stderr;
  }
  const calls = (await readFile(join(root, 'calls'), 'utf8')).trim().split('\n');
  return { code, stdout, stderr, calls };
}

test('restart keeps a registered service loaded and uses kickstart', async () => {
  const result = await runGatewayScript('ok', true);
  assert.equal(result.code, 0, result.stderr);
  assert.ok(result.calls.some((call) => call.startsWith('kickstart -k ')));
  assert.ok(result.calls.every((call) => !/^(bootout|bootstrap) /.test(call)), result.calls.join('\n'));
  assert.match(result.stdout, /Proxy running/);
});

test('restart retries transient launchd registration error 37 when unloaded', async () => {
  const result = await runGatewayScript('busy-once', false);
  assert.equal(result.code, 0, result.stderr);
  assert.equal(result.calls.filter((call) => call.startsWith('bootstrap ')).length, 2);
  assert.ok(result.calls.every((call) => !call.startsWith('kickstart ')), 'RunAtLoad already starts a newly registered service');
  assert.match(result.stdout, /Proxy running/);
});

test('restart reports registration failures without claiming success', async () => {
  const result = await runGatewayScript('bootstrap-fail', false);
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /bootstrap/);
  assert.doesNotMatch(result.stdout, /Proxy running/);
});

test('restart leaves service registered when kickstart fails', async () => {
  const result = await runGatewayScript('kickstart-fail', true);
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /kickstart/);
  assert.ok(result.calls.every((call) => !call.startsWith('bootout ')));
  assert.doesNotMatch(result.stdout, /Proxy running/);
});

test('restart must pass a health check before reporting success', async () => {
  const result = await runGatewayScript('unhealthy', true);
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /health check failed/);
  assert.doesNotMatch(result.stdout, /Proxy running/);
});
