export function normalizeOllamaRequest(body: Buffer): Buffer {
  if (body.length === 0) return body;
  const payload = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
  if (!Array.isArray(payload.input)) return body;

  const normalized: unknown[] = [];
  for (const raw of payload.input) {
    if (!raw || typeof raw !== 'object') continue;
    const item = raw as Record<string, unknown>;
    const type = typeof item.type === 'string' ? item.type : (typeof item.role === 'string' ? 'message' : '');
    switch (type) {
      case 'message': {
        if (item.role === 'developer') normalized.push({ ...item, role: 'system' });
        else normalized.push(item);
        break;
      }
      case 'function_call':
      case 'function_call_output':
      case 'tool_search_call':
      case 'tool_search_output':
      case 'compaction_trigger':
        normalized.push(item);
        break;
      case 'custom_tool_call': {
        const input = typeof item.input === 'string' ? item.input : '';
        normalized.push({
          ...(typeof item.id === 'string' ? { id: item.id } : {}),
          type: 'function_call',
          call_id: item.call_id,
          name: item.name,
          arguments: JSON.stringify({ input }),
        });
        break;
      }
      case 'custom_tool_call_output':
        normalized.push({ type: 'function_call_output', call_id: item.call_id, output: item.output });
        break;
      case 'agent_message': {
        const author = typeof item.author === 'string' ? item.author : '';
        const recipient = typeof item.recipient === 'string' ? item.recipient : '';
        const content = Array.isArray(item.content) ? item.content : [];
        if (!author || !recipient || content.length === 0) break;
        const parts: unknown[] = [{ type: 'input_text', text: `Agent message from "${author}" to "${recipient}":\n` }];
        for (const part of content) {
          if (!part || typeof part !== 'object') continue;
          const value = part as Record<string, unknown>;
          if (value.type === 'input_text' && typeof value.text === 'string') parts.push(value);
          if (value.type === 'encrypted_content' && typeof value.encrypted_content === 'string') {
            parts.push({ type: 'input_text', text: value.encrypted_content });
          }
        }
        normalized.push({ type: 'message', role: 'user', content: parts });
        break;
      }
      case 'reasoning':
      case 'compaction':
        // Provider-specific opaque state is not portable across model backends.
        break;
      default:
        // Ignore unsupported control/history items rather than failing the whole turn.
        break;
    }
  }
  payload.input = normalized;
  return Buffer.from(JSON.stringify(payload));
}
