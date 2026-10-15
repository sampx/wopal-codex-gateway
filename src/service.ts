import { execFile } from 'node:child_process';
import { mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { promisify } from 'node:util';
import { logsDir, projectRootDir, serviceErrorLogPath, serviceLogPath, stateDir } from './config.js';

const execFileAsync = promisify(execFile);
export const LAUNCH_AGENT_LABEL = 'com.wopal.codex-gateway';
const TEMPLATE_NAME = `${LAUNCH_AGENT_LABEL}.plist.template`;

export function launchAgentPath(): string {
  return join(homedir(), 'Library', 'LaunchAgents', `${LAUNCH_AGENT_LABEL}.plist`);
}

export function launchAgentTemplatePath(): string {
  return join(projectRootDir(), 'launchd', TEMPLATE_NAME);
}

export function gatewayDaemonPath(): string {
  return join(projectRootDir(), 'runtime', 'bin', 'wopal-codex-gatewayd');
}

export interface LaunchAgentTemplateValues {
  label: string;
  programPath: string;
  projectRoot: string;
  logPath: string;
  errorLogPath: string;
}

export function renderLaunchAgentPlist(template: string, values: LaunchAgentTemplateValues): string {
  const replacements: Record<string, string> = {
    '{{LABEL}}': values.label,
    '{{PROGRAM_PATH}}': values.programPath,
    '{{PROJECT_ROOT}}': values.projectRoot,
    '{{LOG_PATH}}': values.logPath,
    '{{ERROR_LOG_PATH}}': values.errorLogPath,
  };
  let rendered = template;
  for (const [placeholder, value] of Object.entries(replacements)) {
    rendered = rendered.replaceAll(placeholder, xml(value));
  }
  return rendered;
}

export async function installLaunchAgent(_cliPath?: string): Promise<void> {
  const path = launchAgentPath();
  await mkdir(dirname(path), { recursive: true });
  await mkdir(stateDir(), { recursive: true, mode: 0o700 });
  await mkdir(logsDir(), { recursive: true, mode: 0o700 });
  const template = await readFile(launchAgentTemplatePath(), 'utf8');
  const plist = renderLaunchAgentPlist(template, {
    label: LAUNCH_AGENT_LABEL,
    programPath: gatewayDaemonPath(),
    projectRoot: projectRootDir(),
    logPath: serviceLogPath(),
    errorLogPath: serviceErrorLogPath(),
  });
  await writeFile(path, plist, { mode: 0o644 });
  const domain = `gui/${process.getuid?.() ?? 501}`;
  await execFileAsync('/bin/launchctl', ['bootout', domain, path]).catch(() => undefined);
  await execFileAsync('/bin/launchctl', ['bootstrap', domain, path]);
  await execFileAsync('/bin/launchctl', ['kickstart', '-k', `${domain}/${LAUNCH_AGENT_LABEL}`]);
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
