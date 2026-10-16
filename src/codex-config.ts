export interface RestoreValue {
  hadValue: boolean;
  value?: string;
}

export interface RestoreBooleanValue {
  hadValue: boolean;
  value?: boolean;
}

export interface CodexRestoreState {
  model: RestoreValue;
  modelCatalogJson: RestoreValue;
  openaiBaseUrl: RestoreValue;
  guardianReuseParentCompaction?: RestoreBooleanValue;
}

const MANAGED_KEYS = {
  model: 'model',
  modelCatalogJson: 'model_catalog_json',
  openaiBaseUrl: 'openai_base_url',
  guardianReuseParentCompaction: 'guardian_reuse_parent_compaction',
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

function tableRange(lines: string[], table: string): { start: number; end: number } | undefined {
  const header = `[${table}]`;
  const start = lines.findIndex((line) => line.trim() === header);
  if (start < 0) return undefined;
  const next = lines.findIndex((line, index) => index > start && /^\s*\[/.test(line));
  return { start, end: next < 0 ? lines.length : next };
}

function booleanAssignmentRegex(key: string): RegExp {
  return new RegExp(`^(\\s*${key.replace(/[.*+?^${}()|[\\]\\]/g, '\\$&')}\\s*=\\s*)(true|false)(\\s*(?:#.*)?)(\\n?)$`);
}

function findTableBooleanAssignment(lines: string[], table: string, key: string): { index: number; match: RegExpMatchArray } | undefined {
  const range = tableRange(lines, table);
  if (!range) return undefined;
  const regex = booleanAssignmentRegex(key);
  for (let index = range.start + 1; index < range.end; index += 1) {
    const match = lines[index]!.match(regex);
    if (match) return { index, match };
  }
  return undefined;
}

function getTableBoolean(text: string, table: string, key: string): RestoreBooleanValue {
  const found = findTableBooleanAssignment(splitLines(text), table, key);
  if (!found) return { hadValue: false };
  return { hadValue: true, value: found.match[2] === 'true' };
}

function setTableBoolean(text: string, table: string, key: string, value: boolean): string {
  const lines = splitLines(text);
  const found = findTableBooleanAssignment(lines, table, key);
  if (found) {
    const [, prefix, , suffix, newline] = found.match;
    lines[found.index] = `${prefix}${value ? 'true' : 'false'}${suffix}${newline}`;
    return lines.join('');
  }

  let range = tableRange(lines, table);
  if (!range) {
    const prefix = text.length === 0 || text.endsWith('\n') ? '' : '\n';
    const separator = text.length === 0 ? '' : '\n';
    return `${text}${prefix}${separator}[${table}]\n${key} = ${value ? 'true' : 'false'}\n`;
  }
  let insertion = range.end;
  while (insertion > range.start + 1 && /^\s*$/.test(lines[insertion - 1]!)) insertion -= 1;
  lines.splice(insertion, 0, `${key} = ${value ? 'true' : 'false'}\n`);
  return lines.join('');
}

function removeTableValue(text: string, table: string, key: string): string {
  const lines = splitLines(text);
  const found = findTableBooleanAssignment(lines, table, key);
  if (!found) return text;
  lines.splice(found.index, 1);
  const range = tableRange(lines, table);
  if (!range) return lines.join('');
  const meaningful = lines.slice(range.start + 1, range.end).some((line) => {
    const trimmed = line.trim();
    return trimmed !== '' && !trimmed.startsWith('#');
  });
  if (!meaningful) {
    lines.splice(range.start, range.end - range.start);
    if (range.start > 0 && /^\s*$/.test(lines[range.start - 1] ?? '')) lines.splice(range.start - 1, 1);
  }
  return lines.join('');
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
    guardianReuseParentCompaction: getTableBoolean(text, 'features', MANAGED_KEYS.guardianReuseParentCompaction),
  };
}

export function applyGatewayConfig(text: string, modelCatalogPath: string, gatewayBaseUrl: string): string {
  let next = setRootString(text, MANAGED_KEYS.modelCatalogJson, modelCatalogPath);
  next = setRootString(next, MANAGED_KEYS.openaiBaseUrl, gatewayBaseUrl);
  next = setTableBoolean(next, 'features', MANAGED_KEYS.guardianReuseParentCompaction, false);
  return next;
}

function restoreValue(text: string, key: string, state: RestoreValue): string {
  return state.hadValue ? setRootString(text, key, state.value ?? '') : removeRootValue(text, key);
}

export function restoreGatewayConfig(text: string, state: CodexRestoreState): string {
  let next = restoreValue(text, MANAGED_KEYS.model, state.model);
  next = restoreValue(next, MANAGED_KEYS.modelCatalogJson, state.modelCatalogJson);
  next = restoreValue(next, MANAGED_KEYS.openaiBaseUrl, state.openaiBaseUrl);
  if (state.guardianReuseParentCompaction) {
    next = state.guardianReuseParentCompaction.hadValue
      ? setTableBoolean(next, 'features', MANAGED_KEYS.guardianReuseParentCompaction, state.guardianReuseParentCompaction.value ?? false)
      : removeTableValue(next, 'features', MANAGED_KEYS.guardianReuseParentCompaction);
  }
  return next;
}
