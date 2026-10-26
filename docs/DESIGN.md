# Wopal Codex Gateway

> **Status**: Active
> **Updated**: 2026-10-26
> **Parent Architecture**: N/A
> **Parent Product**: N/A

## Project Role

Wopal Codex Gateway 是 macOS 上的本地模型网关。
它让 Codex Desktop 和 Codex CLI 在同一个模型选择器、同一个会话里使用官方模型和多个外部 provider。

系统负责四件事：

- 路由模型请求到官方链路或指定外部 provider。
- 维护跨 provider 的会话重放与 compaction。
- 在官方额度进入保护区时自动切到备用模型。
- 管理 Codex 配置、模型目录、Keychain、LaunchAgent 和本地状态。

边界：

- 只服务本机单用户。
- 官方模型保持 bare model name。
- 外部模型默认使用 `<provider-id>/<upstream-model>`。
- 用户可以给单个外部模型设置 alias。
- 外部 provider 的每条模型按自己的上游协议路由：OpenAI Chat、OpenAI Responses、Anthropic Messages 或 Gemini。
- prompt、请求正文、凭证和原始账号标识不写入磁盘。

两个模型用途由用户通过 CLI 指定，不写死在代码里：官方额度保护触发时使用的备用模型（`officialFallback`），以及会话压缩与 Codex memories 使用的模型（`compaction`）。

## Capability Scope

| 能力 | 设计 |
| --- | --- |
| 官方模型 | 原生 HTTP / WebSocket 直通 |
| 外部 provider | `wopal-ai` + 用户配置的 provider |
| 上游协议 | `openai-chat`、`openai-responses`、`anthropic-messages`、`gemini`；协议转换由 CLIProxyAPI 承担 |
| Provider Registry | 统一保存 provider、模型归属和运行时能力 |
| 路由 | 只路由已注册的 client-visible model；官方 bare model 走官方链路 |
| 模型目录 | 官方 bundled catalog + 所有已配置外部模型 |
| 模型属性 | models.dev 元数据；用户显式设置优先，非协议属性缺失时用 Gateway 默认值（protocol 无默认值） |
| 额度保护 | 5 小时 97%、每周 99%；只有额度保护触发自动回退 |
| Compaction | 统一使用 `compaction` role |
| Codex memories | 与 Gateway compaction 共用 `compaction` role |
| 安装与恢复 | 接管指定 Codex 配置并可完整恢复 |
| 可观测性 | `/_health`、`/_status`、metadata-only 日志 |

## Key Decisions

| Decision | Rationale |
| --- | --- |
| 单 Go 二进制 | 控制面和数据面共用配置、测试与生命周期 |
| 外部 provider 复用 CLIProxyAPI | chat、responses、messages、gemini 四种转换都由现有转换器承担 |
| 上游协议是模型属性 | Codex 只接受 Responses；同一个 provider 下模型可能分属四种协议，协议跟着模型走 |
| 协议转换不自己写 | CLIProxyAPI 已注册 Responses 到 chat / messages / gemini 的转换器，网关只负责选路与凭据注入 |
| 模型属性默认取 models.dev | 社区维护、多 harness 采信；比自建属性库更现实。它只做默认值，用户显式设置永远优先 |
| 官方链路不进转换 | `official` 仍走原生 WebSocket，协议转换只发生在外部 provider |
| `official` 是内建 provider | 官方 quota、native WebSocket、native compaction 和客户端凭据转发仍走专用链路 |
| Provider Registry 是运行时真相源 | daemon、目录生成和 CLI 使用同一套 provider / model 解析结果 |
| Route Table 只保存 exact route | namespace 不做运行时通配，减少隐式路由 |
| provider id 同时作为名字空间 | 一个 provider 只有一套命名配置，默认名字由 id 加 upstream model 组成 |
| 特殊模型用途保存 provider + upstream model | alias 改名不改变 role 的后端归属 |
| 只保留两个 role | `officialFallback` 负责额度回退；`compaction` 同时服务 Gateway compaction 与 Codex memories，因此 memories 永不占用官方额度 |
| `runtime/config.json` 保持 version 1 | `wopalAi` 与 `wopal-ai/*` 保持稳定；新增字段继续使用 version 1 |

## Module Architecture

