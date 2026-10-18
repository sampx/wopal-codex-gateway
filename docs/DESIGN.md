# Wopal Codex Gateway 设计规范

**状态：** Active
**范围：** `projects/wopal-codex-gateway/`
**角色：** 本文档是 Gateway 架构与行为规范的真相源。

## 1. 项目目标

Wopal Codex Gateway 是 Codex Desktop / Codex CLI 与多个模型提供方之间的本地兼容与路由层。

它不只是一个 HTTP 反向代理。它需要在不破坏 Codex 原有交互模型的前提下，让同一个客户端、同一个会话能够使用：

- ChatGPT / Codex 官方模型；
- 通过 OpenAI 兼容接口提供的 Wopal AI 模型。

对于 Codex 本身不能可靠处理的 provider 边界问题，Gateway 负责补齐，包括：跨 provider 模型切换、可移植上下文压缩、官方额度保护与自动故障转移、模型目录合并、以及可逆的 Codex 配置接管。

## 2. 设计目标

项目必须按以下优先级进行设计：

1. **会话连续性。** 切换 provider 时不得静默丢失历史上下文。
2. **官方兼容性。** 未命名空间的官方模型应尽可能保持 Codex 原生行为。
3. **路由明确。** 第三方模型必须使用显式命名空间；未知命名空间绝不能回落到 OpenAI 官方线路。
4. **凭证隔离。** ChatGPT / OpenAI 凭证绝不能转发给第三方 provider。
5. **状态可移植。** 新产生的 compaction 状态必须能在官方模型和 Wopal 模型之间恢复使用。
6. **额度连续性。** 官方额度进入保护区后，应自动切到备用模型继续任务，而不是中断 Agent。
7. **安装可恢复。** Gateway 接管的 Codex 配置必须能够恢复到安装前状态。
8. **本地优先安全。** 运行时状态、凭证和日志保留在本机，不进入 Git。
9. **运维简单。** 优先使用小型本地 daemon 和确定性规则，避免复杂、隐式、自适应路由。

## 3. 非目标

Gateway 不负责：

- 对公网暴露多租户 API；
- 发明新的通用 provider 协议；
- 把任意未知命名空间静默映射到官方模型；
- 为了可观测性记录 prompt、完整请求正文、凭证或原始账号标识；
- 在没有显式 materialization（物化）步骤的情况下，把 provider 专属 opaque state 伪装成天然可移植状态。

## 4. 总体架构

### 4.1 CLIProxyAPI 的集成方式

本项目使用开源项目 **CLIProxyAPI**，但不是把请求转发给一个独立运行的 CLIProxyAPI 服务，也不是把其源码复制或 fork 到本仓库中维护。

当前实现把 CLIProxyAPI 作为 Go Module / SDK 依赖直接引入：

```text
github.com/router-for-me/CLIProxyAPI/v8 v8.0.29
```

Go daemon 直接使用它提供的 SDK 包：

```text
sdk/api
sdk/api/handlers
sdk/cliproxy
sdk/config
```

启动时由 `wopal-codex-gatewayd` 自己调用 `cliproxy.NewBuilder()` 创建服务，再通过 `Build()` / `Run(ctx)` 启动。因此：

- CLIProxyAPI 与 Wopal Gateway 编译进**同一个 Go 二进制**；
- 运行时只有**一个 `wopal-codex-gatewayd` 进程**；
- 两者共享**同一个监听地址和端口**，默认是 `127.0.0.1:11435`；
- 不需要另外启动 `cliproxy` 可执行文件；
- 不存在“Gateway -> 独立 CLIProxyAPI 进程”的第二跳网络服务。

更准确的关系是：

> **Wopal Gateway 是控制层和兼容层，CLIProxyAPI SDK 是被内嵌的数据面 / provider 适配能力。**

### 4.2 真实进程结构

