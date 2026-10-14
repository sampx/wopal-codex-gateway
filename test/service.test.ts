import assert from 'node:assert/strict';
import test from 'node:test';
import { renderLaunchAgentPlist } from '../src/service.js';

test('renderLaunchAgentPlist fills runtime paths and escapes XML', () => {
  const template = '<plist>{{LABEL}}|{{NODE_PATH}}|{{CLI_PATH}}|{{LOG_PATH}}|{{ERROR_LOG_PATH}}</plist>';
  const rendered = renderLaunchAgentPlist(template, {
    label: 'com.wopal.codex-gateway',
    nodePath: '/tmp/node & tools/node',
    cliPath: '/tmp/project/dist/cli.js',
    logPath: '/tmp/a<b.log',
    errorLogPath: '/tmp/error.log',
  });

  assert.equal(rendered, '<plist>com.wopal.codex-gateway|/tmp/node &amp; tools/node|/tmp/project/dist/cli.js|/tmp/a&lt;b.log|/tmp/error.log</plist>');
});