| Module | Responsibility |
| --- | --- |
| `cmd/gateway` | CLI、daemon 入口和依赖装配 |
| `internal/config` | 配置 schema、路径和持久化 |
| `internal/provider` | Provider Registry、ModelRef、模型解析 |
| `internal/routing` | exact Route Table 与请求路由规则 |
| `internal/proxy` | official executor 与 external executor |
| `internal/catalog` | 模型同步、能力解析、Codex catalog 生成 |
| `internal/codex` | Codex 配置接管与恢复 |
| `internal/quota` | 官方额度保护状态机 |
| `internal/compaction` | Gateway compaction、`wgcp1` 与 native restore |
| `internal/keychain` | provider API key 与 Gateway compaction key |
| `internal/launchd` | LaunchAgent 生命周期 |
| `internal/activity` / `internal/clientfp` | metadata 日志与客户端兼容观测 |

`cmd/gateway` 只做装配。
业务规则放在对应的 `internal/` 包中。

## Project Layout

| Directory | Owns |
| --- | --- |
| `internal/` | 网关业务逻辑 |
| `cmd/gateway/`、`bin/`、`launchd/`、`scripts/` | 进程与 CLI 入口、发布用单一可执行文件、LaunchAgent 模板、面向操作者的薄入口 |
| `docs/`、`runtime/`、`logs/`、`recovery/` | 设计文档、用户配置与跨重启状态、metadata 日志、回滚材料 |

`runtime/` 中的文件权限为 `0600`，目录为 `0700`。
正常运行路径不读取 `recovery/`。

## Technical Stack Choices

运行时使用 Go 1.26 单二进制，内嵌 CLIProxyAPI v8.0.29 与 Gin；配置是 JSON version 1，Codex 配置用行级 TOML 编辑，密钥放在 macOS Keychain，服务由 LaunchAgent 管理。

## Interfaces and Contracts

### CLI

命令组：`config`、`auth`、`models`、`catalog`、`codex`、`service`、`quota`。

多 provider 新增 provider 管理与 role 管理命令：

```text
wopal-codex-gateway providers set <provider> --base-url <url> [--base-url-for <protocol>=<url>]... [--models-url <url>] [--models-auth <none|provider-key>] [--header <name>=<value>]... [--protocol <default>] [--tool-mode <mode>]
wopal-codex-gateway providers remove <provider>
wopal-codex-gateway config role
wopal-codex-gateway config role set <official-fallback|compaction> --provider <id> --model <upstream-name>
wopal-codex-gateway config role reset <official-fallback|compaction>
```

`providers set` 是 upsert，输出 `created` 或 `updated`，让用户看清这次是新建还是修改；创建时必须提供 `--base-url`；修改时省略即保持原值。`--base-url-for` 可以重复，用来写某个协议的专用地址，例如 OpenCode Go 的 messages 和 gemini。`--protocol` 是这个 provider 的最后兜底协议，不是必填项；不填时不猜协议，模型必须由单模型配置或同步元数据确定协议。`--models-url` 指定模型清单地址，省略时默认 `baseUrl + /models`。`--header` 可以重复，写入 provider 的非密钥 header 模板，用于向上游注入必需的常量头或 `$CPA-SESSION-ID`；允许的写法见配置规则。`providers remove` 在 role 仍引用它时拒绝执行。

`config role` 不带参数时打印两个 role 的解析结果：provider、upstream model、client model、默认还是显式、ready/unavailable/invalid。它只读配置，不需要 daemon 运行——role 配错导致 `serve` 拒绝启动时，这正是要看的东西。

静态配置也可以直接看 `config show`；运行中的解析状态看 `/_status`。

`auth` 和 `models` 命令接受 `--provider <id>`；省略时作用于 `wopal-ai`。
`models add/set` 支持 `--alias` 与 `--protocol`，`models set` 支持清除 alias；`models list` 输出每个模型的 `protocol` 和来源（`explicit`、`metadata`、`provider-default`、`needs_protocol`）；`config wopal` 继续管理 `wopalAi`。

命名只有两条规则：provider id 决定默认名字空间和显示名，单个模型的 alias 决定短名。需要短名字时配 alias 即可，配置里没有第二套命名维度。