```text
Codex Desktop / Codex CLI
        |
        | HTTP + WebSocket
        | OpenAI Responses 语义
        v
+---------------------------------------------------------+
| wopal-codex-gatewayd                                    |
| 单进程，监听 127.0.0.1:11435                           |
|                                                         |
|  Wopal Gateway 自定义层                                 |
|  - 路由 / namespace                                    |
|  - 官方 HTTP / WebSocket 直通                          |
|  - quota guard / failover / failback                   |
|  - compaction / wgcp1                                   |
|  - provider 边界清洗                                   |
|  - Wopal WebSocket 状态桥                              |
|             |                                           |
|             | c.Next() / 本机回环 HTTP                 |
|             v                                           |
|  内嵌 CLIProxyAPI SDK                                   |
|  - Responses HTTP handler                               |
|  - OpenAI-compatible provider 适配                      |
|  - Wopal AI / New API 上游转发                         |
|  - /v1/models 等基础 API                               |
+-------------+---------------------------+---------------+
              |                           |
              | 官方路径                  | Wopal 路径
              v                           v
      ChatGPT Codex / OpenAI         Wopal AI / New API
```

项目仍分为两个本仓库实现层：

- **Go daemon（`cmd/gatewayd`）**：运行时路由、HTTP / WebSocket 协议处理、compaction、quota 状态、provider 边界清洗、会话状态，以及对 CLIProxyAPI SDK 的嵌入和扩展。
- **TypeScript CLI（`src/`）**：安装、模型目录生成、Codex 配置修改与恢复、LaunchAgent 管理、Wopal 模型配置。

运行时请求行为以 Go daemon 为准；安装与本地配置管理以 TypeScript CLI 为准。

### 4.3 CLIProxyAPI SDK 如何挂进 Gateway

Go daemon 使用 CLIProxyAPI 的 Builder 创建服务器，并向同一个 Gin Engine 注入 Gateway 自己的 middleware 与运维端点：

```text
cliproxy.NewBuilder()
  -> WithConfig(...)
  -> WithServerOptions(
       WithMiddleware(Wopal routing middleware),
       WithRouterConfigurator(...)
     )
  -> Build()
  -> Run(ctx)
```

因此请求首先进入同一个服务进程，再由 Gateway middleware 决定：

- 由 Gateway 自己完全接管；或
- 清洗 / 改写后继续 `c.Next()`，交给内嵌 CLIProxyAPI handler。

这不是在 CLIProxyAPI 前面再架一层独立 HTTP Proxy，而是在 CLIProxyAPI 提供的服务器内部插入我们的控制逻辑。

### 4.4 三条真实请求路径

#### A. 官方模型

```text
Codex
 -> 127.0.0.1:11435
 -> Gateway routing middleware
 -> Gateway 官方 reverse proxy / WebSocket bridge
 -> ChatGPT Codex backend 或 OpenAI API
```

官方推理请求由 Gateway 自己接管，保持 native pass-through 语义。**官方模型不会经过 CLIProxyAPI 的第三方 provider 翻译链路。**

#### B. Wopal AI 的 HTTP 请求

```text
Codex POST /v1/responses
 -> Gateway routing middleware
 -> 识别 wopal-ai/<model>
 -> 清除官方凭证 / 处理跨 provider 历史
 -> 写入本机 local access key
 -> c.Next()
 -> 内嵌 CLIProxyAPI Responses handler
 -> OpenAI-compatible provider 适配
 -> Wopal AI / New API
```

这里没有额外进程跳转。当前 HTTP 请求仍在同一个 `wopal-codex-gatewayd` 中继续执行，只是从我们的 middleware 落到 CLIProxyAPI SDK 自带的 handler。

#### C. Wopal AI 的 WebSocket 请求

Codex 的 WebSocket 行为与第三方上游常见的 HTTP/SSE 行为并不相同，所以这条路径由 Gateway 自己维护 WebSocket 会话状态：

