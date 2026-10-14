export interface ExternalModelConfig {
  name: string;
  contextWindow?: number;
  vision?: boolean;
  reasoning?: boolean;
}

export interface GatewayConfig {
  listen: { host: string; port: number };
  wopalAi: { baseUrl: string; models: ExternalModelConfig[] };
  ollama: { baseUrl: string };
}
