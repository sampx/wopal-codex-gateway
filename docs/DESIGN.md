# Wopal Codex Gateway

> **Status**: Active
> **Updated**: 2026-10-24
> **Parent Architecture**: N/A
> **Parent Product**: N/A
> **Sub-DESIGNs**:
> - `./DESIGN-model-catalog.md` — 模型目录与能力来源

## Project Role

Wopal Codex Gateway 是 macOS 上的本地模型路由网关。它让 Codex Desktop 和 Codex CLI 在同一个模型选择器、同一个会话里使用官方模型与 `wopal-ai/*` 模型。

它拥有四类职责：

- 协议路由：官方 HTTP/WebSocket 直通，Wopal AI 的 HTTP/WebSocket 适配。
- 会话连续性：跨 provider 的会话历史、compaction 与重放。
- 额度保护：官方额度进入保护区后自动 failover，reset 后 failback。
- 本地控制面：Codex 配置接管与恢复、模型目录、Keychain、LaunchAgent、服务生命周期。

边界：

- 只服务本机单用户，不暴露公网多租户 API。
- 模型命名空间只有 official 和 `wopal-ai/*`。
- 不记录 prompt、完整请求正文、凭证或原始账号标识。
- 不实现模型训练、通用 provider 协议或 Ollama 运行时。

## Capability Scope

| 能力 | 目标状态 |
| --- | --- |
| 官方模型 | 无命名空间；HTTP/WebSocket native pass-through |
| Wopal AI | `wopal-ai/<model>`；HTTP/SSE 与 Codex WebSocket 桥接 |
| 模型目录 | 合并 Codex bundled catalog 与配置中的 Wopal 模型 |
| 额度保护 | 5 小时 97%、每周 99%；按观测最大单步提前停；failover/failback |
| 会话压缩 | 新的 compaction 由 `wopal-ai/deepseek-v4.1-flash` 生成 `wgcp1` capsule |
| Native compaction 恢复 | 官方 capsule 在切到 Wopal 前 materialize；超时即失败 |
| 安装与恢复 | 接管 Codex 指定配置，记录原值，可逆恢复 |
| 服务管理 | 单二进制 `serve/install/restore/service`；LaunchAgent 自启动 |
| 可观测性 | `/_health`、`/_status`、metadata-only 日志 |

## Key Decisions

| Decision | Rationale |
| --- | --- |
| 单 Go 二进制同时拥有控制面和数据面 | 一套工具链、一套配置、一套测试；运行时不依赖 Node |
| 内嵌 CLIProxyAPI v8 SDK | 复用 Responses 与 OpenAI-compatible provider 适配，保持单进程 |
| 5 小时 97%、每周 99%，叠加观测最大单步 | 在整数百分比 telemetry 下仍保留最后 1% 额度 |
| 日志只记录 metadata | 本地隐私边界；prompt 和请求正文不进入持久文件 |
| `model` 仅在 Gateway 提供的模型上恢复 | 保留用户安装后新选的官方模型 |
| 配置带版本号并由单一 Go 包读写 | CLI 与 daemon 共享同一契约，非法配置在启动前失败 |

## Module Architecture

| Module | Responsibility | Carrier |
| --- | --- | --- |
| `cmd/gateway` | 子命令解析、依赖装配、进程入口 | `cmd/gateway/` |
| `internal/config` | 版本化配置的读取、写入、校验 | `internal/config/` |
| `internal/codex` | `config.toml` 的接管、恢复、managed-key 规则 | `internal/codex/` |
| `internal/catalog` | bundled catalog 读取与 Wopal 模型合并 | `internal/catalog/` |
| `internal/keychain` | macOS Keychain 读写 | `internal/keychain/` |
| `internal/launchd` | plist 渲染、launchctl、健康等待 | `internal/launchd/` |
| `internal/routing` | 模型命名空间分类与无状态请求工具；只依赖标准库 | `internal/routing/` |
| `internal/proxy` | 路由 middleware、官方直通、Wopal HTTP/WS | `internal/proxy/` |
| `internal/quota` | 额度窗口、阈值、failover 状态机 | `internal/quota/` |
| `internal/compaction` | `wgcp1`、native restore、官方边界展开 | `internal/compaction/` |
| `internal/notify` | 用户通知 | `internal/notify/` |
| `internal/activity` | JSONL metadata 日志 | `internal/activity/` |