### HTTP

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/responses` | Responses 推理与额度保护回退 |
| `GET /v1/responses` + WebSocket Upgrade | Codex WebSocket 推理 |
| `POST /v1/responses/compact` | Gateway compaction |
| `GET /v1/models` | 已配置 provider 的模型清单；Codex 选择器使用 `catalog refresh` 生成的合并 catalog |
| `GET /_health` | 健康检查 |
| `GET /_status` | 脱敏运行状态 |

### Gateway Config

`runtime/config.json` 保持 version 1。下面只展示本次设计涉及的字段；`listen`、`notifications`、`logs`、`quota` 等字段的语义不变。

```json
{
  "version": 1,
  "wopalAi": {
    "baseUrl": "https://aiapi.wopal.cn/v1",
    "models": [{ "name": "deepseek-v4.1-flash" }]
  },
  "providers": {
    "opencode-go": {
      "baseUrl": "https://opencode.ai/inference/go/openai/v1",
      "baseUrls": {
        "anthropic-messages": "https://opencode.ai/inference/go/anthropic",
        "gemini": "https://opencode.ai/inference/go/google"
      },
      "modelsUrl": "https://opencode.ai/inference/v1/models",
      "headers": {
        "x-opencode-session": "$CPA-SESSION-ID"
      },
      "models": [
        { "name": "kimi-k3", "protocol": "openai-chat" },
        { "name": "gpt-6-luna", "protocol": "openai-responses" },
        { "name": "claude-haiku-5-5", "protocol": "anthropic-messages" },
        { "name": "gemini-4-argon", "protocol": "gemini" },
        { "name": "omen-alpha", "protocol": "openai-chat" }
      ]
    }
  },
  "modelRoles": {
    "officialFallback": { "provider": "opencode-go", "model": "gpt-6-luna" },
    "compaction": { "provider": "wopal-ai", "model": "deepseek-v4.1-flash" }
  }
}
```

配置规则：

- `wopalAi` 固定表示 provider `wopal-ai`。
- `providers` 只保存其他外部 provider；`official` 与 `wopal-ai` 是保留 id。
- 协议只有四个值：`openai-chat`、`openai-responses`、`anthropic-messages`、`gemini`。其他值直接报错。
- 单条模型的协议按“显式 `models set --protocol` → models.dev `provider.shape` → models.dev `provider.npm` → provider `protocol`”解析；都没有时标记 `needs_protocol`，不进入路由表，也不出现在 Codex 选择器里。`wopal-ai` 是兼容例外：省略协议时按 `openai-chat`。
- `baseUrl` 是 provider 的默认上游地址。某个协议的地址不同时，用 `baseUrls[protocol]` 单独指定，CLI 写法是 `--base-url-for <protocol>=<url>`。OpenCode Go 就是这种：chat 与 responses 在 `/go/openai/v1`，messages 在 `/go/anthropic`，gemini 在 `/go/google`。
- `baseUrl` / `baseUrls` 写的是 CLIProxyAPI executor 拼接协议路径之前的根地址，不是最终 endpoint。拼接规则：chat 加 `/chat/completions`；responses 加 `/responses`；messages 加 `/v1/messages?beta=true`；gemini 加 `/v1beta/models/<model>:generateContent`。把最终路径写进 base 会产生重复段，例如 messages 写成 `/go/anthropic/v1` 就会请求到 `/go/anthropic/v1/v1/messages`。
- `modelsUrl` 是模型清单地址，省略时按 `baseUrl + /models`。清单端点和推理端点可以在不同路径：OpenCode 的清单在 `https://opencode.ai/inference/v1/models`，推理分散在 `/go/*` 下。模型发现鉴权由 `modelsAuth` 唯一表达，取值 `none` 或 `provider-key`；默认同 origin 为 `provider-key`（Wopal 即如此），跨 origin 为 `none`。
- 自动发现只覆盖 OpenAI 风格清单：`GET <modelsUrl>`，`provider-key` 发 `Authorization: Bearer <Keychain key>`，响应按 `data[].id` 解析。native Gemini（`/v1beta/models`、`x-goog-api-key`、`models[].name`）和 native Anthropic（`/v1/models`、`anthropic-version`）不参与自动发现，用 `models add` 显式登记。secret 不允许借 `headers` 传递。
- `headers` 只允许两种值：普通字符串字面量，或精确的 `$CPA-SESSION-ID`。其他 `$...` 一律拒绝：CLIProxyAPI 原生支持 `$<任意 client header>`，会把客户端官方凭据送去第三方。
- `headers` 拒绝凭据类与账号路由保留 header 名（大小写不敏感）：`Authorization`、`Proxy-Authorization`、`Cookie`、`x-api-key`、`x-goog-api-key`、`ChatGPT-Account-ID`。密钥只能来自 Keychain。
- provider id 使用小写稳定标识，并生成默认 client-visible name：`<provider-id>/<upstream-model>`。
- `alias` 省略或空字符串都表示没有 alias。
- 非空 alias 是完整 client-visible name，可以不包含 provider id。
- client-visible name 必须全局唯一。手工编辑配置造成重名时，`serve` 直接拒绝启动，不猜测该用哪一个。
- alias 与官方 bundled slug 的冲突在写入 alias 和 `catalog refresh` 时检查。
- 每个外部 provider 可配置 `toolMode` 和 `excludedModels`；tool mode 支持 `code_mode_only`、`code_mode`、`direct`，默认 `code_mode_only`。`excludedModels` 与 `wopal-ai` 同义：列在其中的 upstream model 不进入 `models sync` 结果，也不进入 catalog。
- `modelRoles` 只保存 provider + upstream model，不保存 alias。
- 两个 role 的默认值都指向 `wopal-ai/deepseek-v4.1-flash`。
- 未识别的 version 1 字段继续原样保留。
- 配置写入使用原子 rename，文件权限 `0600`。