```text
Codex WebSocket
 -> Gateway Wopal WebSocket bridge
 -> 整理 / 重放当前会话状态
 -> 本机回环 POST http://127.0.0.1:11435/v1/responses
 -> 同一个进程中的 Gateway middleware
 -> 内嵌 CLIProxyAPI Responses handler
 -> Wopal AI / New API HTTP/SSE
 -> SSE 返回 Gateway WebSocket bridge
 -> 转成 Codex WebSocket 事件
```

这里确实存在一次 **localhost 回环 HTTP**，但它仍然是**同一个进程、同一个监听端口**；目的不是调用另一个 Proxy 服务，而是复用 CLIProxyAPI 已实现并测试过的 HTTP Responses/provider 适配链路，避免 Gateway 再维护第二套第三方 HTTP adapter。

Compaction 调用 Wopal AI 时也复用同一条回环 HTTP 路径。

### 4.5 CLIProxyAPI 配置与密钥边界

Gateway 启动时根据自己的配置动态生成 CLIProxyAPI runtime config：

```text
runtime/cliproxy-runtime.json
```

其中包含：

- 本地监听地址；
- 本地 access key；
- Wopal AI `base-url`；
- 模型 alias 与 context window；
- OpenAI-compatible provider 配置。

真实 Wopal API Key **不会写入这个文件**。磁盘上的配置只保存占位符 `__KEYCHAIN__`；Gateway 从 macOS Keychain 读取真实密钥后，在 `sdkconfig.Config` 已经解析到内存以后再注入：

```text
macOS Keychain
 -> wopal-codex-gatewayd 内存
 -> CLIProxyAPI SDK provider config
```

因此 CLIProxyAPI SDK 能正常完成 Wopal AI 上游认证，但真实密钥不会因为生成 runtime config 而落盘。

### 4.6 为什么采用 SDK 内嵌，而不是独立 Proxy 进程

选择 SDK 内嵌的主要原因是：

1. **单进程部署。** LaunchAgent 只需要管理一个 daemon，减少端口、进程和生命周期故障点。
2. **复用成熟 provider 适配。** Wopal Gateway 不需要重新实现 CLIProxyAPI 已有的 OpenAI-compatible Responses/provider 转换能力。
3. **Gateway 可以在 handler 前控制路由。** quota、compaction、跨 provider 清洗等逻辑可以在请求进入第三方 adapter 前完成。
4. **官方线路保持原生。** 官方请求可以被 middleware 截获并直接送往 ChatGPT/OpenAI，不必经过第三方协议转换。
5. **安全边界更清楚。** 官方凭证在进入 CLIProxyAPI 的 Wopal provider 路径之前已经被 Gateway 清除。

代价是 CLIProxyAPI 版本成为编译期依赖。升级 CLIProxyAPI 时需要重新构建 Gateway，并重新验证 Responses、模型目录和第三方 provider 行为，而不能像外部独立服务那样单独升级。

## 5. 本地端点

默认监听地址：

```text
127.0.0.1:11435
```

主要端点：

| 端点 | 用途 |
| --- | --- |
| `POST /v1/responses` | Responses 推理与 HTTP failover |
| `GET /v1/responses` + WebSocket Upgrade | Codex WebSocket 推理 |
| `POST /v1/responses/compact` | compaction 兼容端点 |
| `/v1/models` | 内嵌 provider / 模型 API |
| `GET /_health` | 进程健康检查 |
| `GET /_status` | 脱敏后的运行时 / quota 状态 |

`/_health` 与 `/_status` 仅用于本地运维，不得暴露凭证或原始账号标识。

## 6. 模型命名空间契约

路由规则必须简单、确定、可预测。

### 6.1 官方模型

**没有命名空间**的模型视为官方模型，例如：

```text
gpt-6.1-sol
```

如果请求带有 `ChatGPT-Account-ID`，走 ChatGPT Codex backend；否则走 OpenAI API 官方线路。