`cmd/gateway` 只做装配。业务规则、协议处理、状态机各自拥有独立包和测试入口。

## Technical Stack Choices

| Domain | Choice | Rationale | Boundary |
| --- | --- | --- | --- |
| Language | Go 1.26 | 单二进制、并发、已有 daemon | 运行时不包含 Node |
| Provider adapter | CLIProxyAPI v8.0.29 | 复用 Responses/OpenAI-compatible 适配 | 编译期内嵌，版本固定 |
| HTTP server | Gin via SDK | 与 provider adapter 共用监听 | 只有一个 listener |
| Gateway config | JSON `runtime/config.json` version 1 | CLI 与 daemon 共享 | 只有一套 parser |
| Codex config | 行级 TOML 编辑器 | 保留注释与无关配置 | 只改 managed keys |
| Secret | macOS Keychain | 密钥不落盘 | 仅 macOS |
| Service | LaunchAgent | 用户级自启动 | 单一 label |
| Logs | JSONL metadata | 本地可观测 | 不记录 prompt/正文 |
| Tests | `go test` | 单一工具链 | 不包含 Node 测试 |

## Interfaces and Contracts

### CLI

```text
wopal-codex-gateway serve
wopal-codex-gateway config show
wopal-codex-gateway config migrate --from <existing.json> --to <new.json>
wopal-codex-gateway config wopal --base-url <url> --model <name>...
wopal-codex-gateway auth set
wopal-codex-gateway auth test
wopal-codex-gateway auth status
wopal-codex-gateway models list
wopal-codex-gateway models inspect [model...]
wopal-codex-gateway models sync
wopal-codex-gateway models add <name>...
wopal-codex-gateway models set <name>...
wopal-codex-gateway models remove <name>...
wopal-codex-gateway catalog refresh
wopal-codex-gateway codex install
wopal-codex-gateway codex restore
wopal-codex-gateway service install
wopal-codex-gateway service start
wopal-codex-gateway service stop
wopal-codex-gateway service restart
wopal-codex-gateway service status
wopal-codex-gateway service uninstall
wopal-codex-gateway quota reset-learning
```

`serve` 是唯一 daemon 入口。`quota reset-learning` 清除已观测最大单步，并写一条 metadata 日志；状态文件尚不存在时按“已无学习值”成功返回。`config`、`auth`、`models`、`catalog`、`codex`、`service`、`quota` 都是同一个二进制上的子命令组。`service install` 与 `codex install` 在报告成功前都必须先确认配置端口已释放，再通过 `/_health`。

### HTTP

| 端点 | 用途 |
| --- | --- |
| `POST /v1/responses` | Responses 推理与 HTTP failover |
| `GET /v1/responses` + WebSocket Upgrade | Codex WebSocket 推理 |
| `POST /v1/responses/compact` | compaction 兼容端点 |
| `GET /v1/models` | 模型目录 API |
| `GET /_health` | 进程健康检查 |
| `GET /_status` | 脱敏后的运行时与额度状态 |

### Gateway config

`runtime/config.json` 的 schema：

```json
{
  "version": 1,
  "listen": { "host": "127.0.0.1", "port": 11435 },
  "wopalAi": {
    "baseUrl": "https://example.com/v1",
    "toolMode": "code_mode_only",
    "models": [
      { "name": "deepseek-v4.1-flash" }
    ],
    "excludedModels": []
  },
  "notifications": { "enabled": true, "sound": "default", "tool": "" },
  "logs": { "maxSizeMb": 10, "maxBackups": 5 },
  "quota": { "maxStepCap": 5 }
}
```

规则：

