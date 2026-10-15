import { appendFile, mkdir } from 'node:fs/promises';
import { dirname } from 'node:path';

export async function logActivity(path: string | undefined, event: Record<string, unknown>): Promise<void> {
  if (!path) return;
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const fields = Object.entries(event)
    .filter(([, value]) => value !== undefined)
    .map(([key, value]) => `${key}=${JSON.stringify(value)}`)
    .join(' ');
  await appendFile(path, `${new Date().toISOString()} ${fields}\n`, { mode: 0o600 })
    .catch(() => undefined);
}