### 6.2 Wopal AI 模型

以 `wopal-ai/` 开头的模型路由到 Wopal AI：

```text
wopal-ai/deepseek-v4.1-flash
```

命名空间同时也是安全边界。请求进入 Wopal AI 前，必须清除官方认证信息。

### 6.3 不支持的命名空间

当前 Go daemon 只启用 official 和 `wopal-ai/*` 两类运行时路由。Ollama 相关 CLI / catalog 代码暂时保留用于未来或本地实验，但当前 daemon 不提供 Ollama 正式运行时路由。

任何其他命名空间都必须明确拒绝：

```text
foo/model -> rejected
```

这样可以避免拼写错误或未知 provider 意外消耗官方额度，或造成官方凭证泄漏。

## 7. 模型目录设计

Codex 模型选择器使用 Gateway 生成的合并 catalog。

### 7.1 官方目录来源

官方模型的 canonical baseline 必须来自当前 Codex 二进制自带的 bundled catalog：

```text
codex debug models --bundled
```

不得把可变运行时文件 `~/.codex/models_cache.json` 当作官方模型真相源。Codex 可能通过 Gateway 自己刷新这个缓存，如果 Gateway 再把它当官方源读取，会形成 catalog 自污染循环。

### 7.2 可见性

官方模型元数据必须原样保留，包括 `visibility`：

- `visibility = "list"`：允许出现在用户模型选择器中；
- `visibility = "hide"`：仅供 Codex 内部流程使用，不能作为普通用户模型展示。

因此审批、安全检查等内部模型可以继续存在于合并 catalog 中，但必须保持隐藏。

### 7.3 外部模型目录

Wopal AI 模型使用显式 namespaced slug，并默认作为用户可选模型显示。

合并 catalog 的职责是“保留官方元数据 + 增加外部条目”，不是重新定义官方模型。

## 8. Codex 安装契约

`codex install` 只能修改 Gateway 明确拥有的配置，并且在修改前记录原值。

当前接管的 root 配置：

```toml
model_catalog_json = ".../wopal-codex-gateway-models.json"
openai_base_url = "http://127.0.0.1:11435/v1"
```

Gateway 安装期间还会接管两个 Memory 模型设置：

```toml
[memories]
extract_model = "wopal-ai/deepseek-v4.1-flash"
consolidation_model = "wopal-ai/deepseek-v4.1-flash"
```

安装前原值记录在 Git 忽略的 runtime restore state 中。`codex restore` 必须恢复原值，不能猜测默认值。

安装 / 恢复过程中应尽量保留与 Gateway 无关的用户配置和注释。

## 9. HTTP 路由

对于 `POST /v1/responses`，当正文内包含 `model` 时，正文模型优先于旧的 routing hint。过期的握手 hint 不得覆盖新模型。

Gateway 必须支持 Codex 可能发送的压缩请求正文；当请求需要改写时，可以规范化为未压缩正文再继续处理。

用于路由解析的正文大小必须有限制，避免无限占用内存。

### 9.1 官方 HTTP

官方请求在清洗掉 Gateway 自己生成、OpenAI 无法验证的状态后，再透明反向代理到官方目标。

### 9.2 Wopal HTTP

Wopal 请求进入第三方 Responses-to-chat adapter 前需要做协议归一化，同时移除官方 Authorization 与 ChatGPT account header。

### 9.3 HTTP quota failover

当官方请求已经进入 quota 保护状态时，Gateway 直接把 effective model 改写为 Wopal fallback 模型，通过 Wopal 路径继续完成请求，而不是返回额度中断错误。

## 10. WebSocket 设计

Codex 使用长连接 WebSocket。一个 socket 的握手、认证与上游连接都绑定到具体 provider，因此 socket 本身就是 provider-bound。

**同一个上游 socket 绝不能跨 provider 复用。**

### 10.1 跨 provider 切换