- `version` 必须是 `1`；不支持的版本在启动前失败。`config migrate` 从无版本号或版本 1 的配置生成单独候选文件，保留现有模型覆盖并移除 `ollama` 段；源文件保持原样，目标文件已存在时拒绝覆盖。
- `listen.host` 默认 `127.0.0.1`，`listen.port` 默认 `11435`。
- `wopalAi.models` 是外部模型配置来源；`excludedModels` 只影响 `models sync`。
- 模型能力按用户覆盖、provider 明确返回值、网关默认值的顺序解析；默认上下文为 1,000,000 tokens，支持图片和推理。来源、同步与覆盖规则见模型目录子设计。
- `wopalAi.toolMode` 统一设置外部模型的工具调用方式，默认 `code_mode_only`；允许 `code_mode` 和 `direct`，其他值在读写配置或刷新目录前报错。
- `logs.maxSizeMb` 与 `logs.maxBackups` 控制日志轮转。
- `quota.maxStepCap` 是已观测最大单步的上限，默认 `5`，允许范围 `1–10`；缺省、`<= 0` 或超出范围时按默认 `5`。
- `maxStepCap` 只约束学习值，不改变基础阈值（5 小时 `97`、每周 `99`）与最小余量（`2`、`1`）的语义。
- 写入使用临时文件加 rename，权限 `0600`。

### Logs config

`runtime/config.json` 的 `logs` 段：

```json
{
  "logs": {
    "maxSizeMb": 10,
    "maxBackups": 5
  }
}
```

- `maxSizeMb` 是单个日志文件的轮转阈值，默认 10。
- `maxBackups` 是保留的旧文件份数，默认 5。
- 三个主日志共用这组参数。

### Codex managed keys

安装只修改以下配置：

- `model_catalog_json`
- `openai_base_url`
- `[memories] extract_model`
- `[memories] consolidation_model`

`model` 不属于安装接管范围。恢复时，仅当当前 `model` 以 `wopal-ai/` 开头，才恢复安装前的值；当前是官方模型时保持用户选择。

### Codex model catalog

`catalog refresh` 从 `codex debug models --bundled` 读取官方 baseline，合并 `wopalAi.models`，写入：

```text
~/.codex/wopal-codex-gateway-models.json
```

官方模型的 `visibility` 原样保留。Wopal 模型使用 `wopal-ai/<model>` slug。

`runtime/config.json` 是用户配置；`wopal-codex-gateway-models.json` 是 Codex
读取的生成目录。用户调整能力后刷新目录，生成文件可重建。

`config wopal --tool-mode <mode>` 选择整个 Wopal provider 的工具模式；
`catalog refresh` 把选择写进目录。默认 `code_mode_only` 提供 `exec`/`wait`，
`code_mode` 同时提供编排工具与直接工具，`direct` 只提供直接工具。
官方模型保留自身模式。改为 `direct` 时，已有 `exec` 历史的会话需要新建对话。
选择模式本身不构成任意模型切换成功的保证。

生成器省略外部条目的空值、Codex 已有的等价默认值和未识别字段。
官方 `base_instructions` 与 `model_messages.instructions_template` 完全相同时只保留
后一份；不同值均保留。每个模型至少保留一份有效的基础指令。
外部模型使用网关的简短编码助手指令，官方模型的有效指令保持原样。
能力来源和同步优化目标见子设计。

### Client compatibility logging

Codex 每周发布多次，新版本可能在不破坏构建的情况下改变请求契约：压缩算法、
工具面形状、beta 特性集合、请求字段。为了让升级后的问题可归因，`gateway.log`
的每条路由与 turn 记录都带上客户端指纹：

| 字段 | 来源 | 用途 |
| --- | --- | --- |
| `client_version` | `version` 头，缺失时从 `User-Agent` 解析 | 把请求绑定到具体 Codex 构建 |
| `client_originator` | `originator` 头 | 区分 Desktop / `codex_exec` / SDK |
| `client_user_agent` | `User-Agent`（折叠控制字符、截断） | 兜底身份 |
| `client_encoding` | `Content-Encoding` | 新压缩算法会让网关无法解码 |
| `client_features` | `x-codex-beta-features` | 契约变化的最早信号 |

请求体只做形状摘要，不留内容：