## Provider Registry and Routing

运行时把 `wopalAi`、`providers` 和内建 `official` 统一成 Provider Registry。

`official` 使用专用 native executor。

每条模型按自己的 `protocol` 选上游，对 Codex 暴露的接口完全相同：

| protocol | 上游 | CLIProxyAPI 承担 |
| --- | --- | --- |
| `openai-chat` | `POST <base>/chat/completions` | Responses 到 chat 的转换 |
| `openai-responses` | `POST <base>/responses` | 兼容模式：改写请求后走 SSE 聚合 |
| `anthropic-messages` | `POST <base>/v1/messages?beta=true` | Responses 到 Messages 的转换 |
| `gemini` | `POST <base>/v1beta/models/<model>:generateContent` | Responses 到 Gemini 的转换 |

四种都是实际需求：wopal-ai 是 chat，DeepSeek 官方是 responses，OpenCode 一个 provider 里四种同时存在（四种协议已用 OpenCode Go 真实 key 端到端验证通过，见 `research/opencode-multi-protocol-spike.md`）。

协议不是 provider 级单值。编译时同一 provider 的模型按协议分桶，生成最多四个 CLIProxyAPI 上游组，共用同一把 Keychain 密钥。用户眼里仍然是一个 provider、一套模型命名。

外部 `openai-responses` 桶固定编译为兼容模式：model 条目 `is-compat: true`，key 条目 `disable-codex-cloaking: true`。前者保证第三方 Responses 端点收到 portable 形状（不要求 native `agent_message`、不要求空 signature thinking），后者避免把 Codex 内部 cloaking 发给外部上游。两项都是实现细节，不需要用户配置；若将来某个上游明确要求原生 Codex 形状，再单独开例外并补验证。

自己写转换层不在范围内。CLIProxyAPI 已经注册了 Responses 到 chat / messages / gemini 的双向转换、工具与 reasoning 的配对处理、以及 Codex WebSocket 到上游 SSE 的桥接；重复实现会复制它最容易出错的那部分。

外部上游不做 header 全量透传：认证头由 CLIProxyAPI executor 用 Keychain 凭据生成，客户端发来的 `Authorization`、账号标识、Cookie 一律不转发。Gateway 自己不复制任何 client header；受控转发沿用各 executor 已定义集合。CLIProxyAPI 的 custom header 允许 `$<任意 client header>` 且能覆盖认证头，这个能力 Gateway 不使用，只编译受限的 `headers` 模板。

OpenCode 依赖 `x-opencode-session` 做路由与 prompt 缓存，但 Codex 客户端不会带这个头。所以在这类 provider 的 `headers` 里配置 `x-opencode-session: $CPA-SESSION-ID`，由 CLIProxyAPI 按会话生成值，而不是写死常量。

