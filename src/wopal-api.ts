export async function fetchWopalModels(
  baseUrl: string,
  apiKey: string,
  fetchImpl: typeof fetch = fetch,
): Promise<string[]> {
  const url = `${baseUrl.replace(/\/+$/, '')}/models`;
  const response = await fetchImpl(url, {
    headers: {
      accept: 'application/json',
      authorization: `Bearer ${apiKey}`,
    },
  });
  const text = await response.text();

  if (!response.ok) {
    throw new Error(`Wopal AI validation failed: HTTP ${response.status}${errorDetail(text)}`);
  }

  let payload: unknown;
  try {
    payload = JSON.parse(text);
  } catch {
    throw new Error('Wopal AI validation failed: /models did not return JSON');
  }

  const data = (payload as { data?: unknown }).data;
  if (!Array.isArray(data)) {
    throw new Error('Wopal AI validation failed: /models response is missing data[]');
  }

  return data.flatMap((item) => {
    const id = (item as { id?: unknown })?.id;
    return typeof id === 'string' && id.trim() ? [id.trim()] : [];
  });
}

function errorDetail(text: string): string {
  if (!text.trim()) return '';
  try {
    const payload = JSON.parse(text) as { error?: { message?: unknown }; message?: unknown };
    const message = payload.error?.message ?? payload.message;
    return typeof message === 'string' && message.trim() ? `: ${message.trim()}` : '';
  } catch {
    return '';
  }
}