当新的 sampling request 改变了 route class，Gateway 使用 WebSocket `1012`（Service Restart）关闭客户端连接：

```text
provider A socket
    -> 模型切换到 provider B
    -> Gateway 发送 1012
    -> Codex 重连
    -> Codex 重放完整 input
    -> Gateway 为 provider B 建立新 socket
```

不能用 API `400` 表示这种切换，因为 `400` 会终止 turn，而不是触发 transport retry。

### 10.2 官方 WebSocket

官方 WebSocket 以透明 pass-through 为主。Gateway 只负责观察 rate-limit telemetry，以及清洗 OpenAI 无法验证的 Gateway-owned state。

### 10.3 Wopal WebSocket bridge

Wopal AI 上游可能只提供 HTTP / SSE，而 Codex 需要 WebSocket 语义。因此 Gateway 在本地维护 Codex WebSocket session，并把每个 sampling step 转成 HTTP / SSE 上游调用。

本地桥接状态包括：

- 上一次归一化请求；
- 上一次 response output；
- 上一次 response id；
- 当前 active compaction id。

会话状态必须有上限；超过上限时宁可失败，也不能无限增长内存。

## 11. Sampling step 语义

Quota 与 provider 切换的判断粒度是 **sampling step**，不是整个用户 turn。

一次用户 / Agent turn 里可能发生多轮：

```text
模型调用
-> 工具调用
-> 模型调用
-> 工具调用
-> 模型调用
```

每个新的 `response.create` 或 `response.append` 都是新的路由与 quota 判断点。

Gateway 能阻止的是“观察到 quota 进入保护状态以后，下一次模型采样请求”。已经开始执行的采样步骤不能事后撤销其额度消耗。

## 12. 官方额度保护

Quota guard 的目标是保留官方订阅额度，同时保证 Agent 任务继续执行。

### 12.1 Telemetry

Quota 状态来自 ChatGPT 官方响应返回的 rate-limit metadata。窗口类型根据窗口时长识别，不能假设 `primary` / `secondary` 永远对应固定含义。

当前识别：

| 窗口 | 时长 | 保护阈值 |
| --- | ---: | ---: |
| 5 小时 | 300 分钟 | 已使用 98% |
| 每周 | 10080 分钟 | 已使用 99% |

这些比例是当前产品策略，不是协议级不变量。

### 12.2 持久化

Quota 状态保存到：

```text
runtime/official-quota-state.json
```

这样 daemon 重启后不会忘记已经处于保护状态的窗口。

原始 ChatGPT account ID 不得落盘；持久化前使用 SHA-256 哈希作为 account key。

到达 reset time 后，相应窗口状态应自动失效。

### 12.3 官方豁免流程

部分官方模型用于恢复或内部审批，不参与普通 quota failover。当前实现显式豁免支持的 Luna 恢复模型和 `codex-auto-review`。

豁免列表属于明确策略，不能靠模型名称猜测。

### 12.4 Failover 状态机

```text
OFFICIAL
   |
   | 下一 sampling step 前发现 protected
   v
WOPAL FAILOVER
   |
   | 下一 sampling step 前发现 quota 已 reset
   v
OFFICIAL
```

HTTP 路径直接改写 model。

WebSocket 路径先使用 `1012` 关闭官方 socket；Codex 重连后，同一个请求由 Wopal bridge 接管。处于 fallback 状态时，每一个 sampling step 都重新检查 quota。窗口 reset 后，Wopal socket 再次用 `1012` 关闭，让 Codex 重连回原本请求的官方模型。

日志必须区分 requested model 与 effective model。

## 13. Memory 与 Compaction 是两套机制

Codex **Memory** 和会话 **compaction** 解决的问题不同，实现方式也不同。

### 13.1 Codex Memory

Memory extraction / consolidation 是 Codex 自己的后台流程。Gateway 不通过识别隐藏模型名去“猜”某个请求是不是 Memory。