注入落点因 provider 类型而异：`openai-compatibility` 挂 provider 顶层，`codex`/`claude`/`gemini` 挂 per-key。实测挂错位置会静默丢头并导致上游 400，见 `research/opencode-multi-protocol-spike.md`。

协议枚举固定为这四种。新增第五种协议必须同时具备 CLIProxyAPI 转换器、真实上游验证和回归测试，不能靠字符串透传。

Route Table 只保存已经配置的 client-visible model。模型状态分两层，不混用：discovery-enabled provider 的 `/models` 返回的是 **available models**，只用于清单展示与元数据；`providers.<id>.models` 是 **configured models**，只有它进入 Provider Registry、Route Table 和 Codex catalog。外部 provider 的 `models sync` 不自动注册新模型，用户需要 `models add` 才会可路由。

路由顺序：

1. 已注册 external client-visible model → 对应 provider。
2. 其余 bare name 命中已验证的 official slug baseline → official。
3. 其他一律 route error；未知 bare name 不回落到 official。

CLI 变更（provider / model / alias / `catalog refresh`）先原子写配置与 baseline，再向运行中的 daemon 发 SIGHUP。daemon 重建整个 Registry + Route Table + baseline snapshot，构建成功才一次性替换；构建失败保留旧快照，把错误写进 `/_status`。新配置对之后到达的请求立即生效，进行中的请求不受影响，不需要重启进程。daemon 不监听配置文件；直接编辑 `runtime/config.json` 后执行 `service restart` 让改动生效。

provider id 只负责生成默认名字，不是运行时 wildcard。
例如 `openrouter/foo` 只有在 `foo` 已配置时才可路由。

`serve` 只根据本地配置构建 Registry 和 Route Table，不执行 `codex debug models --bundled`。为了仍能兑现 bare alias 与官方 slug 的校验，`catalog refresh` 在写 catalog 的同时把 bundled 官方 slug 集合和 Codex catalog artifact 的 identity 写入 `runtime/official-models.json`。

catalog artifact 指真正编入 bundled models 的 native 可执行文件，不是 npm 启动器。解析只读文件系统，不执行 Node 或 Codex。规则：`codex` 入口是 `codex-cli/bin/codex.js` 这类启动器时，按其 platform package 解析到 `vendor/<target-triple>/bin/codex`；入口本身是 native 二进制时直接使用。identity = canonical path + SHA-256；size、mtime、inode 只做快速比较，不作判据。

校验规则：`serve` 启动时必算 SHA-256，baseline 缺失或哈希不一致时拒绝启动（fail closed），提示先运行 `catalog refresh`：official bare 路由依赖这份 baseline。运行期每次走 bare alias 的 external exact route 前先从当前 `codex` 入口重新解析 authoritative native artifact：canonical path 与 baseline 不同立即 fail closed；路径相同才把 stat tuple 当作已验证哈希的缓存键，stat 变化即重算 SHA-256 并以哈希为准；不一致时所有 bare name 路由立即 fail closed（返回 route error），含 `/` 的 external 路由不受影响，直到重新 `catalog refresh` 并完成一次 reload。这样 Codex 升级新增同名 official slug 时，不会在运行中被 external exact route 静默抢走。

## Model Roles

系统只保留两个 role：

| Role | Used by | Default | Validation |
| --- | --- | --- | --- |
| `officialFallback` | 官方额度保护触发后的执行目标 | `wopal-ai:deepseek-v4.1-flash` | 必须是外部 provider 的模型；四种已注册协议均可，因为对 Codex 暴露的都是 Responses |
| `compaction` | Gateway compaction、Codex memories extract / consolidation | `wopal-ai:deepseek-v4.1-flash` | 必须是外部 provider 的模型；四种已注册协议均可 |

Codex memories 不单独配置：Codex install 把 `compaction` role 解析得到的 client-visible model 写入两个 memories model 字段。

这样 memories 永远走外部模型，不占官方额度。官方额度耗尽时，compaction 与 memories 照常工作。

代价是 memories 不能单独选模型。当前没有这个需求，而单独的 memories role 会多出一套配置、校验和恢复分支。

role 规则：

