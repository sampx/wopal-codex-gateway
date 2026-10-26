# OpenCode 多协议接入实测（Spike）

- 日期：2026-10-26
- 目的：在写任何网关代码之前，先验证「CLIProxyAPI 能否把 OpenCode 的四种协议统一转换成 Codex 能用的 Responses」。
- 结论：**可行。四种协议端到端实测全部打通，且发现了一个必须写进设计的配置细节。**

## 一句话结论

OpenCode Go 本身就是一个多协议网关：同一个入口（`https://opencode.ai/inference/go/`）同时提供四种协议的端点，统一用 Bearer 鉴权。CLIProxyAPI 把 Codex 发来的 Responses 请求翻译成任意一种协议转发过去，四条路都通了。

## 实测怎么做的

1. 用一把真实 API key（存 Keychain，不落盘）直连 OpenCode Go，逐个验证四种协议端点。
2. 用 CLIProxyAPI 起一个临时本地网关，把同一个模型（`kimi-k3`）配置到三种协议路径、`gemini-4-argon` 配到 Gemini 路径。
3. 从本地网关统一用 Codex 的 `/v1/responses` 形状发请求，看四条路是否都返回正确文本。

四个 alias 全部返回 HTTP 200，输出文本都是 `OK`。

## 关键事实

### 端点与鉴权

| 协议 | 端点 | 鉴权 |
| --- | --- | --- |
| chat completions | `POST /inference/go/openai/v1/chat/completions` | Bearer |
| responses | `POST /inference/go/openai/v1/responses` | Bearer |
| anthropic messages | `POST /inference/go/anthropic/v1/messages` | Bearer |
| gemini | `POST /inference/go/google/v1beta/models/<model>:generateContent` | Bearer |

四种协议统一 `Authorization: Bearer <key>`。Gemini 不需要 `x-goog-api-key`（带了也不报错）。

### 会话头是硬性要求

所有请求必须带 `x-opencode-session`，否则返回 400：

```json
{"type":"error","error":{"type":"MissingSessionID","message":"..."}}
```

这个头只用于 OpenCode 侧的路由优化（session 粘同一实例），内容用固定字符串即可，OpenCode 不校验格式。Gemini 走 CLIProxyAPI 时同样带上了这个头，同样工作。

### 模型 × 协议是矩阵关系

同一模型可以走多条协议路径，但不是所有模型都支持所有协议：

| 现象 | 实测 |
| --- | --- |
| 一个模型多条协议都通 | `kimi-k3` 走 chat / responses / messages 都返回 200 |
| 模型拒绝某条协议 | `gpt-5.6-luna` 走 chat 返回 `ModelProtocolUnsupported`，只支持 responses |
| 模型在套餐里不可用 | claude / grok / qwen 系列返回 `Model is unavailable`（与协议无关，换协议也一样） |
| 地区限制 | `gpt-5.6-luna` 返回 403 `unsupported_country_region_territory`（上游 OpenAI 的限制） |

这验证了设计里「协议是模型属性、不是 provider 单值」的判断。同时说明：**协议配错了 OpenCode 会明确报错**（不会静默错路由），加上 models.dev 元数据做默认值、报错后用户显式改，这个闭环是稳的。

### 流式响应可用

responses 端点 `stream: true` 返回标准 SSE（`response.created` → `response.in_progress` → …），Codex 的流式消费不需要额外适配。

## CLIProxyAPI 的配置细节（重要发现）

给上游注入 `x-opencode-session` 头时，CLIProxyAPI 的 `headers` 配置位置**因 provider 类型而异**：

| provider 类型 | headers 挂在哪 | 实测 |
| --- | --- | --- |
| `openai-compatibility` | **provider 顶层**（`OpenAICompatibility.headers`） | 挂在 per-key 上不生效，MissingSessionID 直接透传 |
| `codex` | per-key（`keys[].headers`） | 生效 |
| `claude` | per-key（`keys[].headers`） | 生效 |
| `gemini` | per-key（`keys[].headers`） | 生效 |

最小可用配置示例（完整样例见 `.wopal-space/.tmp/opencode-spike/config.template.yaml`）：

```yaml
api-keys:
  openai-compatibility:
    - name: "opencode-chat"
      base-url: "https://opencode.ai/inference/go/openai/v1"
      headers:
        x-opencode-session: "codex-spike"
      keys:
        - api-key: "..."
  codex:
    - name: "opencode-responses"
      base-url: "https://opencode.ai/inference/go/openai/v1"
      keys:
        - api-key: "..."
          disable-codex-cloaking: true
          headers:
            x-opencode-session: "codex-spike"
```

这个差异对网关编译配置的生成逻辑有直接影响：按协议分桶生成上游组时，不同协议桶的 headers 落点不同。

## models.dev 元数据与 OpenCode 现状的偏差

以 OpenCode Go 为例（2026-10-26 实测）：

- live `/models` 返回 85 个模型（`https://opencode.ai/inference/v1/models`，无需鉴权）。
- models.dev 的 `opencode-go` 条目约 37 个模型，部分协议标注与官方文档对齐，部分已过期。
- live 有、models.dev 没有的模型（如 `grok-4.7`）不猜协议，标记 `needs_protocol`，等用户显式设置——这正是设计里 Sync Rules 的行为，实测合理。

## 对设计的启示

1. **方案成立**：CLIProxyAPI 的转换能力覆盖 OpenCode 全部四种协议，不需要自研转换层。
2. **必须新增的设计约束**：CLIProxyAPI headers 落点差异要写进「配置编译」一节，否则 `openai-compatibility` 桶会踩 MissingSessionID。
3. **错误处理可以依赖上游**：协议不支持、套餐不可用、地区限制，OpenCode 都返回结构化错误且可区分，网关按原样透传给 Codex 即可，不需要网关层猜。
4. **`x-opencode-session` 从客户端透传**：Codex 客户端没有这个头，所以网关要从 provider 配置注入；注入规则见上表。

## 复现方式

Spike 脚本保留在 `.wopal-space/.tmp/opencode-spike/`：

- `config.template.yaml` — 四协议配置模板（key 用占位符，从 Keychain 注入）
- `run.sh` — 起临时网关，退出自动清理
- `probe.sh` — 四协议各打一次 `/v1/responses`
- `README.md` — 结论摘要

API key 只存 Keychain（`security add-generic-password -s opencode-spike -a opencode`），不进仓库、不进对话。

