import { execFile, spawn } from 'node:child_process';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);
const SERVICE = 'com.wopal.codex-gateway.wopal-ai';
const ACCOUNT = 'wopal-ai';

export async function getWopalApiKey(): Promise<string> {
  try {
    const { stdout } = await execFileAsync('/usr/bin/security', ['find-generic-password', '-a', ACCOUNT, '-s', SERVICE, '-w'], { encoding: 'utf8' });
    const key = stdout.trim();
    if (!key) throw new Error('empty key');
    return key;
  } catch {
    throw new Error('Wopal AI API key is not configured. Run: wopal-codex-gateway auth set');
  }
}

export async function hasWopalApiKey(): Promise<boolean> {
  try { await getWopalApiKey(); return true; } catch { return false; }
}

export async function setWopalApiKeyInteractive(): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const child = spawn('/usr/bin/security', ['add-generic-password', '-a', ACCOUNT, '-s', SERVICE, '-U', '-w'], {
      stdio: 'inherit',
    });
    child.once('error', reject);
    child.once('exit', (code) => code === 0 ? resolve() : reject(new Error(`security exited with code ${code ?? 'unknown'}`)));
  });
}