- `config role set` 在写入前验证 provider 和 model 存在。
- 显式 `officialFallback` 或 `compaction` 悬空时，`serve` 拒绝启动。
- compatibility default 悬空时，普通推理仍可启动；实际使用该 role 时返回 `model_role_unavailable`。
- role 自身执行失败后直接返回错误，不继续套用另一个 role。

### Official Fallback

自动回退只有一个触发来源：官方额度保护。

| Situation | Fallback |
| --- | --- |
| 5 小时或每周额度进入保护区 | 是 |
| 429 | 否 |
| 5xx | 否 |
| 连接失败 | 否 |
| WebSocket 握手失败 | 否 |
| 鉴权失败 | 否 |
| 已经产生输出 | 否 |

除额度保护外，官方链路按原生错误语义返回。

## Model Catalog

`catalog refresh` 先解析 authoritative native artifact 并算 H1，用这一个 native 二进制执行 `debug models --bundled`，输出后再算 H2；只有 H1 = H2 才同时提交 slug 集合、identity 与 catalog。然后用这份 slug 集合合并所有外部模型，生成：

```text
~/.codex/wopal-codex-gateway-models.json
```

模型命名：

- 官方模型保持 bundled bare slug。
- Wopal 保持 `wopal-ai/<model>`。
- 其他 provider 默认是 `<provider-id>/<upstream-model>`。
- 非空 alias 直接成为该模型的 client-visible name。

目录刷新前检查所有 client-visible name 与官方 slug 的碰撞。
冲突时保留现有生成目录并返回错误。

### Model Attributes

模型清单和模型属性来自两个不同的来源。

| 事实 | 来源 | 理由 |
| --- | --- | --- |
| 有哪些模型（available） | discovery-enabled provider 的 `/models` | 只有它知道当前在服务什么；discovery-disabled provider 没有 remote available set |
| 模型是什么 | models.dev | provider 的 `/models` 只回名字 |

`models.dev/catalog.json` 一次给出全部 provider 与其模型属性，带 `ETag`，日常刷新用 `If-None-Match` 取 304。拉取失败时保留上一份，不阻断其他 provider 同步。

只保存 join 之后的切片，不落全量 6 MB。

### Protocol Resolution

`/v1/models` 不返回协议。models.dev 的模型条目可以带 `provider.shape`（`responses` / `completions`）和 `provider.npm`，都优先于 provider 级 `npm`。

| 来源（按序） | 协议 |
| --- | --- |
| 模型 `provider.shape = responses` | `openai-responses` |
| 模型 `provider.shape = completions` | `openai-chat` |
| `@ai-sdk/openai` | `openai-responses` |
| `@ai-sdk/openai-compatible` | `openai-chat` |
| `@ai-sdk/anthropic` | `anthropic-messages` |
| `@ai-sdk/google` | `gemini` |

`shape` 必须最先看：`@ai-sdk/openai-compatible` 的 provider 里已经有模型用 `shape = responses` 提供 Responses（models.dev 的 `sakana/fugu`）。npm 先看模型级 `provider.npm`，再看 provider 级 `npm`。

未映射的 npm（例如 `@ai-sdk/mistral`、`@ai-sdk/xai`）标记 `needs_protocol`，等待显式设置。

### Multi-API Providers

同一个 provider 经常同时提供多种 API。DeepSeek 官方既接受 `POST /responses` 也接受 `POST /chat/completions`，两类请求都返回 401 而不是 404，说明两个端点都真实存在。

models.dev 每个模型只记一种协议，它选的是自己认为最合适的那条。这个选择会过时，也会判错。

因此网关按单协议路由：一条模型同一时刻只走一条上游路径，但用户可以随时改用另一条。DeepSeek 官方文档给 Codex 的接入配置就是 `wire_api = "responses"`、`base_url = "https://api.deepseek.com/"`，网关按同样的方式走 Responses：

```bash
wopal-codex-gateway providers set deepseek --base-url https://api.deepseek.com
wopal-codex-gateway models set deepseek-v4-pro --provider deepseek --protocol openai-responses
```

要改走 chat completions，把 `--protocol` 换成 `openai-chat`。

网关不因为请求失败自动换协议。4xx 可能是鉴权、限流、参数或模型下线；自动切换会制造重复计费和不可解释的路由变化。

### Attribute Precedence