| 字段 | 含义 |
| --- | --- |
| `request_tool_surface` | `code_mode`（`additional_tools` namespace）或 `direct`（顶层 `tools`） |
| `request_tool_count` | 工具总数 |
| `request_tool_digest` | 排序后工具名的 8 字节摘要；集合变化即变化 |
| `request_functions` | `functions` namespace 内的工具名，例如 `[exec wait]` |
| `request_input_types` | 历史条目类型计数，例如 `message=12` |
| `request_keys` | 顶层请求字段 |

同一 `client_version` + model 的形状发生变化时，额外写一条
`client_request_contract_changed`，记录 `tool_surface_from/to`、
`tool_count_from/to`、`functions_from/to`、`keys_added/removed`、
`encoding_from/to`、`features_from/to`。变化只在发生的那次记录一次，不是每个
turn 都写。观察表有界（64 项），淘汰只会导致重复记录，不会漏掉请求。

这些字段全部是 header 值或计数，仍然满足 metadata-only 边界：不记录 prompt、
正文、工具参数或工具输出。

### Status

`/_status` 至少包含：

- `official_quota.state`
- `official_quota.five_hour`、`official_quota.weekly`
- `official_quota.effective_five_hour_stop_percent`
- `official_quota.effective_weekly_stop_percent`
- `official_quota.observed_five_hour_max_step`
- `official_quota.observed_weekly_max_step`
- `official_quota.failover`
- `notifications`

`effective_*` 是按观测最大单步计算后的实际停止点。基础阈值与实际阈值同时存在，操作者能解释提前停的原因。

## Data and State Model

| State | Location | Owner | Rules |
| --- | --- | --- | --- |
| Gateway config | `runtime/config.json` | CLI + daemon | version 1，`0600`，原子写入 |
| Codex restore | `runtime/codex-restore.json` | CLI | 安装前记录，恢复后删除，`0600` |
| Quota state | `runtime/official-quota-state.json` | daemon | account key 使用 SHA-256；跨重启保留；最大单步跨窗口保留 |
| Model catalog | `~/.codex/wopal-codex-gateway-models.json` | CLI | 生成文件，`0600` |
| Logs | `logs/gateway.log`、`logs/service.log`、`logs/service-error.log` | daemon | JSONL metadata；不记录 prompt/正文 |
| Binary | `runtime/bin/wopal-codex-gateway` | build | 单一可执行文件 |

日志与临时文件规则：

- `serve` 通过 `sdkapi.WithRequestLoggerFactory` 返回 nil request logger。SDK 只在工厂返回非 nil 日志器时才安装 request-logging middleware；正文捕获能力只存在于该中间件中，因此成功与出错路径都不会把正文写入磁盘。
- 不实现 no-op logger：`sdk/logging.RequestLogger` 的方法签名引用 SDK 内部类型，外部模块无法实现；返回 nil 是 SDK 支持的退出方式。
- `/_status` 的 `request_body_logging` 固定报告 `"disabled"`，与实际安装的机制一致。
- 请求正文只在内存中处理，不写入 `logs/`、`runtime/` 或其他目录。
- 错误诊断只记录状态码、模型、错误码、request id 和耗时。
- 升级前提：升级 CLIProxyAPI 后必须重新确认三件事，缺一不可：
  1. factory 返回 nil 时仍不安装 request-logging middleware；
  2. 配置 reload 不会在 logger 为 nil 时重新创建 logger；
  3. 新版本没有在 request-logging middleware 之外新增 request/response body 的文件写入路径。
- 回归保障：`TestServedGatewayNeverWritesPromptBodies` 通过真实嵌入式服务跑「成功请求 + 上游 5xx + 畸形请求」，随后扫描整个 runtime 树，断言没有任何 `request-log*`／`request-body*`／`response-body*`／`error-*` 文件且正文标记零命中。去掉 nil factory 后该测试必须失败，这是它有效的判据。

## Reference Documents

| Document | Purpose |
| --- | --- |
| `../README.zh-CN.md` | 中文使用说明 |
| `../README.md` | English user guide |
