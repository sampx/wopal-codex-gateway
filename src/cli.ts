#!/usr/bin/env node
import { fileURLToPath } from 'node:url';
import { activityLogPath, loadConfig, saveConfig } from './config.js';
import { installIntoCodex, refreshCatalog, restoreCodex } from './codex.js';
import { getWopalApiKey, hasWopalApiKey, setWopalApiKeyInteractive } from './keychain.js';
import { createGatewayServer } from './proxy.js';

async function main(): Promise<void> {
  const [command, subcommand, ...rest] = process.argv.slice(2);

  if (!command || command === 'help' || command === '--help' || command === '-h') {
    printHelp();
    return;
  }

  if (command === 'serve') {
    const config = await loadConfig();
    const server = createGatewayServer({
      wopalAiBaseUrl: config.wopalAi.baseUrl,
      ollamaBaseUrl: config.ollama.baseUrl,
      getWopalApiKey,
      activityLogPath: activityLogPath(),
    });
    server.listen(config.listen.port, config.listen.host, () => {
      console.log(`wopal-codex-gateway listening on http://${config.listen.host}:${config.listen.port}`);
    });
    return;
  }

  if (command === 'config' && subcommand === 'show') {
    console.log(JSON.stringify(await loadConfig(), null, 2));
    return;
  }

  if (command === 'config' && subcommand === 'wopal') {
    const config = await loadConfig();
    const baseUrl = option(rest, '--base-url');
    if (baseUrl) {
      const parsed = new URL(baseUrl);
      if (!['http:', 'https:'].includes(parsed.protocol)) throw new Error('Wopal AI base URL must use http or https');
      config.wopalAi.baseUrl = baseUrl.replace(/\/+$/, '');
    }
    const names = options(rest, '--model');
    if (names.length > 0) {
      const contextWindow = numberOption(rest, '--context-window') ?? 262_144;
      const vision = rest.includes('--vision');
      const reasoning = !rest.includes('--no-reasoning');
      config.wopalAi.models = names.map((name) => ({ name, contextWindow, vision, reasoning }));
    }
    await saveConfig(config);
    console.log(`Wopal AI configured: ${config.wopalAi.baseUrl || '(base URL not set)'}; models=${config.wopalAi.models.map((model) => `wopal-ai/${model.name}`).join(', ')}`);
    return;
  }

  if (command === 'config' && subcommand === 'ollama') {
    const baseUrl = option(rest, '--base-url');
    if (!baseUrl) throw new Error('Usage: wopal-codex-gateway config ollama --base-url http://127.0.0.1:11434');
    const config = await loadConfig();
    config.ollama.baseUrl = baseUrl.replace(/\/+$/, '');
    await saveConfig(config);
    console.log(`Ollama configured: ${config.ollama.baseUrl}`);
    return;
  }

  if (command === 'auth' && subcommand === 'set') {
    await setWopalApiKeyInteractive();
    console.log('Wopal AI API key saved in macOS Keychain.');
    return;
  }

  if (command === 'auth' && subcommand === 'status') {
    console.log((await hasWopalApiKey()) ? 'Wopal AI API key: configured' : 'Wopal AI API key: not configured');
    return;
  }

  if (command === 'catalog' && subcommand === 'refresh') {
    const result = await refreshCatalog(await loadConfig());
    printCatalogResult(result);
    return;
  }

  if (command === 'codex' && subcommand === 'install') {
    const result = await installIntoCodex(await loadConfig(), fileURLToPath(import.meta.url));
    printCatalogResult(result);
    console.log('Codex configured to use wopal-codex-gateway. Restart ChatGPT/Codex Desktop to reload the model catalog.');
    return;
  }

  if (command === 'codex' && subcommand === 'restore') {
    await restoreCodex();
    console.log('Codex gateway configuration restored. Restart ChatGPT/Codex Desktop to reload the original configuration.');
    return;
  }

  throw new Error(`Unknown command: ${[command, subcommand].filter(Boolean).join(' ')}`);
}

function printCatalogResult(result: { native: number; wopalAi: number; ollama: number; ollamaWarning?: string }): void {
  console.log(`Model catalog: official=${result.native}, wopal-ai=${result.wopalAi}, ollama=${result.ollama}`);
  if (result.ollamaWarning) console.warn(`Ollama discovery warning: ${result.ollamaWarning}`);
}

function option(args: string[], name: string): string | undefined {
  const index = args.indexOf(name);
  return index >= 0 ? args[index + 1] : undefined;
}

function options(args: string[], name: string): string[] {
  const values: string[] = [];
  for (let index = 0; index < args.length; index += 1) {
    if (args[index] === name && args[index + 1]) values.push(args[index + 1]!);
  }
  return values;
}

function numberOption(args: string[], name: string): number | undefined {
  const raw = option(args, name);
  if (raw === undefined) return undefined;
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${name} must be a positive integer`);
  return value;
}

function printHelp(): void {
  console.log(`wopal-codex-gateway\n\nCommands:\n  config show\n  config wopal --base-url <url> [--model qwen3-coder] [--context-window 262144] [--vision] [--no-reasoning]\n  config ollama --base-url http://127.0.0.1:11434\n  auth set\n  auth status\n  catalog refresh\n  codex install\n  codex restore\n  serve\n\nRouting:\n  wopal-ai/<model> -> Wopal AI / New API\n  ollama/<model>   -> local Ollama\n  all other models -> native ChatGPT/OpenAI\n`);
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
});
