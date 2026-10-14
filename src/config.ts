import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import type { GatewayConfig } from './types.js';

export const DEFAULT_PORT = 11435;

export function stateDir(): string { return join(homedir(), '.wopal-codex-gateway'); }
export function configPath(): string { return join(stateDir(), 'config.json'); }
export function restoreStatePath(): string { return join(stateDir(), 'codex-restore.json'); }
export function activityLogPath(): string { return join(stateDir(), 'gateway.log'); }
export function codexHome(): string { return process.env.CODEX_HOME || join(homedir(), '.codex'); }
export function codexConfigPath(): string { return join(codexHome(), 'config.toml'); }
export function codexNativeCatalogPath(): string { return join(codexHome(), 'models_cache.json'); }
export function codexGatewayCatalogPath(): string { return join(codexHome(), 'wopal-codex-gateway-models.json'); }

export const defaultConfig: GatewayConfig = {
  listen: { host: '127.0.0.1', port: DEFAULT_PORT },
  wopalAi: { baseUrl: '', models: [{ name: 'qwen3-coder', contextWindow: 262_144, reasoning: true }], excludedModels: [] },
  ollama: { baseUrl: 'http://127.0.0.1:11434' },
};

export async function loadConfig(): Promise<GatewayConfig> {
  try {
    const saved = JSON.parse(await readFile(configPath(), 'utf8')) as Partial<GatewayConfig>;
    return {
      listen: { ...defaultConfig.listen, ...saved.listen },
      wopalAi: { ...defaultConfig.wopalAi, ...saved.wopalAi, models: saved.wopalAi?.models ?? defaultConfig.wopalAi.models, excludedModels: saved.wopalAi?.excludedModels ?? defaultConfig.wopalAi.excludedModels },
      ollama: { ...defaultConfig.ollama, ...saved.ollama },
    };
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return structuredClone(defaultConfig);
    throw error;
  }
}

export async function saveConfig(config: GatewayConfig): Promise<void> {
  const path = configPath();
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const temp = `${path}.tmp-${process.pid}`;
  await writeFile(temp, `${JSON.stringify(config, null, 2)}\n`, { mode: 0o600 });
  await rename(temp, path);
}
