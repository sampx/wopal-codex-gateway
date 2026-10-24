# Wopal Codex Gateway

[English](README.md) | 简体中文

Wopal Codex Gateway 是运行在 macOS 本机上的 Codex 路由网关，交付形态是单个 Go 二进制 `runtime/bin/wopal-codex-gateway`。它让 ChatGPT / Codex Desktop 或 Codex CLI 在保留官方模型能力的同时，也能在同一个模型选择器、同一个会话中使用 `wopal-ai/*` 模型。

它不只是一个 HTTP 代理，还负责模型目录合并、Codex 配置接管与恢复、WebSocket/HTTP 跨 Provider 路由、可移植的会话压缩、老会话兼容，以及官方额度保护与自动 failover/failback。

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

### 只有两种模型命名空间

- 无命名空间，例如 `gpt-6.1-sol`，走官方线路；
- `wopal-ai/<model>`，例如 `wopal-ai/deepseek-v4.1-flash`，走 Wopal AI。

其他命名空间一律拒绝，返回 HTTP 404 和 `model namespace is disabled`。官方凭证在进入 Wopal AI 路径前会被移除，不会转发给第三方 Provider。

### 同一会话可跨 Provider 切换

同一个 Codex 会话可以在官方模型与 Wopal AI 之间来回切换：

```text
官方模型 -> Wopal AI -> 官方模型
```

WebSocket 跨 Provider 时，Gateway 会用 `1012 Service Restart` 关闭旧连接，让 Codex 重新连接并在绑定正确 Provider 与凭证的 socket 上重放完整输入。

### 会话压缩由 Gateway 接管

新的会话压缩统一由：

```text
wopal-ai/deepseek-v4.1-flash
```

生成摘要，再封装为 Gateway 自己的 `wgcp1` 可移植 capsule，不再生成只能由 OpenAI 理解的 opaque 压缩状态。

### Codex Memory 走 Wopal AI

Gateway 安装期间会配置：

```toml
[memories]
extract_model = "wopal-ai/deepseek-v4.1-flash"
consolidation_model = "wopal-ai/deepseek-v4.1-flash"
```

因此 Memory extraction / consolidation 不会消耗主要官方模型额度。

### 官方额度自动保护

基础保护阈值：

| 额度窗口 | 基础阈值 |
| --- | ---: |
| 5 小时 | 使用达到 97% |
| 每周 | 使用达到 99% |

保护粒度是每一次新的 sampling step，即每个 `response.create` / `response.append` 之前重新判断，而不是每个用户回合只判断一次。

上游只上报整数百分比，一步可能消耗不止 1 个点。为避免越过阈值，Gateway 会按「已观测最大单步超出最小余量的部分」提前停：5 小时窗口最小余量 2 个点，每周窗口最小余量 1 个点。观测步长默认最多按 5 个点计算（`quota.maxStepCap`，范围 1–10），因此在默认上限下有效阈值最低分别为 94% 和 95%。没有观测到大步长时，有效阈值等于基础阈值。

进入保护区后：

```text
请求的官方模型
    -> quota guard
    -> wopal-ai/deepseek-v4.1-flash
```

额度 reset 后，下一次安全的 sampling step 会重新连接并切回原来请求的官方模型。

```sh
runtime/bin/wopal-codex-gateway quota reset-learning
```

会清除已学习的单步值，让有效阈值回到基础值，直到再次观测到新的步长。

### 默认不落盘请求正文

请求与响应正文从不写入磁盘。日志只记录 metadata，例如状态码、模型、错误码、request ID 和耗时。`/_status` 会报告：

```json
{"request_body_logging": "disabled"}
```

### Codex 接入可逆

`codex install` 只修改以下 Codex 配置：

- `model_catalog_json`
- `openai_base_url`
- `[memories] extract_model`
- `[memories] consolidation_model`

**不修改当前 `model`。** `codex restore` 会恢复这些键在安装前保存的值；`model` 仅在当前值仍以 `wopal-ai/` 开头时回退到安装前取值，如果你已经切到官方模型，则保留你的选择。

