import { mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';
import { buildCatalog, discoverOllamaModels, loadBundledNativeCatalog, writeCatalog } from './catalog.js';
import { applyGatewayConfig, captureRestoreState, mergeGatewayRestoreState, restoreGatewayConfig, type CodexRestoreState } from './codex-config.js';
import {
  codexConfigPath, codexGatewayCatalogPath, restoreStatePath,
} from './config.js';
import type { ExternalModelConfig, GatewayConfig } from './types.js';
import { hasWopalApiKey } from './keychain.js';
import { installLaunchAgent, uninstallLaunchAgent } from './service.js';

export interface CatalogRefreshResult { native: number; wopalAi: number; ollama: number; ollamaWarning?: string }

export async function refreshCatalog(config: GatewayConfig): Promise<CatalogRefreshResult> {
  const native = await loadBundledNativeCatalog();
  let ollamaModels: ExternalModelConfig[] = [];
  let ollamaWarning: string | undefined;
  if (config.ollama.enabled) {
    try {
      ollamaModels = await discoverOllamaModels(config.ollama.baseUrl);
    } catch (error) {
      ollamaWarning = error instanceof Error ? error.message : String(error);
    }
  }
  const catalog = buildCatalog(native, config.wopalAi.models, ollamaModels);
  await mkdir(dirname(codexGatewayCatalogPath()), { recursive: true });
  await writeCatalog(codexGatewayCatalogPath(), catalog);
  return { native: native.models.length, wopalAi: config.wopalAi.models.length, ollama: ollamaModels.length, ollamaWarning };
}

export async function installIntoCodex(config: GatewayConfig, cliPath: string): Promise<CatalogRefreshResult> {
  if (!config.wopalAi.baseUrl.trim()) throw new Error('Wopal AI base URL is not configured. Run: wopal-codex-gateway config wopal --base-url <url>');
  if (!(await hasWopalApiKey())) throw new Error('Wopal AI API key is not configured. Run: wopal-codex-gateway auth set');

  const refresh = await refreshCatalog(config);
  await installLaunchAgent(cliPath);
  await waitForHealth(config.listen.host, config.listen.port);

  const configFile = codexConfigPath();
  const currentText = await readFile(configFile, 'utf8').catch((error: NodeJS.ErrnoException) => {
    if (error.code === 'ENOENT') return '';
    throw error;
  });
  const gatewayBaseUrl = `http://${config.listen.host}:${config.listen.port}/v1`;
  const currentState = captureRestoreState(currentText);
  const alreadyManaged = currentState.openaiBaseUrl.value === gatewayBaseUrl
    && currentState.modelCatalogJson.value === codexGatewayCatalogPath();

  await mkdir(dirname(restoreStatePath()), { recursive: true, mode: 0o700 });
  if (!alreadyManaged) {
    await writeFile(restoreStatePath(), `${JSON.stringify(currentState, null, 2)}\n`, { mode: 0o600 });
  } else {
    const savedText = await readFile(restoreStatePath(), 'utf8').catch((error: NodeJS.ErrnoException) => {
      if (error.code === 'ENOENT') return undefined;
      throw error;
    });
    if (savedText) {
      const saved = JSON.parse(savedText) as CodexRestoreState;
      const merged = mergeGatewayRestoreState(saved, currentState);
      if (JSON.stringify(merged) !== JSON.stringify(saved)) {
        await writeFile(restoreStatePath(), `${JSON.stringify(merged, null, 2)}\n`, { mode: 0o600 });
      }
    }
  }
  const nextText = applyGatewayConfig(currentText, codexGatewayCatalogPath(), gatewayBaseUrl);
  await writeAtomic(configFile, nextText);
  return refresh;
}

export async function restoreCodex(): Promise<void> {
  const state = JSON.parse(await readFile(restoreStatePath(), 'utf8')) as CodexRestoreState;
  const configFile = codexConfigPath();
  const currentText = await readFile(configFile, 'utf8').catch((error: NodeJS.ErrnoException) => {
    if (error.code === 'ENOENT') return '';
    throw error;
  });
  await writeAtomic(configFile, restoreGatewayConfig(currentText, state));
  await uninstallLaunchAgent();
  await rm(restoreStatePath(), { force: true });
  await rm(codexGatewayCatalogPath(), { force: true });
}

async function writeAtomic(path: string, content: string): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  const temp = `${path}.wopal-${process.pid}.tmp`;
  await writeFile(temp, content, { mode: 0o600 });
  const { rename } = await import('node:fs/promises');
  await rename(temp, path);
}

export async function waitForHealth(host: string, port: number, timeoutMs = 90_000, retryDelayMs = 250): Promise<void> {
  const url = `http://${host}:${port}/_health`;
  const deadline = Date.now() + timeoutMs;
  let lastError: unknown;
  while (Date.now() < deadline) {
    const remaining = Math.max(1, deadline - Date.now());
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(Math.min(500, remaining)) });
      if (response.ok) return;
      lastError = new Error(`health returned ${response.status}`);
    } catch (error) {
      lastError = error;
    }
    const sleepMs = Math.min(retryDelayMs, Math.max(0, deadline - Date.now()));
    if (sleepMs > 0) await new Promise((resolve) => setTimeout(resolve, sleepMs));
  }
  throw new Error(`gateway service did not become healthy within ${timeoutMs}ms: ${lastError instanceof Error ? lastError.message : String(lastError)}`);
}