context、vision、reasoning 这类能力属性单独解析：

| 顺序 | 来源 | 标记 |
| --- | --- | --- |
| 1 | 用户显式设置 | `explicit` |
| 2 | models.dev | `metadata` |
| 3 | Gateway 默认值 | `default` |

协议不走这张表，也没有默认值：`explicit` → models.dev（先 `shape` 再 `npm`）→ provider `protocol` → `needs_protocol`。落到 `needs_protocol` 的模型不进路由表，也不出现在 Codex 选择器。

`wopal-ai` 在 models.dev 没有对应条目，不参与 models.dev 合成。它使用独立 metadata source，优先级：用户显式设置 > Wopal 服务端 `/models` 的明确属性 > Gateway 默认值；协议省略时按 `openai-chat`。它的元数据读写 `runtime/model-metadata.json`，并遵守用户显式设置优先于服务端返回属性。两个 store 的边界写死为：`wopal-ai` 只读写 `runtime/model-metadata.json`，不写 `runtime/provider-model-metadata.json`；后者只保存其他外部 provider。两个 store 各自只有一个拥有者，不存在同一 provider 两份快照需要按 `fetchedAt` 仲裁的情况。

### Capability Defaults

能力字段的默认值：100 万 tokens 上下文、支持图片、支持推理。

models.dev 有明确值时用它的值（如 `limit.context`、`modalities.input`），并在 metadata snapshot 里带版本锚点。provider 只回模型名时，不回落到猜测；模型名、`owned_by`、创建时间和名称后缀都不是能力证据。

### Sync Rules

- 用户显式设置永不自动覆盖。models.dev 与显式值冲突时打印 warning，保留用户值。
- models.dev 来源的值在同步时更新，并打印 `protocol changed: old -> new`。
- `models sync` 对 `providers` 里的外部 provider 只写元数据 store，不改写配置里的显式字段。对 discovery-disabled provider（native Gemini / native Anthropic），它只刷新已 configured models 的 models.dev 元数据，不调用模型清单端点，也不新增模型；登记始终靠 `models add`。省略 `--provider` 时作用于 `wopal-ai`：拉取 Wopal `/models` → 保留用户显式 override → 更新 `wopalAi.models` → 写 `runtime/model-metadata.json` → refresh catalog。这是兼容例外，不受外部 provider 规则约束。
- 拉取失败或解析失败保留上一次成功结果；没有上一次结果就只用用户设置和默认值。
- discovery-enabled provider：models.dev 有而 `/models` 没有的模型不进入清单；`/models` 有而 models.dev 没有的模型进入清单并标 `needs_protocol`。discovery-disabled provider 的清单只来自 configured models。

### Metadata Staleness

models.dev 是社区维护的 MIT 项目，不是权威 API 文档：DeepSeek 被标成纯 chat，官方却同时提供 Responses 和 chat completions；OpenCode 的 `mistral-large-4` 标成 `@ai-sdk/mistral`，实际走 `/openai/v1`。所以它只是默认来源，用户显式设置永远能盖掉它。

## Credentials

- `official` 使用 Codex 请求携带的官方凭据。
- `wopal-ai` 使用固定 Keychain service，account 为 `wopal-ai`。
- 其他 provider 使用统一 service + provider id account。一个 provider 一把密钥，多种协议共用。
- `auth test --provider` 对 discovery-enabled provider 请求其 `modelsUrl`；discovery-disabled provider 只报告 Keychain 中是否已配置凭据，不发起网络请求。
- 凭据呈现完全由对应 CLIProxyAPI executor 决定，Gateway 只提供 Keychain key，不在设计层重新概括 header。参考行为：OpenAI-compatible / Codex 用 `Authorization: Bearer`；Claude 在官方 Anthropic API-key 端点用 `x-api-key`，在第三方 / custom base 上用 `Authorization: Bearer`；Gemini 用 `x-goog-api-key`。Spike 已证明 OpenCode Go 的 Gemini 路径在 `x-goog-api-key` 下可用。`headers` 只做非密钥增值头；若上游要求非标准 secret header，必须单独设计 Keychain-backed binding，禁止把 secret 写进 `headers`。
- `wgcp1` 使用 Gateway 自己的 compaction key。
- provider API key 不进入 JSON、SDK runtime config 或日志。

