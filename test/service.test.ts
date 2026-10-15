import assert from 'node:assert/strict';
import test from 'node:test';
import { renderLaunchAgentPlist } from '../src/service.js';

test('renderLaunchAgentPlist fills daemon paths and escapes XML', () => {
  const template = '<plist>{{LABEL}}|{{PROGRAM_PATH}}|{{PROJECT_ROOT}}|{{LOG_PATH}}|{{ERROR_LOG_PATH}}</plist>';
  const rendered = renderLaunchAgentPlist(template, {
    label: 'com.wopal.codex-gateway',
    programPath: '/tmp/project/runtime/bin/wopal-codex-gatewayd',
    projectRoot: '/tmp/project & tools',
    logPath: '/tmp/a<b.log',
    errorLogPath: '/tmp/error.log',
  });

  assert.equal(rendered, '<plist>com.wopal.codex-gateway|/tmp/project/runtime/bin/wopal-codex-gatewayd|/tmp/project &amp; tools|/tmp/a&lt;b.log|/tmp/error.log</plist>');
});