安装时直接配置 Codex：

```text
wopal-ai/deepseek-v4.1-flash
```

作为 extraction 与 consolidation 的模型。

这种方式可以避免隐藏的官方 Memory worker 消耗官方额度，同时避免 Proxy 中增加脆弱的启发式规则。

### 13.2 会话 Compaction

会话 compaction 由 Gateway 接管。

无论前台当前使用哪个模型，新 compaction 都会被 Gateway 拦截，并使用专用模型执行。当前模型：

```text
wopal-ai/deepseek-v4.1-flash
```

Compaction 模型是 Gateway 当前策略常量，不继承前台模型。

## 14. 可移植 Compaction（`wgcp1`）

OpenAI 原生 compaction capsule 对第三方 provider 是 opaque 的。为了支持跨 provider，会话中新产生的 compaction 统一由 Gateway 生成自己的可移植加密 capsule。

### 14.1 压缩流程

```text
Codex 发起 compaction
    -> Gateway 拦截
    -> 准备可用的完整会话上下文
    -> 专用 Wopal compaction 模型生成 handoff summary
    -> Gateway 封装 summary 与需保留的 prefix metadata
    -> 返回 Gateway-owned compaction item 给 Codex
```

加密 payload 前缀：

```text
wgcp1.
```

Payload 使用 AES-GCM 认证加密，密钥由本机 Wopal secret 派生。ciphertext 中不得出现 summary 明文。

### 14.2 Retained prefix

与 provider 无关、但压缩后仍需存在的上下文，例如支持的 `additional_tools` 声明，可以一起加密保存在 capsule 中，并在 summary 前恢复。

如果客户端已经带了最新声明，恢复时不得重复插入。

### 14.3 展开的 summary message

当 `wgcp1` 被展开成官方 Responses `message` 时，合成 ID 必须满足官方约束：

- 前缀必须是 `msg_`；
- 最大长度 64 字符；
- 相同 compaction id 必须得到稳定、确定的 ID。

当前形式为 `msg_wopal_compaction_summary_...`；源 ID 太长时使用稳定哈希。

### 14.4 本地 response ID

Gateway 自己生成的 compaction response id 不是 OpenAI 服务端 continuation id。切回官方模型时，必须删除本地 `previous_response_id`，改为发送 materialized history。

## 15. 官方边界清洗

Gateway 自己生成的 opaque state 不能伪装成 OpenAI 生成的数据再转给官方。

官方请求发送前必须：

- 把可读取的 `wgcp1` 展开成普通 message；
- 删除 Gateway / Wopal 生成的 reasoning item；
- 当历史已经 materialize 时删除 Gateway-local `previous_response_id`；
- 原样保留真正的 OpenAI native reasoning / compaction state。

如果 Gateway capsule 无法解密，必须显式降级或失败，不能泄漏 ciphertext，也不能伪造丢失的历史。

## 16. 旧 OpenAI Native Compaction

旧会话可能仍然携带 Gateway 接管 compaction 之前产生的 OpenAI opaque capsule。

第三方模型无法直接理解这类状态。

当旧官方历史切到 Wopal 时，Gateway 通过 native compaction bridge，请原 provider 把 opaque state materialize 成可用 summary。恢复结果需要缓存，避免同一个 capsule 重复请求官方恢复。

Legacy restore 必须有超时，并且一旦拿到 completed response 就立即结束，不能无期限等待 stream EOF。

如果 native state 无法恢复，跨 provider 切换必须失败，不能静默丢弃历史。

这是一条迁移兼容路径。新的 compaction 不得继续制造 OpenAI native opaque state。

## 17. 跨 Provider 会话连续性规则

以下规则是强制约束：