## 2. 系统要求

当前运行环境是 macOS，因为项目依赖：

- macOS Keychain 保存 Wopal AI API Key；
- LaunchAgent 管理后台服务；
- 本机 Codex / ChatGPT Desktop 配置目录。

你需要：

- macOS；
- 已安装 ChatGPT/Codex Desktop 或 Codex CLI；
- 可执行的 `codex` 命令，并支持 `codex debug models --bundled`；
- 构建需要 Go `1.26.x`（以 `go.mod` 为准）；运行时不依赖 Node.js；
- 一个可用的 Wopal AI / New API OpenAI-compatible endpoint；
- 对应的 API Key；
- （可选）`terminal-notifier`：额度 failover 时弹 macOS 通知，安装方式 `brew install terminal-notifier`；未安装时只是不弹通知，路由功能不受影响。

检查基本环境：

```sh
codex --version
go version
```

Gateway 还需要能够执行：

```sh
codex debug models --bundled
```

它使用 Codex 当前版本自带的 bundled catalog 作为官方模型目录真相源。

## 3. 从源码构建

以下命令都在项目根目录执行。

```sh
mkdir -p runtime/bin
go build -o runtime/bin/wopal-codex-gateway ./cmd/gateway
```

最终二进制：

```text
runtime/bin/wopal-codex-gateway
```

