export interface RestoreValue {
  hadValue: boolean;
  value?: string;
}

export interface CodexRestoreState {
  model: RestoreValue;
  modelCatalogJson: RestoreValue;
  openaiBaseUrl: RestoreValue;
}

const MANAGED_KEYS = {
  model: 'model',
  modelCatalogJson: 'model_catalog_json',
  openaiBaseUrl: 'openai_base_url',
} as const;

function splitLines(text: string): string[] {
  return text.match(/.*(?:\n|$)/g)?.filter((line) => line.length > 0) ?? [];
}

function rootEndIndex(lines: string[]): number {
  const index = lines.findIndex((line) => /^\s*\[/.test(line));
  return index < 0 ? lines.length : index;
}

function assignmentRegex(key: string): RegExp {
  return new RegExp(`^(\\s*${key.replace(/[.*+?^${}()|[\\]\\]/g, '\\$&')}\\s*=\\s*)("(?:\\\\.|[^"\\\\])*")(\\s*(?:#.*)?)(\\n?)$`);
}

function parseTomlString(raw: string): string {
  return JSON.parse(raw) as string;
}

function quoteTomlString(value: string): string {
  return JSON.stringify(value);
}

function findRootAssignment(lines: string[], key: string): { index: number; match: RegExpMatchArray } | undefined {
  const end = rootEndIndex(lines);
  const regex = assignmentRegex(key);
  for (let index = 0; index < end; index += 1) {
    const match = lines[index]!.match(regex);
    if (match) return { index, match };
  }
  return undefined;
}

function getRootString(text: string, key: string): RestoreValue {
  const found = findRootAssignment(splitLines(text), key);
  if (!found) return { hadValue: false };
  return { hadValue: true, value: parseTomlString(found.match[2]!) };
}

function setRootString(text: string, key: string, value: string): string {
  const lines = splitLines(text);
  const found = findRootAssignment(lines, key);
  if (found) {
    const [, prefix, , suffix, newline] = found.match;
    lines[found.index] = `${prefix}${quoteTomlString(value)}${suffix}${newline}`;
    return lines.join('');
  }

  let insertion = rootEndIndex(lines);
  while (insertion > 0 && /^\s*$/.test(lines[insertion - 1]!)) insertion -= 1;
  const newline = text.length === 0 || text.endsWith('\n') ? '' : '\n';
  lines.splice(insertion, 0, `${newline}${key} = ${quoteTomlString(value)}\n`);
  return lines.join('');
}

function removeRootValue(text: string, key: string): string {
  const lines = splitLines(text);
  const found = findRootAssignment(lines, key);
  if (!found) return text;
  lines.splice(found.index, 1);
  return lines.join('');
}

export function captureRestoreState(text: string): CodexRestoreState {
  return {
    model: getRootString(text, MANAGED_KEYS.model),
    modelCatalogJson: getRootString(text, MANAGED_KEYS.modelCatalogJson),
    openaiBaseUrl: getRootString(text, MANAGED_KEYS.openaiBaseUrl),
  };
}

export function applyGatewayConfig(text: string, modelCatalogPath: string, gatewayBaseUrl: string): string {
  let next = setRootString(text, MANAGED_KEYS.modelCatalogJson, modelCatalogPath);
  next = setRootString(next, MANAGED_KEYS.openaiBaseUrl, gatewayBaseUrl);
  return next;
}

function restoreValue(text: string, key: string, state: RestoreValue): string {
  return state.hadValue ? setRootString(text, key, state.value ?? '') : removeRootValue(text, key);
}

export function restoreGatewayConfig(text: string, state: CodexRestoreState): string {
  let next = restoreValue(text, MANAGED_KEYS.model, state.model);
  next = restoreValue(next, MANAGED_KEYS.modelCatalogJson, state.modelCatalogJson);
  next = restoreValue(next, MANAGED_KEYS.openaiBaseUrl, state.openaiBaseUrl);
  return next;
}