1. 目标 provider 无法读取 compaction 时，绝不能静默丢弃它。
2. 绝不能把 provider A 的 opaque encrypted state 原样发给 provider B 碰运气。
3. 当 provider-local state 已经 materialize 成普通历史时，必须解除旧的 `previous_response_id` 链接。
4. WebSocket 跨 provider 切换必须通过 reconnect + replay。
5. 切回官方时，Wopal-owned item 必须转换成官方合法的 Responses item。
6. 旧官方历史切到 Wopal 前，必须先 materialize native state。

会话连续性优先级高于保住某一个 transport connection。

## 18. 认证与凭证边界

### 18.1 Wopal AI secret

Wopal API key 存储在 macOS Keychain，由 daemon 运行时读取。

生成给 CLIProxyAPI 的运行时配置不能写入真实 Wopal API key。

### 18.2 官方凭证

官方 Authorization 与 ChatGPT account header 只能存在于官方线路。

进入 Wopal 路径前必须：

- 把 Authorization 替换成本地 embedded proxy credential；
- 删除 `ChatGPT-Account-ID`；
- 不转发其他只属于官方 provider 的认证信息。

### 18.3 本地文件

运行时配置、日志、生成二进制、auth state、restore state 和 quota persistence 都属于 Git 忽略的本地运行数据。

包含敏感状态的文件应尽可能使用严格权限。

## 19. 日志与可观测性

主要本地日志：

```text
logs/gateway.log
logs/service.log
logs/service-error.log
```

`gateway.log` 记录路由 / 生命周期元数据，例如 provider / model、failover / failback、quota observation、WebSocket 生命周期。

日志应优先记录 metadata，不记录：

- prompt 或完整请求正文；
- API key 或 Authorization header；
- 原始 ChatGPT account ID；
- 加密 capsule 的明文内容。

`/_status` 是轻量查看健康状态与最新 quota 状态的标准接口。

### 19.1 用户通知

自动 quota failover 会替换用户请求的官方模型，属于用户可感知的行为变化，因此 Gateway 在替换发生时发送一条 macOS 通知。

- 触发点是模型被替换的那次 sampling step，不是每一次 quota 检查；
- 同一个保护周期、同一个额度窗口、同一个被替换的模型只通知一次；
- 通知是 best-effort：通知工具缺失或发送失败都不影响路由，只在日志中记录事件；
- 通知由 `terminal-notifier` 发送，不使用 `osascript`——`osascript` 发出的通知归属 Script Editor，点击后会打开脚本编辑器；
- 通知内容只包含请求模型、生效模型和额度重置时间，不含 prompt、凭证或账号标识。

通知开关与参数属于本地运行配置（`runtime/config.json` 的 `notifications` 段），默认开启。`/_status` 的 `notifications` 字段用于确认通知是否可用。

## 20. 服务生命周期

Gateway 以 macOS 当前用户的 LaunchAgent 运行：

```text
com.wopal.codex-gateway
```

正式 daemon 路径：

```text
runtime/bin/wopal-codex-gatewayd
```

安装流程必须等 `/_health` 成功后才能报告完成。

新编译 Mach-O 在 macOS 上首次运行可能因为系统安全检查而明显变慢，因此 installer 使用有限总 deadline，而不是很短的固定重试次数。当前总 health deadline 为 90 秒。

较长 deadline 不代表允许隐藏 daemon crash；超过 deadline 仍必须报告安装失败。

## 21. 运行时失败原则

Gateway 遵循以下失败策略：

- **未知命名空间：** 明确拒绝。
- **跨 provider 历史无法恢复：** 失败，但不得丢历史。
- **官方 WebSocket upgrade 前上游拨号失败：** 返回干净 HTTP 错误，不能留下半开 socket。
- **Provider 变化：** 重连，不跨不兼容凭证直接隧道复用。
- **Quota 保护：** 能 failover 就继续任务，不优先返回中断。
- **Quota 恢复：** 在安全 sampling boundary 回到原请求的官方模型。
- **状态超限：** 失败，不允许无限保留会话状态。
- **Compaction 失败：** 不能伪造成功 capsule。

