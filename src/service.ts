import { execFile } from 'node:child_process';
import { mkdir, rm, writeFile } from 'node:fs/promises';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { promisify } from 'node:util';
import { stateDir } from './config.js';

const execFileAsync = promisify(execFile);
const LABEL = 'com.wopal.codex-gateway';

export function launchAgentPath(): string {
  return join(homedir(), 'Library', 'LaunchAgents', `${LABEL}.plist`);
}

export async function installLaunchAgent(cliPath: string): Promise<void> {
  const path = launchAgentPath();
  await mkdir(dirname(path), { recursive: true });
  await mkdir(stateDir(), { recursive: true, mode: 0o700 });
  const log = join(stateDir(), 'service.log');
  const errorLog = join(stateDir(), 'service-error.log');
  const plist = `<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0">\n<dict>\n  <key>Label</key><string>${LABEL}</string>\n  <key>ProgramArguments</key>\n  <array>\n    <string>${xml(process.execPath)}</string>\n    <string>${xml(cliPath)}</string>\n    <string>serve</string>\n  </array>\n  <key>RunAtLoad</key><true/>\n  <key>KeepAlive</key><true/>\n  <key>StandardOutPath</key><string>${xml(log)}</string>\n  <key>StandardErrorPath</key><string>${xml(errorLog)}</string>\n</dict>\n</plist>\n`;
  await writeFile(path, plist, { mode: 0o644 });
  const domain = `gui/${process.getuid?.() ?? 501}`;
  await execFileAsync('/bin/launchctl', ['bootout', domain, path]).catch(() => undefined);
  await execFileAsync('/bin/launchctl', ['bootstrap', domain, path]);
  await execFileAsync('/bin/launchctl', ['kickstart', '-k', `${domain}/${LABEL}`]);
}

export async function uninstallLaunchAgent(): Promise<void> {
  const path = launchAgentPath();
  const domain = `gui/${process.getuid?.() ?? 501}`;
  await execFileAsync('/bin/launchctl', ['bootout', domain, path]).catch(() => undefined);
  await rm(path, { force: true });
}

function xml(value: string): string {
  return value.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;').replaceAll("'", '&apos;');
}
