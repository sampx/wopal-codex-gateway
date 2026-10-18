# Wopal Codex Gateway

[English](README.md) | 简体中文

Wopal Codex Gateway 是运行在 macOS 本机上的 Codex 路由与兼容网关。它让 ChatGPT / Codex Desktop 或 Codex CLI 在保留官方模型能力的同时，也能在同一个模型选择器、同一个会话中使用 `wopal-ai/*` 第三方模型。

它不只是一个 HTTP 代理，还负责：模型目录合并、Codex 配置接管与恢复、WebSocket/HTTP 跨 Provider 路由、上下文压缩、老会话兼容、官方额度保护与自动 failover/failback。

详细架构与行为规范见 [`docs/DESIGN.md`](docs/DESIGN.md)。

## 目录

- [1. 能力概览](#1-能力概览)
- [2. 系统要求](#2-系统要求)
- [3. 从源码构建](#3-从源码构建)
- [4. 首次配置](#4-首次配置)
- [5. 启用 Gateway](#5-启用-gateway)
- [6. 验证安装](#6-验证安装)
- [7. 日常使用](#7-日常使用)
- [8. 模型管理](#8-模型管理)
- [9. 服务管理](#9-服务管理)
- [10. 更新 Gateway](#10-更新-gateway)
- [11. 停用与卸载](#11-停用与卸载)
- [12. 故障排查](#12-故障排查)
- [13. 安全与本地状态](#13-安全与本地状态)
- [14. 开发与验证](#14-开发与验证)

## 1. 能力概览

### 官方模型保持原生

没有命名空间的模型仍走官方线路，例如：

```text
gpt-6.1-sol
gpt-6-astra
gpt-5.6-terra
```

Gateway 会根据 Codex 请求中的认证上下文，将其转发到 ChatGPT Codex backend 或 OpenAI API 官方线路。

### Wopal AI 使用显式命名空间

第三方模型统一使用：

```text
wopal-ai/<model>
```

例如：

```text
wopal-ai/deepseek-v4.1-flash
```

官方凭证在进入 Wopal AI 路径前会被移除，不会转发给第三方 Provider。

### 同一会话可跨 Provider 切换

Gateway 支持在同一个 Codex 会话中：

```text
官方模型 -> Wopal AI -> 官方模型
```

WebSocket 跨 Provider 时会使用 `1012 Service Restart` 让 Codex 重连并重放完整输入，而不是把一个已经绑定官方凭证的 socket 强行复用到第三方 Provider。

### 新的会话压缩统一由 Gateway 接管

新的 compaction 不再生成只能由 OpenAI 自己理解的 opaque capsule，而是统一由：

```text
wopal-ai/deepseek-v4.1-flash
```

生成摘要，再封装为 Gateway 自己的 `wgcp1` 可移植 capsule。这样压缩后的会话可以继续在官方模型与 Wopal 模型之间切换。

### Codex Memory 默认走 Wopal AI

Gateway 安装期间会配置：

```toml
[memories]
extract_model = "wopal-ai/deepseek-v4.1-flash"
consolidation_model = "wopal-ai/deepseek-v4.1-flash"
```

因此 Memory extraction / consolidation 不再消耗主要官方模型额度。

### 官方额度自动保护

当前策略：

| 额度窗口 | 保护阈值 |
| --- | ---: |
| 5 小时 | 使用达到 98% |
| 每周 | 使用达到 99% |

保护粒度是每一次新的 sampling step，即每个 `response.create` / `response.append` 之前重新判断。

达到阈值后：

```text
请求的官方模型
    -> Gateway quota guard
    -> wopal-ai/deepseek-v4.1-flash
```

额度 reset 后，下一次安全的 sampling step 会自动切回原来请求的官方模型。

当前 `gpt-5.6-luna`、`gpt-6-luna` 和内部 `codex-auto-review` 保持官方路径，不参与普通 failover。

## 2. 系统要求

当前正式运行环境是 macOS，因为项目依赖：

- macOS Keychain 保存 Wopal AI API Key；
- LaunchAgent 管理后台服务；
- 本机 Codex / ChatGPT Desktop 配置目录。

从源码运行需要：

- macOS；
- 已安装 ChatGPT/Codex Desktop 或 Codex CLI；
- `codex` 命令可执行；
- Go `1.26.x`（以 `go.mod` 为准）；
- Node.js `22.15+`；
- pnpm `11.x`；
- 一个可用的 Wopal AI / New API OpenAI-compatible endpoint；
- 对应的 API Key；
- 当前上游必须提供 `deepseek-v4.1-flash`，因为当前版本的 compaction、Memory 和 quota failover 都使用该模型；
- （可选）`terminal-notifier`：自动 quota failover 时弹 macOS 通知，安装方式 `brew install terminal-notifier`；未安装时只是不弹通知，路由功能不受影响。

检查基本环境：

```sh
codex --version
go version
node --version
pnpm --version
```

Gateway 需要能够执行：

```sh
codex debug models --bundled
```

它使用 Codex 当前版本自带的 bundled catalog 作为官方模型目录真相源。

## 3. 从源码构建

以下命令都在项目根目录执行。

### 3.1 安装 Node 依赖

```sh
pnpm install
```

### 3.2 构建 TypeScript CLI

```sh
pnpm build
```

构建结果位于：

```text
dist/
```

### 3.3 构建 Go daemon

```sh
mkdir -p runtime/bin
go build -o runtime/bin/wopal-codex-gatewayd ./cmd/gatewayd
```

最终后台服务二进制：

```text
runtime/bin/wopal-codex-gatewayd
```

### 3.4 推荐先做一次验证

```sh
go test ./...
go vet ./...
pnpm test
pnpm typecheck
```

全部通过后再进行首次配置。

## 4. 首次配置

推荐使用项目自带的高层管理入口：

```sh
./scripts/gateway
```

而不是直接操作 `dist/cli.js`、LaunchAgent 或 Codex 配置文件。

### 4.1 配置 Wopal AI / New API 地址

```sh
./scripts/gateway config wopal --base-url https://your-new-api.example.com/v1
```

`--base-url` 应填写你的 OpenAI-compatible API 根地址。

只查看当前 Gateway 配置：

```sh
node dist/cli.js config show
```

注意：`scripts/gateway config ...` 是高层变更入口，在执行配置命令后还会自动刷新模型 catalog；如果服务已经安装，也会重新启动服务。因此仅查看配置时优先使用上面的底层只读命令。

### 4.2 保存 Wopal AI API Key

```sh
./scripts/gateway auth set
```

API Key 使用 macOS `security` 命令写入 Keychain：

```text
service: com.wopal.codex-gateway.wopal-ai
account: wopal-ai
```

真实 API Key 不写入项目配置文件。

`auth set` 写入后会立即调用 Wopal AI `/models` 进行验证。

检查是否已经配置：

```sh
./scripts/gateway auth status
```

重新测试 Key：

```sh
./scripts/gateway auth test
```

### 4.3 查看 Wopal AI 当前可用模型

```sh
./scripts/gateway models list
```

这一步读取远端 `/models`，显示当前 API Key 能看到的模型 ID。

当前版本请确认输出中存在：

```text
deepseek-v4.1-flash
```

如果不存在，不要继续正式安装：当前版本的 compaction、Memory 和 quota failover 都依赖这个模型。

### 4.4 同步模型到 Codex catalog

推荐首次配置直接同步：

```sh
./scripts/gateway models sync
```

`models sync` 会：

1. 从 Wopal AI `/models` 获取模型；
2. 自动跳过明显的 embedding / rerank 模型；
3. 保留已经手工设置过的模型 metadata；
4. 更新 Gateway 本地配置；
5. 重新生成 Codex 合并模型目录。

同步后，默认 metadata 为：

```text
context window: 262144
vision: false
reasoning: true
```

这些只是 Gateway/Codex 本地 metadata，不会修改上游模型本身。因此重要模型建议继续校准真实上下文长度和能力。

例如：

```sh
./scripts/gateway models set deepseek-v4.1-flash \
  --context-window 1000000 \
  --reasoning \
  --no-vision
```

模型名可以写：

```text
deepseek-v4.1-flash
```

不需要手工加 `wopal-ai/` 前缀。

## 5. 启用 Gateway

首次完整启用只需要：

```sh
./scripts/gateway install
```

这是推荐的正式安装入口。

它会完成以下闭环操作：

1. 从 `codex debug models --bundled` 读取官方模型目录；
2. 合并官方模型与 `wopal-ai/*` 模型；
3. 生成 Gateway 专用 Codex model catalog；
4. 安装并启动 `com.wopal.codex-gateway` LaunchAgent；
5. 等待 `/_health` 健康检查通过；
6. 记录安装前的 Codex 配置恢复点；
7. 将 Codex `openai_base_url` 指向本地 Gateway；
8. 将 Codex `model_catalog_json` 指向合并后的 catalog；
9. 配置 Memory extraction / consolidation 使用 Wopal AI；
10. 保留用户其他 Codex 配置不变。

成功后会提示重启 ChatGPT/Codex Desktop。

### 5.1 必须重启 ChatGPT/Codex Desktop

安装成功后，请完全退出并重新启动 ChatGPT/Codex Desktop。

这是为了让客户端重新读取：

- `openai_base_url`；
- `model_catalog_json`；
- Memory 模型配置；
- 新的模型列表。

重启后，模型选择器应该同时存在：

```text
官方用户可见模型
+
wopal-ai/*
```

Codex 内部 `visibility=hide` 的模型仍会留在 catalog 中供内部流程使用，但不应该显示在正常用户模型选择器里。

## 6. 验证安装

### 6.1 检查服务

```sh
./scripts/gateway status
```

正常结果类似：

```text
installed=yes loaded=yes healthy=yes
```

### 6.2 检查健康端点

```sh
curl -fsS http://127.0.0.1:11435/_health
```

正常示例：

```json
{"mode":"single-process","ok":true,"wopal_models":33}
```

### 6.3 查看运行状态与官方额度

```sh
curl -fsS http://127.0.0.1:11435/_status
```

会显示：

- Gateway 是否正常；
- 当前监听地址；
- official route 状态；
- 5 小时 / weekly quota 百分比与 reset 时间；
- quota guard 是否处于 `open` 或 `protected`；
- Wopal adapter 状态。

`/_status` 不暴露 API Key 或原始 ChatGPT account ID。

### 6.4 再次验证 Wopal API

```sh
./scripts/gateway auth test
```

### 6.5 检查模型 catalog

如果想从 Codex 侧检查当前实际模型目录，可使用：

```sh
codex debug models
```

官方模型的基线来自：

```sh
codex debug models --bundled
```

Gateway 不使用容易被运行时覆盖的 `~/.codex/models_cache.json` 作为官方 canonical source。

## 7. 日常使用

### 7.1 使用官方模型

在 Codex 模型选择器中直接选择没有命名空间的官方模型：

```text
gpt-6.1-sol
```

请求会保持官方路径。

### 7.2 使用 Wopal AI 模型

选择：

```text
wopal-ai/deepseek-v4.1-flash
```

或其他同步进来的 `wopal-ai/*` 模型。

### 7.3 在同一个老会话中切换模型

无需为了使用 Gateway 清理老会话。

正常情况下，同一个会话可以：

```text
Wopal AI -> 官方模型
官方模型 -> Wopal AI
```

新的 `wgcp1` compaction 可以跨 Provider 展开。

对于非常早期、仍只包含 OpenAI native opaque compaction 的历史会话，第一次切到第三方 Provider 时可能需要通过官方线路先物化一次历史摘要；Gateway 不会为了切换 Provider 静默丢弃这部分历史。

### 7.4 自动 compaction

无论当前前台模型是官方还是 Wopal AI，新的会话 compaction 都由 Gateway 接管，当前固定使用：

```text
wopal-ai/deepseek-v4.1-flash
```

这不是 Codex `[memories]` 配置，而是 Gateway runtime 策略。

### 7.5 自动 quota failover

当官方额度达到保护阈值时，Gateway 在下一次新的 sampling step 之前自动判断。

例如：

```text
gpt-6.1-sol
    -> quota protected
    -> wopal-ai/deepseek-v4.1-flash
```

任务不需要因为官方额度保护而主动终止。

reset 到期后：

```text
Wopal fallback
    -> next sampling step
    -> quota recovered
    -> reconnect
    -> original official model
```

WebSocket 路径出现 `1012 Service Restart` 在 Provider 切换和 quota failover/failback 场景下是预期控制行为，Codex 应重新建立连接并重放输入。

两个方向都会弹 macOS 通知，每个保护周期各一次，不会在大批请求里反复弹窗：

- 切换发生时：告诉你请求的官方模型被换成了哪个模型、额度大约什么时候恢复；
- 额度 reset 时：告诉你可以切回官方模型了（由定时器驱动，不需要你先发一次请求）。

想随时确认「现在这句跑在哪个模型上」，看这里：

```sh
curl -fsS http://127.0.0.1:11435/_status
```

`official_quota.state` 为 `protected` 表示官方额度处于保护区；`official_quota.failover.active` 为 `true` 表示你的官方请求正在被自动换成 Wopal 模型。

通知依赖 `terminal-notifier`；未安装时不会弹通知，路由功能不受影响。需要关闭通知时，在 `runtime/config.json` 里加上：

```json
"notifications": { "enabled": false }
```

## 8. 模型管理

### 8.1 查看远端所有模型

```sh
./scripts/gateway models list
```

### 8.2 自动同步

```sh
./scripts/gateway models sync
```

同步后需要重启 ChatGPT/Codex Desktop 才能确保模型选择器重新加载 catalog。

### 8.3 手工添加模型

```sh
./scripts/gateway models add my-model --context-window 262144
```

支持：

```text
--context-window N
--vision
--no-reasoning
```

例如：

```sh
./scripts/gateway models add vision-model \
  --context-window 131072 \
  --vision
```

### 8.4 修改模型 metadata

```sh
./scripts/gateway models set deepseek-v4.1-flash \
  --context-window 1000000 \
  --reasoning \
  --no-vision
```

可修改：

```text
--context-window N
--vision / --no-vision
--reasoning / --no-reasoning
```

修改已经被现有 Codex session 加载过的模型 metadata 后，建议重启客户端或重新打开/分叉相应会话。

### 8.5 移除模型

```sh
./scripts/gateway models remove model-a model-b
```

被移除的模型会加入本地 exclusion 列表，后续 `models sync` 不会自动把它们重新加回来。

要恢复被排除的模型，显式执行：

```sh
./scripts/gateway models add model-a
```

### 8.6 手工刷新 catalog

```sh
./scripts/gateway catalog refresh
```

适合以下场景：

- Codex 升级后官方 bundled catalog 发生变化；
- 手工调整了模型配置；
- 模型选择器与当前配置不同步。

刷新后重启 ChatGPT/Codex Desktop。

## 9. 服务管理

### 查看状态

```sh
./scripts/gateway status
```

### 临时停止

```sh
./scripts/gateway stop
```

这只停止 Gateway 服务，**不会恢复 Codex 配置**。

因此 `stop` 适合短时间维护，不适合正式卸载。服务停止期间 Codex 仍可能指向 `127.0.0.1:11435`，此时模型请求会失败。

### 启动

```sh
./scripts/gateway start
```

前提是 LaunchAgent 已安装。

### 重启

```sh
./scripts/gateway restart
```

### 只安装服务

```sh
./scripts/gateway service install
```

该命令只负责 LaunchAgent，不负责完整 Codex 接入。

### 只卸载服务

```sh
./scripts/gateway service uninstall
```

**不要把它当作正常卸载命令。**

它只移除 LaunchAgent，不会恢复 `~/.codex/config.toml`。如果 Codex 仍然指向 Gateway，而服务已经卸载，Codex 请求会失败。

正常停用请使用：

```sh
./scripts/gateway disable
```

## 10. 更新 Gateway

源码更新后，推荐执行完整的重新构建 + 重新安装流程。

### 10.1 临时停止服务

```sh
./scripts/gateway stop
```

### 10.2 更新依赖并重新构建

```sh
pnpm install
pnpm build
go build -o runtime/bin/wopal-codex-gatewayd ./cmd/gatewayd
```

### 10.3 重新应用 Gateway 安装状态

```sh
./scripts/gateway install
```

重复执行 `install` 是支持的。Gateway 会保留最初的 Codex restore point，而不是把 Gateway 自己已经修改后的配置错误地当作“原始配置”。

### 10.4 重启客户端

完全退出并重启 ChatGPT/Codex Desktop。

如果只是模型列表变化，也可以只执行：

```sh
./scripts/gateway catalog refresh
```

然后重启客户端。

## 11. 停用与卸载

### 11.1 推荐：完整停用 Gateway

使用：

```sh
./scripts/gateway disable
```

这是真正的用户级“卸载/恢复”入口。

它会：

1. 恢复 Gateway 安装前保存的 Codex 配置；
2. 恢复原来的 `openai_base_url`；
3. 恢复原来的 `model_catalog_json`；
4. 恢复原来的 Memory model override；
5. 恢复安装前记录的相关 model 配置；
6. 卸载并移除 LaunchAgent；
7. 删除 Gateway 生成的 Codex 合并 catalog；
8. 删除本次安装使用的 restore state。

完成后必须重启 ChatGPT/Codex Desktop，让客户端重新加载原始配置。

### 11.2 `disable` 不会删除什么

正常 `disable` 不会删除：

- 项目源码；
- `runtime/config.json`；
- 已构建 daemon；
- 本地日志；
- macOS Keychain 中的 Wopal AI API Key。

这样以后重新启用时不需要重新输入所有配置。

### 11.3 可选：删除 Keychain API Key

如果确定不再使用 Gateway，可以在已经执行 `disable` 之后手工删除 Keychain 项：

```sh
/usr/bin/security delete-generic-password \
  -a wopal-ai \
  -s com.wopal.codex-gateway.wopal-ai
```

这一步是不可逆的凭证清理，不属于默认 `disable` 行为。

### 11.4 可选：完全删除项目目录

确认已经：

```sh
./scripts/gateway disable
```

并且：

```sh
./scripts/gateway status
```

不再显示已安装/健康服务后，如果不再需要源码、日志和本地配置，可以通过 Finder 将整个项目目录移到废纸篓。

不要在 Codex 仍指向 Gateway 时直接删除项目目录。

## 12. 故障排查

### 12.1 官方模型突然不见了

先刷新 catalog：

```sh
./scripts/gateway catalog refresh
```

然后完全退出并重启 ChatGPT/Codex Desktop。

Gateway 的官方 catalog 基线来自：

```sh
codex debug models --bundled
```

而不是 `~/.codex/models_cache.json`。

如果仍异常，检查：

```sh
codex debug models
```

### 12.2 Wopal 模型没有显示

依次执行：

```sh
./scripts/gateway auth test
./scripts/gateway models list
./scripts/gateway models sync
./scripts/gateway catalog refresh
```

然后重启 ChatGPT/Codex Desktop。

### 12.3 服务不健康

```sh
./scripts/gateway status
```

再检查：

```sh
curl -v http://127.0.0.1:11435/_health
```

本地日志：

```text
logs/gateway.log
logs/service.log
logs/service-error.log
```

查看最近日志可以使用：

```sh
tail -n 100 logs/service-error.log
tail -n 100 logs/service.log
tail -n 100 logs/gateway.log
```

首次运行新构建的 macOS 二进制偶尔可能受到系统安全扫描影响而启动较慢。安装器会最多等待约 90 秒健康检查，不应因为第一次十几秒没有响应就立刻判定 daemon 已崩溃。

### 12.4 API Key 验证失败

检查配置：

```sh
node dist/cli.js config show
./scripts/gateway auth status
./scripts/gateway auth test
```

确认 `wopalAi.baseUrl` 正确，并且该 API Key 能访问对应 `/models`。

需要重新录入：

```sh
./scripts/gateway auth set
```

### 12.5 修改模型后当前会话仍使用旧 metadata

模型 catalog 是 Codex 客户端加载的本地 metadata。执行：

```sh
./scripts/gateway catalog refresh
```

然后：

- 重启 ChatGPT/Codex Desktop；或
- 对已经加载旧 catalog 的会话重新打开/分叉。

### 12.6 Provider 切换时看到 WebSocket 1012

如果日志或客户端显示 `1012 Service Restart`，并且发生在：

- 官方模型 <-> Wopal AI 切换；
- quota failover；
- quota reset 后 failback；

这通常是预期行为。Gateway 用它要求 Codex 重新建立一个绑定正确 Provider 和凭证的 socket，并重放完整输入。

### 12.7 老会话遇到 legacy native compaction

很早的会话可能仍然包含 OpenAI native opaque compaction。

Gateway 会尝试通过官方 Provider 将它物化为可继续使用的历史，而不是静默丢弃。

如果此时官方 quota 已进入 protected 状态，legacy native compaction restore 可能会被暂停到额度 reset。新的 compaction 已经使用 `wgcp1`，不会继续制造这种官方专属 opaque state。

### 12.8 正常卸载后客户端仍显示 Gateway 模型

执行：

```sh
./scripts/gateway disable
```

然后**完全退出并重新启动** ChatGPT/Codex Desktop。

客户端运行期间通常会缓存模型目录，仅修改磁盘配置并不会立即刷新 UI。

### 12.9 没有收到 quota failover 通知

依次检查：

1. `terminal-notifier` 是否可用：`command -v terminal-notifier`；
2. 「系统设置 → 通知」里是否允许 `terminal-notifier` 发送通知；
3. `runtime/config.json` 中 `notifications.enabled` 是否为 `false`；
4. 当前保护周期是否已经通知过（同一保护周期、同一窗口、同一被替换模型只通知一次）；
5. `logs/gateway.log` 中的 `quota_failover_notified`、`quota_recovery_notified`、`quota_failover_notify_unavailable`、`quota_failover_notify_failed` 事件；
6. Gateway 是否在保护周期内重启过（重启会丢失「恢复通知」的定时器，该周期不会再补发）。

## 13. 安全与本地状态

### API Key

Wopal AI API Key 保存在 macOS Keychain。

Gateway 生成的：

```text
runtime/cliproxy-runtime.json
```

只保存：

```text
__KEYCHAIN__
```

占位符。真实 Key 只在 daemon 启动时读取到内存并注入内嵌 CLIProxyAPI SDK 配置。

### 本地监听

默认：

```text
127.0.0.1:11435
```

Gateway 设计为本机用户服务，不是公开多租户 API。

### 额度状态

官方 quota 状态保存在：

```text
runtime/official-quota-state.json
```

ChatGPT account ID 不以明文保存，而是使用 SHA-256 后的 key。

### Codex 恢复状态

安装前需要恢复的配置保存在：

```text
runtime/codex-restore.json
```

该文件用于 `scripts/gateway disable` / `codex restore`，不要在 Gateway 正常启用期间手工删除。

### Git

运行时配置、日志、Keychain 相关状态、生成的 daemon 和其他本地状态都应保持 Git ignored，不应提交敏感凭证。

## 14. 开发与验证

项目主要由两层组成：

- Go daemon：`cmd/gatewayd/`
- TypeScript CLI：`src/`

CLIProxyAPI 不是独立运行的第二个 Proxy。项目把：

```text
github.com/router-for-me/CLIProxyAPI/v8
```

作为 Go SDK 直接编译进 `wopal-codex-gatewayd`。完整设计见 [`docs/DESIGN.md`](docs/DESIGN.md)。

标准验证命令：

```sh
go test ./...
go vet ./...
pnpm test
pnpm typecheck
```

构建 TypeScript CLI：

```sh
pnpm build
```

构建 daemon：

```sh
go build -o runtime/bin/wopal-codex-gatewayd ./cmd/gatewayd
```

Node 测试设置了全局 timeout 和强制退出策略，WebSocket/server fixture 也应在成功和失败路径都释放资源。测试完成后不应残留 `node --test`、`tsx`、`esbuild` 或 WebSocket 测试进程。

---

如果只是第一次安装，最短可执行流程是：

```sh
pnpm install
pnpm build
mkdir -p runtime/bin
go build -o runtime/bin/wopal-codex-gatewayd ./cmd/gatewayd

./scripts/gateway config wopal --base-url https://your-new-api.example.com/v1
./scripts/gateway auth set
./scripts/gateway models sync
./scripts/gateway models set deepseek-v4.1-flash --context-window 1000000 --reasoning --no-vision
./scripts/gateway install
./scripts/gateway status
```

然后重启 ChatGPT/Codex Desktop，即可开始使用。