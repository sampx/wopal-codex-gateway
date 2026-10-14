#!/usr/bin/env node
import { fileURLToPath } from 'node:url';
import { activityLogPath, loadConfig, saveConfig } from './config.js';
import { installIntoCodex, refreshCatalog, restoreCodex } from './codex.js';
import { getWopalApiKey, hasWopalApiKey, setWopalApiKeyInteractive } from './keychain.js';
import { createGatewayServer } from './proxy.js';
import { fetchWopalModels } from './wopal-api.js';
import { addWopalModelConfigs, removeWopalModelConfigs, syncWopalModelConfigs } from './wopal-models.js';
import { installLaunchAgent, uninstallLaunchAgent } from './service.js';

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
      const explicit = new Set(names.map((name) => name.startsWith('wopal-ai/') ? name.slice('wopal-ai/'.length) : name));
      config.wopalAi.excludedModels = config.wopalAi.excludedModels.filter((name) => !explicit.has(name));
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
    try {
      const { models, configured } = await validateStoredWopalKey();
      console.log(`Wopal AI API key saved and validated: ${models.length} models available.`);
      printConfiguredModelStatus(configured);
    } catch (error) {
      throw new Error(`Wopal AI API key saved in macOS Keychain, but validation failed: ${error instanceof Error ? error.message : String(error)}`);
    }
    return;
  }

  if (command === 'auth' && subcommand === 'test') {
    const { models, configured } = await validateStoredWopalKey();
    console.log(`Wopal AI API key validation succeeded: ${models.length} models available.`);
    printConfiguredModelStatus(configured);
    return;
  }

  if (command === 'auth' && subcommand === 'status') {
    console.log((await hasWopalApiKey()) ? 'Wopal AI API key: configured' : 'Wopal AI API key: not configured');
    return;
  }

  if (command === 'models' && subcommand === 'list') {
    const config = await loadConfig();
    const models = await fetchWopalModels(config.wopalAi.baseUrl, await getWopalApiKey());
    console.log(models.join('\n'));
    return;
  }

  if (command === 'models' && subcommand === 'sync') {
    const config = await loadConfig();
    if (!config.wopalAi.baseUrl.trim()) throw new Error('Wopal AI base URL is not configured');
    const available = await fetchWopalModels(config.wopalAi.baseUrl, await getWopalApiKey());
    const synced = syncWopalModelConfigs(config.wopalAi.models, available, config.wopalAi.excludedModels);
    config.wopalAi.models = synced.models;
    await saveConfig(config);
    const result = await refreshCatalog(config);
    console.log(`Wopal AI models synced: ${synced.models.length} Codex-usable model(s).`);
    if (synced.skipped.length > 0) console.log(`Skipped non-chat model(s): ${synced.skipped.join(', ')}`);
    if (synced.excluded.length > 0) console.log(`Manually excluded model(s): ${synced.excluded.join(', ')}`);
    printCatalogResult(result);
    return;
  }

  if (command === 'models' && subcommand === 'add') {
    const config = await loadConfig();
    const names = modelPositionals(rest, new Set(['--context-window']));
    if (names.length === 0) throw new Error('Usage: wopal-codex-gateway models add <model...> [--context-window 262144] [--vision] [--no-reasoning]');
    const added = addWopalModelConfigs(config.wopalAi.models, names, {
      contextWindow: numberOption(rest, '--context-window') ?? 262_144,
      vision: rest.includes('--vision'),
      reasoning: !rest.includes('--no-reasoning'),
    });
    config.wopalAi.models = added.models;
    const unexcluded = new Set([...added.added, ...added.existing]);
    config.wopalAi.excludedModels = config.wopalAi.excludedModels.filter((name) => !unexcluded.has(name));
    await saveConfig(config);
    const result = await refreshCatalog(config);
    if (added.added.length > 0) console.log(`Wopal AI model(s) added: ${added.added.join(', ')}`);
    if (added.existing.length > 0) console.log(`Already configured: ${added.existing.join(', ')}`);
    printCatalogResult(result);
    return;
  }

  if (command === 'models' && subcommand === 'remove') {
    const config = await loadConfig();
    const names = rest.filter((value) => !value.startsWith('--'));
    if (names.length === 0) throw new Error('Usage: wopal-codex-gateway models remove <model...>');
    const removed = removeWopalModelConfigs(config.wopalAi.models, names);
    config.wopalAi.models = removed.models;
    config.wopalAi.excludedModels = [...new Set([...config.wopalAi.excludedModels, ...removed.removed, ...removed.missing])];
    await saveConfig(config);
    const result = await refreshCatalog(config);
    if (removed.removed.length > 0) console.log(`Wopal AI model(s) removed: ${removed.removed.join(', ')}`);
    if (removed.missing.length > 0) console.log(`Not configured, but excluded from future sync: ${removed.missing.join(', ')}`);
    if (removed.removed.length > 0) console.log('Removed model(s) are excluded from future sync until explicitly added again.');
    printCatalogResult(result);
    return;
  }

  if (command === 'service' && subcommand === 'install') {
    await installLaunchAgent(fileURLToPath(import.meta.url));
    console.log('LaunchAgent installed and proxy started.');
    return;
  }

  if (command === 'service' && subcommand === 'uninstall') {
    await uninstallLaunchAgent();
    console.log('LaunchAgent unloaded and removed.');
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

async function validateStoredWopalKey(): Promise<{ models: string[]; configured: string[] }> {
  const config = await loadConfig();
  if (!config.wopalAi.baseUrl.trim()) throw new Error('Wopal AI base URL is not configured');
  const models = await fetchWopalModels(config.wopalAi.baseUrl, await getWopalApiKey());
  const available = new Set(models);
  return { models, configured: config.wopalAi.models.filter((model) => available.has(model.name)).map((model) => model.name) };
}

function printConfiguredModelStatus(configured: string[]): void {
  console.log(configured.length > 0
    ? `Configured model(s) available: ${configured.join(', ')}`
    : 'Warning: none of the configured Wopal AI models were returned by /models.');
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


function modelPositionals(args: string[], valueOptions: Set<string>): string[] {
  const values: string[] = [];
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index]!;
    if (valueOptions.has(arg)) {
      index += 1;
      continue;
    }
    if (!arg.startsWith('--')) values.push(arg);
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
  console.log(`wopal-codex-gateway\n\nCommands:\n  config show\n  config wopal --base-url <url> [--model qwen3-coder] [--context-window 262144] [--vision] [--no-reasoning]\n  config ollama --base-url http://127.0.0.1:11434\n  auth set\n  auth test\n  auth status\n  models list\n  models sync\n  models add <model...> [--context-window 262144] [--vision] [--no-reasoning]\n  models remove <model...>\n  service install\n  service uninstall\n  catalog refresh\n  codex install\n  codex restore\n  serve\n\nRouting:\n  wopal-ai/<model> -> Wopal AI / New API\n  ollama/<model>   -> local Ollama\n  all other models -> native ChatGPT/OpenAI\n`);
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
});
