export interface RestoreValue {
  hadValue: boolean;
  value?: string;
}

export interface CodexRestoreState {
  model: RestoreValue;
  modelCatalogJson: RestoreValue;
  openaiBaseUrl: RestoreValue;
  memoriesSectionExisted?: boolean;
  memoryExtractModel?: RestoreValue;
  memoryConsolidationModel?: RestoreValue;
}

const MANAGED_KEYS = {
  model: 'model',
  modelCatalogJson: 'model_catalog_json',
  openaiBaseUrl: 'openai_base_url',
} as const;

const MEMORY_SECTION = 'memories';
const MEMORY_MODEL = 'wopal-ai/deepseek-v4.1-flash';

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

function sectionRange(lines: string[], section: string): { header: number; start: number; end: number } | undefined {
  const escaped = section.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const header = lines.findIndex((line) => new RegExp(`^\\s*\\[${escaped}\\]\\s*(?:#.*)?(?:\\n)?$`).test(line));
  if (header < 0) return undefined;
  let end = header + 1;
  while (end < lines.length && !/^\s*\[/.test(lines[end]!)) end += 1;
  return { header, start: header + 1, end };
}

function findSectionAssignment(lines: string[], section: string, key: string): { index: number; match: RegExpMatchArray } | undefined {
  const range = sectionRange(lines, section);
  if (!range) return undefined;
  const regex = assignmentRegex(key);
  for (let index = range.start; index < range.end; index += 1) {
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

function getSectionString(text: string, section: string, key: string): RestoreValue {
  const found = findSectionAssignment(splitLines(text), section, key);
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

function setSectionString(text: string, section: string, key: string, value: string): string {
  const lines = splitLines(text);
  const found = findSectionAssignment(lines, section, key);
  if (found) {
    const [, prefix, , suffix, newline] = found.match;
    lines[found.index] = `${prefix}${quoteTomlString(value)}${suffix}${newline}`;
    return lines.join('');
  }

  const range = sectionRange(lines, section);
  if (range) {
    let insertion = range.end;
    while (insertion > range.start && /^\s*$/.test(lines[insertion - 1]!)) insertion -= 1;
    lines.splice(insertion, 0, `${key} = ${quoteTomlString(value)}\n`);
    return lines.join('');
  }

  const separator = text.length === 0 || text.endsWith('\n\n') ? '' : text.endsWith('\n') ? '\n' : '\n\n';
  return `${text}${separator}[${section}]\n${key} = ${quoteTomlString(value)}\n`;
}

function removeRootValue(text: string, key: string): string {
  const lines = splitLines(text);
  const found = findRootAssignment(lines, key);
  if (!found) return text;
  lines.splice(found.index, 1);
  return lines.join('');
}

function removeSectionValue(text: string, section: string, key: string): string {
  const lines = splitLines(text);
  const found = findSectionAssignment(lines, section, key);
  if (!found) return text;
  lines.splice(found.index, 1);
  return lines.join('');
}

function removeSectionIfEmpty(text: string, section: string): string {
  const lines = splitLines(text);
  const range = sectionRange(lines, section);
  if (!range) return text;
  const hasContent = lines.slice(range.start, range.end).some((line) => {
    const trimmed = line.trim();
    return trimmed !== '' && !trimmed.startsWith('#');
  });
  if (hasContent) return text;
  let start = range.header;
  if (start > 0 && /^\s*$/.test(lines[start - 1]!)) start -= 1;
  lines.splice(start, range.end - start);
  return lines.join('');
}

export function captureRestoreState(text: string): CodexRestoreState {
  const lines = splitLines(text);
  return {
    model: getRootString(text, MANAGED_KEYS.model),
    modelCatalogJson: getRootString(text, MANAGED_KEYS.modelCatalogJson),
    openaiBaseUrl: getRootString(text, MANAGED_KEYS.openaiBaseUrl),
    memoriesSectionExisted: sectionRange(lines, MEMORY_SECTION) !== undefined,
    memoryExtractModel: getSectionString(text, MEMORY_SECTION, 'extract_model'),
    memoryConsolidationModel: getSectionString(text, MEMORY_SECTION, 'consolidation_model'),
  };
}

export function mergeGatewayRestoreState(saved: CodexRestoreState, current: CodexRestoreState): CodexRestoreState {
  return {
    ...saved,
    memoriesSectionExisted: saved.memoriesSectionExisted ?? current.memoriesSectionExisted,
    memoryExtractModel: saved.memoryExtractModel ?? current.memoryExtractModel,
    memoryConsolidationModel: saved.memoryConsolidationModel ?? current.memoryConsolidationModel,
  };
}

export function applyGatewayConfig(text: string, modelCatalogPath: string, gatewayBaseUrl: string): string {
  let next = setRootString(text, MANAGED_KEYS.modelCatalogJson, modelCatalogPath);
  next = setRootString(next, MANAGED_KEYS.openaiBaseUrl, gatewayBaseUrl);
  next = setSectionString(next, MEMORY_SECTION, 'extract_model', MEMORY_MODEL);
  next = setSectionString(next, MEMORY_SECTION, 'consolidation_model', MEMORY_MODEL);
  return next;
}

function restoreValue(text: string, key: string, state: RestoreValue): string {
  return state.hadValue ? setRootString(text, key, state.value ?? '') : removeRootValue(text, key);
}

function restoreSectionValue(text: string, section: string, key: string, state: RestoreValue): string {
  return state.hadValue ? setSectionString(text, section, key, state.value ?? '') : removeSectionValue(text, section, key);
}

export function restoreGatewayConfig(text: string, state: CodexRestoreState): string {
  let next = restoreValue(text, MANAGED_KEYS.model, state.model);
  next = restoreValue(next, MANAGED_KEYS.modelCatalogJson, state.modelCatalogJson);
  next = restoreValue(next, MANAGED_KEYS.openaiBaseUrl, state.openaiBaseUrl);
  if (state.memoryExtractModel) next = restoreSectionValue(next, MEMORY_SECTION, 'extract_model', state.memoryExtractModel);
  if (state.memoryConsolidationModel) next = restoreSectionValue(next, MEMORY_SECTION, 'consolidation_model', state.memoryConsolidationModel);
  if (state.memoriesSectionExisted === false) next = removeSectionIfEmpty(next, MEMORY_SECTION);
  return next;
}