## 22. 测试契约

任何涉及跨 provider、历史变换或凭证边界的行为，都必须有回归测试。

项目最低验证命令：

```sh
go test ./...
go vet ./...
pnpm test
pnpm typecheck
```

并发敏感 Go 路径还应运行有针对性的 `go test -race`。

### 22.1 测试生命周期卫生

无论测试成功还是失败，都不能遗留本地子进程或 socket。

Node 测试当前使用：

- 单测试硬超时；
- suite 完成后强制退出 worker；
- 显式关闭 WebSocket / client；
- server 正常 close 无法排空连接时使用兜底强制关闭。

“测试全部通过但留下垃圾进程”应视为验证失败。

### 22.2 必须长期覆盖的回归主题

至少包括：

- official 与 namespaced routing；
- 第三方边界凭证清洗；
- HTTP / WebSocket provider switching；
- `1012` replay 行为；
- sampling-step 级 quota protect / failover / failback；
- quota 状态跨重启持久化；
- Wopal compaction 闭环；
- Wopal -> official compaction expansion；
- official -> Wopal legacy compaction materialization；
- 官方合法 synthetic message ID；
- hidden 官方模型可见性；
- Codex 配置 install / restore 可逆；
- 不把 stale `models_cache.json` 当官方 catalog 源。

## 23. 稳定架构不变量

除非本文档被明确修订，未来实现必须保持以下约束：

1. **官方模型保持无命名空间。**
2. **第三方 provider 必须使用显式命名空间。**
3. **未知命名空间绝不能回落到官方。**
4. **官方凭证绝不能到达 Wopal AI。**
5. **Codex bundled catalog 是官方模型基线。**
6. **官方 hidden 模型必须继续 hidden。**
7. **WebSocket 跨 provider 必须 reconnect + replay。**
8. **新的会话 compaction 由 Gateway 接管并保持可移植。**
9. **Gateway-owned encrypted / reasoning state 在进入官方前必须清洗。**
10. **无法读取的 provider 专属历史绝不能被静默丢弃。**
11. **每个新 sampling step 都要检查 quota protection。**
12. **Codex 安装必须可逆。**
13. **运行时凭证与状态不得进入 Git。**
14. **测试必须回收自己创建的进程和 socket。**

## 24. 当前策略参数

以下值是当前部署策略，不是永久架构约束；以后可以修改，但必须显式调整并有测试覆盖。

| 策略 | 当前值 |
| --- | --- |
| 默认监听地址 | `127.0.0.1:11435` |
| Quota fallback 模型 | `wopal-ai/deepseek-v4.1-flash` |
| 专用 compaction 模型 | `wopal-ai/deepseek-v4.1-flash` |
| Memory extraction 模型 | `wopal-ai/deepseek-v4.1-flash` |
| Memory consolidation 模型 | `wopal-ai/deepseek-v4.1-flash` |
| 5 小时窗口保护阈值 | 已使用 `98%` |
| Weekly 保护阈值 | 已使用 `99%` |
| Native compaction restore 超时 | 约 `3 分钟` |
| Install health deadline | `90 秒` |
| Node 单测试超时 | `30 秒` |
| Failover 用户通知 | 默认开启（每个保护周期、窗口、模型各一次） |
| 通知工具 | `terminal-notifier`（缺失时不通知，仅记录日志） |

## 25. 变更纪律

以下行为发生变化时，必须在同一变更集中同步更新本文档：

- 路由命名空间语义；
- provider 凭证边界；
- 模型 catalog 来源或可见性规则；
- quota 状态机或保护阈值；
- compaction 所有权或 capsule 格式；
- Memory 路由策略；
- 跨 provider replay 行为；
- Codex install / restore 接管范围；
- 运行时安全不变量。

只要不改变本文定义的行为与不变量，内部实现细节可以自由演进。