## Codex Integration and Restore

Gateway 只管理四个 Codex 配置项：

- `model_catalog_json`
- `openai_base_url`
- `[memories] extract_model`
- `[memories] consolidation_model`

`model` 不是安装时固定写入的 managed key。

恢复状态记录 Gateway 接管期间暴露过的 external client-visible model。
每次成功 `catalog refresh` 都把本次外部模型名称并入集合，集合只增不减。

`codex restore` 时：

- 当前 model 在该集合中 → 恢复安装前 model。
- 当前 model 是官方模型 → 保留用户当前选择。

这样可以覆盖安装之后新增、删除或改名 alias 的场景。

## Rollback

回滚必须同时恢复 daemon 和 Codex-facing 状态。

固定顺序：

1. 当前版本执行 `codex restore`。
2. 启动 recovery 中的上一版，直到 `/_health` 通过。
3. 上一版执行 `codex install`，重新生成它能理解的 catalog 和 memories model。

任何一步失败都停止后续步骤。
成功回滚后，第三方 bare alias 不得继续出现在 Codex catalog 或当前 model 中。

## Client Compatibility Logging

日志只记录诊断所需的 metadata。

- 记录 Codex 版本、originator、User-Agent、Content-Encoding、beta feature 标识；请求体只提取字段名、工具数量、工具名称摘要和输入类型计数。
- 同一客户端版本的请求形状变化记录一次 contract-change 事件。
- prompt、工具参数、工具输出、完整请求正文和凭证永不写盘。

SDK request logger 保持关闭。
`/_status.request_body_logging` 固定报告 `disabled`。

## Quota Protection

官方额度保护合同：

- 5 小时基础停止点：97%。
- 每周基础停止点：99%。
- 学习到的最大单步只会让实际停止点更早。
- 额度窗口 reset time 允许小幅抖动，不因秒级差异丢弃整段 telemetry。
- `/_status` 同时暴露基础状态、实际停止点、学习值和 telemetry 新鲜度。

额度进入保护区后使用 `officialFallback`。
额度恢复后回到用户原本请求的官方模型。

## Status

`/_status` 报告 provider、两个 role、官方 quota/failover、credential 是否配置以及服务状态。
状态输出不包含 API key、prompt 或原始账号标识。

## Data and State Model

| State | Location | Owner |
| --- | --- | --- |
| Gateway config | `runtime/config.json` | CLI + daemon |
| SDK runtime config | `runtime/cliproxy-runtime.json` | daemon |
| SDK auth directory | `runtime/cliproxy-auth/` | daemon |
| Provider metadata（除 wopal-ai 外的外部 provider） | `runtime/provider-model-metadata.json` | CLI |
| Wopal metadata（仅 wopal-ai） | `runtime/model-metadata.json` | CLI |
| Official slug baseline + catalog artifact identity（serve 校验用） | `runtime/official-models.json` | CLI（catalog refresh 写） |
| Codex restore state | `runtime/codex-restore.json` | CLI |
| Official quota state | `runtime/official-quota-state.json` | daemon |
| Model catalog | `~/.codex/wopal-codex-gateway-models.json` | CLI |
| Logs | `logs/*.log` | daemon |
| Rollback material | `recovery/` | operator |

所有用户配置和跨重启状态都使用原子写入。
密钥只存在 Keychain。

## Compatibility Contract

- `runtime/config.json` 保持 `version: 1`。
- `wopalAi` 字段语义保持不变，它下面的模型全部是 `openai-chat`，升级时不需要补 `protocol`。
- `wopalAi` 省略 `protocol` 时按 `openai-chat` 处理；`providers` 缺少协议且没有模型元数据时标记 `needs_protocol`，不猜。
- Wopal client model 保持 `wopal-ai/<model>`。
- `auth` 与 `models` 省略 `--provider` 时继续操作 `wopal-ai`。
- 官方模型保持 bare slug 和原生链路。
- 回滚后的上一版只暴露它自己能路由的官方 + Wopal 模型。

## Reference Documents

| Document | Purpose |
| --- | --- |
| `../README.zh-CN.md` | 中文使用说明 |
| `../README.md` | English user guide |
| `research/opencode-multi-protocol-spike.md` | OpenCode 四协议接入的实测记录与配置差异证据 |