`scripts/gateway` 只是同一个二进制的薄包装器，二进制缺失时会直接报错。验证命令见 [第 14 节](#14-开发与验证)。

## 4. 首次配置

### 4.1 创建或升级 `runtime/config.json`

Gateway 读取 `runtime/config.json`。文件不存在时使用默认配置；每次写入都使用临时文件加 rename，权限为 `0600`。

已存在的文件**必须**包含 `"version": 1`。完整 schema：

```json
{
  "version": 1,
  "listen": { "host": "127.0.0.1", "port": 11435 },
  "wopalAi": { "baseUrl": "", "models": [], "excludedModels": [] },
  "notifications": { "enabled": true, "sound": "default", "tool": "" },
  "logs": { "maxSizeMb": 10, "maxBackups": 5 },
  "quota": { "maxStepCap": 5 }
}
```

规则：

- `version` 必须是 `1`；其他值会在启动前被拒绝。
- 缺少 `listen` 时默认 `127.0.0.1:11435`。
- 缺少 `logs` 时默认单文件 10 MB、保留 5 份备份。
- `quota.maxStepCap` 是已观测最大单步的上限，默认 `5`，允许范围 `1–10`；缺省、`<= 0` 或超出范围时按默认 `5`。
- `wopalAi.models` 是配置的模型列表；`excludedModels` 只影响 `models sync`。
- `wopalAi.models` 缺失时使用内置默认项。

**从旧版本升级：** 如果 `runtime/config.json` 已经存在，启动新二进制前要先做两件事。

1. 加入 `"version": 1`。
2. 删除 `"ollama"` 块。Ollama 支持已经移除，残留的块会被明确拒绝，而不是被静默忽略。

缺少 `version` 时会失败并指出：

```text
unsupported gateway config version 0 (expected 1)
```

仍带已移除段落时会失败并指出：

```text
unsupported configuration section "ollama". Ollama support was removed; delete the "ollama" block
```

### 4.2 CLI 总览

```text
serve
config show
config wopal --base-url <url> [--model <name>...] [--context-window 262144] [--vision] [--no-reasoning]
auth set
auth test
auth status
models list
models sync
models add <name...>
models set <name...>
models remove <name...>
catalog refresh
codex install
codex restore
service install
service start
service stop
service restart
service status
service uninstall
quota reset-learning
help
```

### 4.3 配置 Wopal AI 地址

```sh
runtime/bin/wopal-codex-gateway config wopal --base-url https://your-new-api.example.com/v1
```

`--base-url` 应填写 OpenAI-compatible API 的根地址。

也可以同时设置模型列表：

```sh
runtime/bin/wopal-codex-gateway config wopal \
  --base-url https://your-new-api.example.com/v1 \
  --model deepseek-v4.1-flash \
  --context-window 262144
```

`--model` 会替换已配置的模型列表。`--context-window`（默认 `262144`）、`--vision`、`--no-reasoning` 作用于这些模型项。用这种方式修改模型列表后，需要执行 `catalog refresh` 重建 Codex catalog。

只读查看当前配置：

```sh
runtime/bin/wopal-codex-gateway config show
```

修改 `runtime/config.json` 后，`serve` 会在下次启动时读取新值；服务已安装时需要重启服务。

### 4.4 保存 Wopal AI API Key

```sh
runtime/bin/wopal-codex-gateway auth set
```

API Key 使用 macOS `security` 命令写入 Keychain：

```text
service: com.wopal.codex-gateway.wopal-ai
account: wopal-ai
```

真实 API Key 不写入项目配置文件。`auth set` 写入后会立即调用 Wopal AI `/models` 进行验证。

检查状态并重新测试：

```sh
runtime/bin/wopal-codex-gateway auth status
runtime/bin/wopal-codex-gateway auth test
```

### 4.5 查看与同步模型

```sh
runtime/bin/wopal-codex-gateway models list
```

该命令使用已保存的 Key 读取远端 `/models`。

```sh
runtime/bin/wopal-codex-gateway models sync
```

`models sync` 会：

1. 从 Wopal AI `/models` 获取模型 ID；
2. 跳过明显的 embedding 与 rerank 模型；
3. 保留已经手工设置过的模型 metadata；
4. 遵守本地 exclusion 列表；
5. 更新 `runtime/config.json`；
6. 重新生成 Codex 合并模型目录。

新发现的模型默认 metadata：

```text
context window: 262144
vision: false
reasoning: true
```

这些只是本地 Codex metadata，不会修改上游模型本身。

### 4.6 模型目录

`catalog refresh` 从 `codex debug models --bundled` 读取官方基线，合并已配置的 Wopal AI 模型，写入：

```text
~/.codex/wopal-codex-gateway-models.json
```

刷新失败时，上一份可用目录会保留，命令以非 0 状态退出并说明原因。常见原因包括：

- `codex` 命令缺失；
- `codex debug models --bundled` 返回非 0 退出码；
- 命令输出非法 JSON；
- bundled catalog 为空。

### 4.7 `scripts/gateway` 包装器

`scripts/gateway <command>` 运行同一个二进制，只重命名两个命令：

| 包装器命令 | 实际命令 |
| --- | --- |
| `install` | `codex install` |
| `disable` | `codex restore` |
| `start`、`stop`、`restart`、`status` | `service start`、`service stop`、`service restart`、`service status` |
| 其他 | 原样透传 |

## 5. 启用 Gateway

推荐的完整安装命令：

```sh
runtime/bin/wopal-codex-gateway codex install
```

或通过包装器：

```sh
./scripts/gateway install
```

它会完成完整的接入流程：

1. 检查 base URL 与已保存的 API Key；
2. 从 `codex debug models --bundled` 读取官方模型目录；
3. 合并官方模型与 `wopal-ai/*` 模型；
4. 生成 Gateway 专用 Codex model catalog；
5. 安装并启动 `com.wopal.codex-gateway` LaunchAgent，程序参数为 `<binary> serve --project-root <root>`；
6. 等待 `/_health` 健康检查通过；
7. 记录安装前的 Codex 配置恢复点；
8. 将 Codex `openai_base_url` 指向本地 Gateway；
9. 将 Codex `model_catalog_json` 指向合并后的 catalog；
10. 配置 Memory extraction / consolidation 使用 Wopal AI；
11. 保留用户其他 Codex 配置与当前 `model` 不变。

二进制缺失、配置非法或健康检查失败时，命令会在打印成功前以非 0 状态退出。

### 重启 ChatGPT/Codex Desktop

安装完成后，请完全退出并重新启动 ChatGPT/Codex Desktop，让客户端重新读取：

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

Codex 内部 `visibility=hide` 的模型仍会留在 catalog 中供内部流程使用，但不应显示在正常用户模型选择器里。

## 6. 验证安装

### 6.1 检查服务

```sh
runtime/bin/wopal-codex-gateway service status
```

正常结果：

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

会显示脱敏后的运行状态，包括：

- 监听地址与官方线路状态；
- 5 小时 / 每周 quota 百分比、reset 时间与 failover 状态；
- quota guard 的阈值与已观测步长：

```text
official_quota.five_hour_stop_percent
official_quota.weekly_stop_percent
official_quota.effective_five_hour_stop_percent
official_quota.effective_weekly_stop_percent
official_quota.observed_five_hour_max_step
official_quota.observed_weekly_max_step
```

- `request_body_logging: "disabled"`。

`/_status` 不暴露 API Key 或原始 ChatGPT account ID。

### 6.4 再次验证 Wopal API

```sh
runtime/bin/wopal-codex-gateway auth test
```

### 6.5 检查模型 catalog

从 Codex 侧检查当前实际模型目录：

```sh
codex debug models
```

官方模型的基线来自：

```sh
codex debug models --bundled
```

Gateway 不使用容易被运行时覆盖的 `~/.codex/models_cache.json` 作为官方 canonical source。

## 7. 日常使用

### 使用官方模型

在 Codex 模型选择器中直接选择没有命名空间的官方模型，例如：

```text
gpt-6.1-sol
```

请求会保持官方路径。

### 使用 Wopal AI 模型

选择：

```text
wopal-ai/deepseek-v4.1-flash
```

或其他同步进来的 `wopal-ai/*` 模型。

### 在同一个老会话中切换模型

无需为了使用 Gateway 清理老会话。正常情况下，同一个会话可以：

```text
Wopal AI -> 官方模型
官方模型 -> Wopal AI
```

新的 `wgcp1` compaction 可以跨 Provider 展开。

对于非常早期、仍只包含 OpenAI native opaque compaction 的历史会话，第一次切到第三方 Provider 时可能需要通过官方线路先物化一次历史摘要；Gateway 不会为了切换 Provider 静默丢弃这部分历史。

### 自动 compaction

无论当前前台模型是官方还是 Wopal AI，新的会话 compaction 都由 Gateway 接管，使用：

```text
wopal-ai/deepseek-v4.1-flash
```

这不是 Codex `[memories]` 配置，而是 Gateway runtime 策略。

### 自动 quota failover/failback

每次新的 sampling step 之前，Gateway 都会重新检查额度。处于保护区的官方请求会切到 Wopal fallback 模型；reset 之后，下一次安全的 step 会切回原来请求的官方模型。

Provider 切换、quota failover 或 failback 时出现 WebSocket `1012 Service Restart` 是预期控制行为，Codex 应重新建立连接并重放输入。

两个方向都会弹 macOS 通知，每个保护周期各一次，不会在大批请求里反复弹窗：

- 切换发生时：告诉你请求的官方模型被换成了哪个模型、额度大约什么时候恢复；
- 额度 reset 时：告诉你可以切回官方模型了（由定时器驱动，不需要你先发一次请求）。

想随时确认「现在这句跑在哪个模型上」，看这里：

```sh
curl -fsS http://127.0.0.1:11435/_status
```

`official_quota.state` 为 `protected` 表示官方额度处于保护区；`official_quota.failover.active` 为 `true` 表示官方请求正在被自动换成 Wopal 模型。

通知依赖 `terminal-notifier`；未安装时不会弹通知，路由功能不受影响。需要关闭通知时，在 `runtime/config.json` 中设置：

```json
"notifications": { "enabled": false }
```

## 8. 模型管理

### 查看远端模型

```sh
runtime/bin/wopal-codex-gateway models list
```

### 同步可用模型

```sh
runtime/bin/wopal-codex-gateway models sync
```

同步后需要重启 ChatGPT/Codex Desktop 才能确保模型选择器重新加载 catalog。

### 手工添加模型

```sh
runtime/bin/wopal-codex-gateway models add my-model --context-window 262144
```

支持：

```text
--context-window N
--vision
--no-reasoning
```

例如：

```sh
runtime/bin/wopal-codex-gateway models add vision-model \
  --context-window 131072 \
  --vision
```

### 修改模型 metadata

```sh
runtime/bin/wopal-codex-gateway models set deepseek-v4.1-flash \
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

### 移除模型

```sh
runtime/bin/wopal-codex-gateway models remove model-a model-b
```

被移除的模型会加入本地 exclusion 列表，后续 `models sync` 不会自动把它们重新加回来。

要恢复被排除的模型，显式执行：

```sh
runtime/bin/wopal-codex-gateway models add model-a
```

### 刷新 catalog

```sh
runtime/bin/wopal-codex-gateway catalog refresh
```

适合以下场景：

- Codex 升级后官方 bundled catalog 发生变化；
- 手工调整了模型配置；
- 模型选择器与当前配置不同步。

刷新后重启 ChatGPT/Codex Desktop。

## 9. 服务管理

### 查看状态

```sh
runtime/bin/wopal-codex-gateway service status
```

### 临时停止

```sh
runtime/bin/wopal-codex-gateway service stop
```

这只停止 Gateway 服务，**不会恢复 Codex 配置**。因此 `service stop` 适合短时间维护，不适合正式卸载。服务停止期间 Codex 仍可能指向 `127.0.0.1:11435`，此时模型请求会失败。

### 启动

```sh
runtime/bin/wopal-codex-gateway service start
```

前提是 LaunchAgent 已安装。

### 重启

```sh
runtime/bin/wopal-codex-gateway service restart
```

### 只安装服务

```sh
runtime/bin/wopal-codex-gateway service install
```

该命令只负责 LaunchAgent，不负责完整 Codex 接入。二进制缺失、配置非法或服务未通过 `/_health` 时，会以非 0 状态退出。

### 只卸载服务

```sh
runtime/bin/wopal-codex-gateway service uninstall
```

**不要把它当作正常卸载命令。** 它只移除 LaunchAgent，不会恢复 Codex 配置。如果 Codex 仍然指向 Gateway，而服务已经卸载，Codex 请求会失败。

正常停用请使用：

```sh
runtime/bin/wopal-codex-gateway codex restore
```

或 `./scripts/gateway disable`。

## 10. 更新 Gateway

### 10.1 升级既有配置

如果 `runtime/config.json` 来自旧版本，在启动新二进制前先按
[从旧版本升级](#从旧版本升级) 的两步处理：加入 `"version": 1`，并删除旧的 `"ollama"` 块。
否则 `serve` 和所有配置命令都会拒绝该文件。

### 10.2 临时停止服务

```sh
runtime/bin/wopal-codex-gateway service stop
```

### 10.3 重新构建二进制

```sh
go build -o runtime/bin/wopal-codex-gateway ./cmd/gateway
```

### 10.4 重新应用安装状态

```sh
runtime/bin/wopal-codex-gateway codex install
```

重复执行 `install` 是支持的。Gateway 会保留最初的 Codex restore point，而不是把 Gateway 自己已经修改后的配置错误地当作「原始配置」。该命令会安装 LaunchAgent、等待 `/_health` 通过，然后才报告成功。

### 10.5 重启客户端

完全退出并重启 ChatGPT/Codex Desktop。

如果只是模型列表变化，也可以只执行：

```sh
runtime/bin/wopal-codex-gateway catalog refresh
```

然后重启客户端。

## 11. 停用与卸载

### 11.1 推荐：完整停用 Gateway

使用：

```sh
runtime/bin/wopal-codex-gateway codex restore
```

或 `./scripts/gateway disable`。

这是真正的用户级「卸载/恢复」入口。它会：

1. 恢复 Gateway 安装前保存的 Codex 配置；
2. 恢复原来的 `openai_base_url`；
3. 恢复原来的 `model_catalog_json`；
4. 恢复原来的 Memory model override；
5. 仅当当前 `model` 仍以 `wopal-ai/` 开头时恢复安装前取值，否则保留你当前选择的官方模型；
6. 卸载并移除 LaunchAgent；
7. 删除 Gateway 生成的 Codex 合并 catalog；
8. 删除本次安装使用的 restore state。

完成后必须重启 ChatGPT/Codex Desktop，让客户端重新加载原始配置。

### 11.2 restore 不会删除什么

正常 restore 不会删除：

- 项目源码；
- `runtime/config.json`；
- 已构建的二进制；
- 本地日志；
- macOS Keychain 中的 Wopal AI API Key。

这样以后重新启用时不需要重新输入所有配置。

### 11.3 可选：删除 Keychain API Key

在 `codex restore` 之后，如果确定不再使用 Gateway，可以手工删除 Keychain 项：

```sh
/usr/bin/security delete-generic-password \
  -a wopal-ai \
  -s com.wopal.codex-gateway.wopal-ai
```

这一步是不可逆的凭证清理，不属于默认 restore 行为。

### 11.4 可选：完全删除项目目录

先执行 `codex restore`，并确认服务不再运行。如果不再需要源码、日志和本地配置，可以通过 Finder 将整个项目目录移到废纸篓。

不要在 Codex 仍指向 Gateway 时直接删除项目目录。

## 12. 故障排查

### 启动时报配置版本不受支持

按 [从旧版本升级](#从旧版本升级) 的两步处理：加入 `"version": 1`，并删除旧的 `"ollama"` 块。
错误信息会指出文件路径和期望的版本号。

### 启动时报 unsupported configuration section

Ollama 支持已经移除。请从 `runtime/config.json` 删除 `"ollama"` 块，它不会被自动迁移。

### 官方模型突然不见了

先刷新 catalog：

```sh
runtime/bin/wopal-codex-gateway catalog refresh
```

然后完全退出并重启 ChatGPT/Codex Desktop。

如果刷新失败，请阅读错误信息：它会区分 `codex` 命令缺失、非 0 退出码、非法 JSON 和空目录。上一份可用 catalog 会保留。

检查：

```sh
codex debug models
codex debug models --bundled
```

Gateway 的官方 catalog 基线来自 `codex debug models --bundled`，而不是 `~/.codex/models_cache.json`。

### Wopal 模型没有显示

依次执行：

```sh
runtime/bin/wopal-codex-gateway auth test
runtime/bin/wopal-codex-gateway models list
runtime/bin/wopal-codex-gateway models sync
runtime/bin/wopal-codex-gateway catalog refresh
```

然后重启 ChatGPT/Codex Desktop。

### 服务不健康

```sh
runtime/bin/wopal-codex-gateway service status
curl -v http://127.0.0.1:11435/_health
```

本地日志：

```text
logs/gateway.log
logs/service.log
logs/service-error.log
```

查看最近日志：

```sh
tail -n 100 logs/service-error.log
tail -n 100 logs/service.log
tail -n 100 logs/gateway.log
```

首次运行新构建的 macOS 二进制偶尔可能受到系统安全扫描影响而启动较慢。安装器最多等待约 90 秒的 `/_health` 健康检查，不应因为第一次十几秒没有响应就立刻判定服务已崩溃。

### API Key 验证失败

检查配置与认证状态：

```sh
runtime/bin/wopal-codex-gateway config show
runtime/bin/wopal-codex-gateway auth status
runtime/bin/wopal-codex-gateway auth test
```

确认 `wopalAi.baseUrl` 正确，并且该 API Key 能访问对应 `/models`。

需要重新录入：

```sh
runtime/bin/wopal-codex-gateway auth set
```

### 修改模型后当前会话仍使用旧 metadata

```sh
runtime/bin/wopal-codex-gateway catalog refresh
```

然后重启 ChatGPT/Codex Desktop，或对已经加载旧 catalog 的会话重新打开/分叉。

### Provider 切换时看到 WebSocket 1012

如果日志或客户端显示 `1012 Service Restart`，并且发生在：

- 官方模型 <-> Wopal AI 切换；
- quota failover；
- quota reset 后 failback；

这通常是预期行为。Gateway 用它要求 Codex 重新建立绑定正确 Provider 和凭证的 socket，并重放完整输入。

### 老会话遇到 legacy native compaction

很早的会话可能仍然包含 OpenAI native opaque compaction。

Gateway 会尝试通过官方 Provider 将它物化为可继续使用的历史，而不是静默丢弃。

如果此时官方 quota 已进入 protected 状态，legacy native compaction restore 可能会被暂停到额度 reset。新的 compaction 已经使用 `wgcp1`，不会继续制造这种官方专属 opaque state。

### 还没到 97% / 99% 就停了

观测到较大单步时，有效阈值会低于基础阈值。检查：

```sh
curl -fsS http://127.0.0.1:11435/_status
```

比较 `official_quota.five_hour_stop_percent`、`official_quota.weekly_stop_percent` 与 `official_quota.effective_five_hour_stop_percent`、`official_quota.effective_weekly_stop_percent`，并查看 `observed_five_hour_max_step`、`observed_weekly_max_step`。

清除学习值：

```sh
runtime/bin/wopal-codex-gateway quota reset-learning
```

### 正常卸载后客户端仍显示 Gateway 模型

执行：

```sh
runtime/bin/wopal-codex-gateway codex restore
```

然后**完全退出并重新启动** ChatGPT/Codex Desktop。客户端运行期间通常会缓存模型目录，仅修改磁盘配置并不会立即刷新 UI。

### 没有收到 quota failover 通知

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

### 日志

日志只包含 metadata：

```text
logs/gateway.log
logs/service.log
logs/service-error.log
```

按 `runtime/config.json` 中的 `logs.maxSizeMb` 与 `logs.maxBackups` 轮转（默认单文件 10 MB、保留 5 份备份）。请求与响应正文从不写入磁盘，`/_status` 报告 `request_body_logging: "disabled"`。

### 额度状态

官方 quota 状态保存在：

```text
runtime/official-quota-state.json
```

ChatGPT account ID 不以明文保存，而是使用 SHA-256 后的 key。

### Codex 恢复状态

`codex restore` 需要的安装前配置保存在：

```text
runtime/codex-restore.json
```

不要在 Gateway 正常启用期间手工删除该文件。

### Git

运行时配置、日志、构建产物、认证状态和其他本地运行数据都应保持 Git ignored，不应提交敏感凭证。

## 14. 开发与验证

CLI 与依赖装配位于 `cmd/gateway/`。业务能力拆分在 `internal/*`：`internal/config`（版本化配置）、`internal/codex`（Codex 接管/恢复）、`internal/catalog`（模型目录）、`internal/keychain`、`internal/launchd`（LaunchAgent 生命周期）、`internal/routing`、`internal/proxy`、`internal/quota`、`internal/compaction`、`internal/notify`、`internal/activity`。`cmd/gateway` 只解析子命令并把它们装配进进程。

CLIProxyAPI 不是独立运行的第二个 Proxy。项目把：

```text
github.com/router-for-me/CLIProxyAPI/v8
```

作为 Go SDK 直接编译进 `wopal-codex-gateway`。完整设计见 [`docs/DESIGN.md`](docs/DESIGN.md)。

标准验证命令：

```sh
go test ./...
go vet ./...
go build -o /tmp/wopal-codex-gateway ./cmd/gateway
```

正式构建：

```sh
go build -o runtime/bin/wopal-codex-gateway ./cmd/gateway
```

---

如果只是第一次安装，最短可执行流程是：

```sh
mkdir -p runtime/bin
go build -o runtime/bin/wopal-codex-gateway ./cmd/gateway

./scripts/gateway config wopal --base-url https://your-new-api.example.com/v1
./scripts/gateway auth set
./scripts/gateway models sync
./scripts/gateway models set deepseek-v4.1-flash --context-window 1000000 --reasoning --no-vision
./scripts/gateway install
./scripts/gateway status
```

如果 `runtime/config.json` 已经存在，先确认其中包含 `"version": 1`，并且没有旧的 `"ollama"` 块。

然后重启 ChatGPT/Codex Desktop，即可开始使用。
