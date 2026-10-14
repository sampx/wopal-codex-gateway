export interface ExternalModelConfig {
  name: string;
  contextWindow?: number;
  vision?: boolean;
  reasoning?: boolean;
}

export interface GatewayConfig {
  listen: { host: string; port: number };
  wopalAi: { baseUrl: string; models: ExternalModelConfig[]; excludedModels: string[] };
  ollama: { baseUrl: string };
}

export interface GatewayServerOptions {
  wopalAiBaseUrl: string;
  ollamaBaseUrl: string;
  chatgptBaseUrl?: string;
  openaiBaseUrl?: string;
  getWopalApiKey: () => Promise<string>;
  activityLogPath?: string;
}
