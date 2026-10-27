# Wopal Codex Gateway

[English](README.md) | 简体中文

Wopal Codex Gateway 是运行在 macOS 本机上的 Codex 路由网关，交付形态是单个 Go 二进制 `bin/wopal-codex-gateway`。它让 ChatGPT / Codex Desktop 或 Codex CLI 在保留官方模型能力的同时，也能在同一个模型选择器、同一个会话中使用 Wopal AI 与已配置的其他外部 Provider 模型。

它不只是一个 HTTP 代理，还负责模型目录合并、Codex 配置接管与恢复、WebSocket/HTTP 跨 Provider 路由、可移植的会话压缩、老会话兼容，以及官方额度保护与自动 failover/failback。

详细架构与行为规范见 [`docs/DESIGN.md`](docs/DESIGN.md)。

## 目录

- [1. 能力概览](#1-能力概览)
- [2. 项目结构](#2-项目结构)
- [3. 系统要求](#3-系统要求)
- [4. 从源码构建](#4-从源码构建)
- [5. 首次配置](#5-首次配置)
- [6. 启用 Gateway](#6-启用-gateway)
- [7. 验证安装](#7-验证安装)
- [8. 日常使用](#8-日常使用)
- [9. 模型管理](#9-模型管理)
- [10. 服务管理](#10-服务管理)
- [11. 更新 Gateway](#11-更新-gateway)
- [12. 回滚](#12-回滚)
- [13. 停用与卸载](#13-停用与卸载)
- [14. 故障排查](#14-故障排查)
- [15. 安全与本地状态](#15-安全与本地状态)
- [16. 开发与验证](#16-开发与验证)

## 1. 能力概览

### 精确模型名路由

- 无命名空间，例如 `gpt-6.1-sol`，走官方线路；
- `wopal-ai/<model>`，例如 `wopal-ai/deepseek-v4.1-flash`，走 Wopal AI；
- 其他已配置外部模型使用 `<provider-id>/<upstream-model>` 或显式 alias。

未知命名空间、已删除 alias 和未知 bare model 都会 fail closed，返回 `model_route_unavailable`。官方凭证在进入外部 Provider 路径前会被移除。

### 同一会话可跨 Provider 切换

同一个 Codex 会话可以在官方模型与 Wopal AI 之间来回切换：

```text
官方模型 -> Wopal AI -> 官方模型
```

WebSocket 跨 Provider 时，Gateway 会用 `1012 Service Restart` 关闭旧连接，让 Codex 重新连接并在绑定正确 Provider 与凭证的 socket 上重放完整输入。

### 会话压缩由 Gateway 接管

新的会话压缩使用当前 `compaction` role，默认目标是：

```text
wopal-ai/deepseek-v4.1-flash
```

生成摘要，再封装为 Gateway 自己的 `wgcp1` 可移植 capsule，不再生成只能由 OpenAI 理解的 opaque 压缩状态。

### Codex Memory 与 compaction 共用 role

Gateway 安装期间会把当前 `compaction` 目标写入 Codex 的两个 memory 字段。默认 role 对应：

```toml
[memories]
extract_model = "wopal-ai/deepseek-v4.1-flash"
consolidation_model = "wopal-ai/deepseek-v4.1-flash"
```

Codex 仍由该 Gateway 接管时，修改或重置 `compaction` 会同步更新这两个受管字段。role 可以指向任意已配置的外部 Provider 模型。

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
    -> 当前 officialFallback role
```

额度 reset 后，下一次安全的 sampling step 会重新连接并切回原来请求的官方模型。

```sh
bin/wopal-codex-gateway quota reset-learning
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

## 2. 项目结构

| 目录 | 放什么 | 说明 |
| --- | --- | --- |
| `bin/` | 构建出来的可执行文件 | 只有 `wopal-codex-gateway` 一个二进制，进去就删 |
| `cmd/gateway/` | 进程入口与子命令 | 只做装配，业务规则在 `internal/` |
| `internal/` | 配置、路由、额度、压缩等实现 | 每个包有独立测试 |
| `launchd/` | LaunchAgent 模板 | 安装时渲染成 `~/Library/LaunchAgents` 下的 plist |
| `scripts/` | `scripts/gateway` 薄入口 | 只是转发到 `bin/`，不实现逻辑 |
| `docs/` | 设计文档 | 目标态规范，不是使用手册 |
| `runtime/` | 配置与运行状态 | 你的设置、额度状态、安装恢复记录 |
| `logs/` | 三个日志文件 | 只记 metadata，不含对话内容 |
| `recovery/` | 回滚用 | 上一版程序，正常运行时完全不用 |

三条规则：

- **可执行、状态、日志分开放。** `bin/` 可以随时删了重建，`runtime/` 删了就丢配置，`logs/` 可以随便清。生命周期不同，所以不同目录。
- **`runtime/` 只放需要跨重启保留的东西。** 临时文件不进这里。
- **`recovery/` 不参与运行。** 正常启动路径从不读它，它只回答一个问题：新版本起不来时怎么退回。

`bin/`、`runtime/`、`logs/`、`recovery/` 都在本机，不进 Git。


## 3. 系统要求

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

## 4. 从源码构建

以下命令都在项目根目录执行。

```sh
mkdir -p bin
go build -o bin/wopal-codex-gateway ./cmd/gateway
```

最终二进制：

```text
bin/wopal-codex-gateway
```

`scripts/gateway` 只是同一个二进制的薄包装器，二进制缺失时会直接报错。验证命令见 [第 16 节](#16-开发与验证)。

## 5. 首次配置

### 5.1 配置文件

Gateway 读 `runtime/config.json`。文件不存在就用默认值；写入走临时文件加 rename，权限 `0600`。

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

`version` 必须是 `1`，其他值在启动前就被拒绝。缺 `listen` 默认 `127.0.0.1:11435`；缺 `logs` 默认 10 MB、留 5 份；`quota.maxStepCap` 默认 `5`，范围 `1–10`，越界按默认值处理。

旧版本的文件如果缺 `version` 或还带 `ollama` 段，用 [5.4](#54-升级旧配置) 的迁移命令处理，不要手工改。

### 5.2 配置地址与 Key

```sh
bin/wopal-codex-gateway config wopal --base-url https://your-new-api.example.com/v1
bin/wopal-codex-gateway auth set      # 写入 Keychain 并立即验证
```

`--base-url` 填 OpenAI-compatible API 的根地址。要同时指定模型：

```sh
bin/wopal-codex-gateway config wopal \
  --base-url https://your-new-api.example.com/v1 \
  --model deepseek-v4.1-flash \
  --context-window 262144
```

`--model` 会替换整个模型列表。只有显式给出的 `--context-window`、`--vision`/`--no-vision`、`--reasoning`/`--no-reasoning` 才存成你的覆盖，其余走 provider 值再走默认值。

Key 存在 Keychain（`service: com.wopal.codex-gateway.wopal-ai`，`account: wopal-ai`），不落任何配置文件。检查状态：

```sh
bin/wopal-codex-gateway auth status
bin/wopal-codex-gateway auth test
```

用 `config show` 只读查看当前配置。改完 `runtime/config.json` 要重启服务才生效。

### 5.3 导入模型

```sh
bin/wopal-codex-gateway models sync
```

读远端 `/models`、跳过 embedding 与 rerank、保留你手工设过的能力值，重建 Codex catalog。新模型默认 100 万上下文、支持图片和推理。增删改模型、看能力来源、何时刷新 catalog 见 [第 9 节](#9-模型管理)。

### 5.4 升级旧配置

```sh
bin/wopal-codex-gateway config migrate --from runtime/config.json --to runtime/config-v1.json
```

生成 `0600` 的候选文件：补版本号、去掉 `ollama` 段、保留 provider 与模型设置；目标已存在时拒绝覆盖。验证候选无误后再替换。该命令不动 Codex 配置，也不启停服务。

### 5.5 `scripts/gateway` 包装器

`scripts/gateway <command>` 转发到同一个二进制，只改了命令名：

| 包装器 | 实际命令 |
| --- | --- |
| `install` | `codex install` |
| `disable` | `codex restore` |
| `start`、`stop`、`restart`、`status` | `service` 同名子命令 |
| 其他 | 原样透传 |


## 6. 启用 Gateway

推荐的完整安装命令：

```sh
bin/wopal-codex-gateway codex install
```

或通过包装器：

```sh
./scripts/gateway install
```

它会完成完整的接入流程：

1. 检查 base URL 与已保存的 API Key；
2. 从 `codex debug models --bundled` 读取官方模型目录；
3. 合并官方、Wopal AI 与所有已配置外部 Provider 模型；
4. 生成 Gateway 专用 Codex model catalog；
5. 安装并启动 `com.wopal.codex-gateway` LaunchAgent，程序参数为 `<binary> serve --project-root <root>`；
6. 等待 `/_health` 健康检查通过；
7. 记录安装前的 Codex 配置恢复点；
8. 将 Codex `openai_base_url` 指向本地 Gateway；
9. 将 Codex `model_catalog_json` 指向合并后的 catalog；
10. 配置 Memory extraction / consolidation 使用当前 `compaction` role；
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

## 7. 验证安装

### 7.1 检查服务

```sh
bin/wopal-codex-gateway service status
```

正常结果：

```text
installed=yes loaded=yes healthy=yes
```

### 7.2 检查健康端点

```sh
curl -fsS http://127.0.0.1:11435/_health
```

正常示例：

```json
{"mode":"single-process","ok":true,"wopal_models":33}
```

### 7.3 查看运行状态与官方额度

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

### 7.4 再次验证 Wopal API

```sh
bin/wopal-codex-gateway auth test
```

### 7.5 检查模型 catalog

从 Codex 侧检查当前实际模型目录：

```sh
codex debug models
```

官方模型的基线来自：

```sh
codex debug models --bundled
```

Gateway 不使用容易被运行时覆盖的 `~/.codex/models_cache.json` 作为官方 canonical source。

## 8. 日常使用

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

无论当前前台模型是官方还是 Wopal AI，新的会话 compaction 都由 Gateway 接管，使用当前 `compaction` role。默认目标为：

```text
wopal-ai/deepseek-v4.1-flash
```

Codex memories 使用同一个 role；因此 Gateway 接管期间两者不会指向不同模型。查看或修改 role：

```sh
bin/wopal-codex-gateway config role
bin/wopal-codex-gateway config role set compaction --provider vendor --model model-name
bin/wopal-codex-gateway config role reset compaction
```

### 自动 quota failover/failback

每次新的 sampling step 之前，Gateway 都会重新检查额度。处于保护区的官方请求会切到当前 `officialFallback` role；reset 之后，下一次安全的 step 会切回原来请求的官方模型。

Provider 切换、quota failover 或 failback 时出现 WebSocket `1012 Service Restart` 是预期控制行为，Codex 应重新建立连接并重放输入。

两个方向都会弹 macOS 通知，每个保护周期各一次，不会在大批请求里反复弹窗：

- 切换发生时：告诉你请求的官方模型被换成了哪个模型、额度大约什么时候恢复；
- 额度 reset 时：告诉你可以切回官方模型了（由定时器驱动，不需要你先发一次请求）。

想随时确认「现在这句跑在哪个模型上」，看这里：

```sh
curl -fsS http://127.0.0.1:11435/_status
```

`official_quota.state` 为 `protected` 表示官方额度处于保护区；`official_quota.failover.active` 为 `true` 表示官方请求正在被自动换成当前 `officialFallback` 目标。Provider 与 role 的 readiness 也会出现在 `/_status` 中。

通知依赖 `terminal-notifier`；未安装时不会弹通知，路由功能不受影响。需要关闭通知时，在 `runtime/config.json` 中设置：

```json
"notifications": { "enabled": false }
```

## 9. 模型管理

模型列表来自 `/models`，能力顺序是：你的显式设置 → provider 明确返回的值 → 网关默认值（100 万上下文、支持图片和推理）。不猜、不接第三方能力库。

### 同步与查看

```sh
bin/wopal-codex-gateway models list     # 读远端 /models
bin/wopal-codex-gateway models sync     # 导入可用模型 + 刷新 catalog
```

`models sync` 会跳过 embedding 与 rerank，保留你手工设过的能力值，尊重本地排除列表，把 provider 明确返回的能力另存到 `runtime/model-metadata.json`，最后重建 Codex catalog。之后重启 ChatGPT/Codex Desktop。

### 手工增删改

```sh
bin/wopal-codex-gateway models add my-model --context-window 262144
bin/wopal-codex-gateway models set deepseek-v4.1-flash --context-window 1000000 --reasoning --no-vision
bin/wopal-codex-gateway models remove model-a model-b
```

三个命令共用同一组参数：

```text
--context-window N
--vision / --no-vision
--reasoning / --no-reasoning
```

`remove` 会把模型加进本地排除列表，后续 `models sync` 不会把它加回来；要恢复就显式 `models add`。

`models set <模型...> --auto` 清除全部能力覆盖；`--reset-context-window`、`--reset-vision`、`--reset-reasoning` 分别清单项。显式设成和默认值相同的值，仍算你的覆盖。

### 看清每个值从哪来

```sh
bin/wopal-codex-gateway models inspect deepseek-v4.1-flash
```

逐字段显示有效值和来源（`user` / `provider` / `default`），并提示 Codex 全局上下文设置可能覆盖逐模型值（网关不动你的全局配置）。

### 何时刷新 catalog

```sh
bin/wopal-codex-gateway catalog refresh
```

三种情况要做：Codex 升级导致官方目录变化、手工改过模型配置、模型选择器与配置不同步。刷新失败会保留上一份可用目录并以非 0 退出。

刷新后重启 ChatGPT/Codex Desktop，模型选择器才会读到新目录。已经加载过某模型 metadata 的老会话，建议重新打开或分叉。


## 10. 服务管理

### 查看状态

```sh
bin/wopal-codex-gateway service status
```

### 临时停止

```sh
bin/wopal-codex-gateway service stop
```

这只停止 Gateway 服务，**不会恢复 Codex 配置**。因此 `service stop` 适合短时间维护，不适合正式卸载。服务停止期间 Codex 仍可能指向 `127.0.0.1:11435`，此时模型请求会失败。

### 启动

```sh
bin/wopal-codex-gateway service start
```

前提是 LaunchAgent 已安装。

### 重启

```sh
bin/wopal-codex-gateway service restart
```

### 只安装服务

```sh
bin/wopal-codex-gateway service install
```

该命令只负责 LaunchAgent，不负责完整 Codex 接入。二进制缺失、配置非法或服务未通过 `/_health` 时，会以非 0 状态退出。

### 只卸载服务

```sh
bin/wopal-codex-gateway service uninstall
```

**不要把它当作正常卸载命令。** 它只移除 LaunchAgent，不会恢复 Codex 配置。如果 Codex 仍然指向 Gateway，而服务已经卸载，Codex 请求会失败。

正常停用请使用：

```sh
bin/wopal-codex-gateway codex restore
```

或 `./scripts/gateway disable`。

## 11. 更新 Gateway

### 11.1 升级步骤

```sh
# 1. 先把手头这个版本存进 recovery/，这是回滚的本钱
cp -p bin/wopal-codex-gateway recovery/wopal-codex-gatewayd

# 2. 停服务
bin/wopal-codex-gateway service stop

# 3. 重新构建
go build -o bin/wopal-codex-gateway ./cmd/gateway

# 4. 重新接管 Codex 并等健康检查通过
bin/wopal-codex-gateway codex install
```

`codex install` 重复执行是安全的：它保留最初的 restore point，不会把网关自己改过的配置当成「原始配置」，并且在 `/_health` 通过之后才报告成功。

### 11.2 配置不兼容时

`runtime/config.json` 缺 `version` 或还带 `ollama` 段时，先按 [5.4](#54-升级旧配置) 生成迁移候选，再替换。否则所有命令都会拒绝这个文件。

### 11.3 只改了模型列表

不需要重新构建，也不需要停服务：

```sh
bin/wopal-codex-gateway catalog refresh
```

然后完全退出并重启 ChatGPT/Codex Desktop。


## 12. 回滚

新版本起不来时，回到上一个可用版本。

### 12.1 什么时候用

- 升级后服务起不来，`curl 127.0.0.1:11435/_health` 没有响应；
- 服务起来了但请求报错，日志指向新版本的问题；
- 想先退回旧版本，再慢慢排查。

### 12.2 回滚步骤

直接运行 recovery 入口：

```sh
recovery/start-gateway.sh
```

脚本严格按下面的顺序执行，任一步失败都会停止：

1. 用**当前版本**执行 `codex restore`，先恢复 Codex 配置并卸载当前 LaunchAgent；
2. 从 `recovery/` 启动上一版，直到 `/_health` 通过；
3. 把已经验证可用的上一版恢复到 `bin/wopal-codex-gateway`，再用**上一版**执行 `codex install`，让上一版重新生成它能理解的 catalog 和 memories 配置。

完成后可以再次确认：

```sh
curl -s 127.0.0.1:11435/_health
```

返回带 `instance` 和 `"ok": true` 的 JSON 就说明上一版正在服务。脚本只有在上一版重新安装 Codex-facing 状态成功后才输出 `RESULT=ROLLED_BACK`。

### 12.3 回滚后的状态

- `runtime/config.json` 和额度状态都是共享的，不需要恢复；
- Codex catalog、memories model 和 Gateway 接管配置由上一版重新生成，不会继续保留新版本才认识的第三方 bare alias；
- 旧版本会自己接管 `logs/`，日志继续写在同一处。

确认旧版本稳定后，再决定是修新版本，还是让 `recovery/` 里的版本继续跑。

### 12.4 保持 recovery 可用

`recovery/` 里的东西不要删。每次发布新版本前都要保持这三样材料可用：上一版 `wopal-codex-gatewayd`、指向它的 `rollback-launchd.plist`，以及 `start-gateway.sh`。它们都是本机 operator material，不进 Git。


## 13. 停用与卸载

### 13.1 推荐：完整停用 Gateway

使用：

```sh
bin/wopal-codex-gateway codex restore
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

### 13.2 restore 不会删除什么

正常 restore 不会删除：

- 项目源码；
- `runtime/config.json`；
- 已构建的二进制；
- 本地日志；
- macOS Keychain 中的 Wopal AI API Key。

这样以后重新启用时不需要重新输入所有配置。

### 13.3 可选：删除 Keychain API Key

在 `codex restore` 之后，如果确定不再使用 Gateway，可以手工删除 Keychain 项：

```sh
/usr/bin/security delete-generic-password \
  -a wopal-ai \
  -s com.wopal.codex-gateway.wopal-ai
```

这一步是不可逆的凭证清理，不属于默认 restore 行为。

### 13.4 可选：完全删除项目目录

先执行 `codex restore`，并确认服务不再运行。如果不再需要源码、日志和本地配置，可以通过 Finder 将整个项目目录移到废纸篓。

不要在 Codex 仍指向 Gateway 时直接删除项目目录。

## 14. 故障排查

### 服务起不来

```sh
bin/wopal-codex-gateway service status
curl -v http://127.0.0.1:11435/_health
tail -n 100 logs/service-error.log
```

新构建的二进制首次运行偶尔被杀毒与系统扫描拖慢。安装器会等约 90 秒的 `/_health`，别因为头十几秒没响应就判定失败。真要退回上一版，见 [第 12 节](#12-回滚)。

### 配置被拒绝

- `unsupported gateway config version 0 (expected 1)`：文件缺 `version`；
- `unsupported configuration section "ollama"`：文件还有已移除的 `ollama` 段。

两种都按 [5.4](#54-升级旧配置) 生成迁移候选，不要手工改。

### 官方模型突然不见了

```sh
bin/wopal-codex-gateway catalog refresh
codex debug models --bundled
```

官方基线只来自 `codex debug models --bundled`，不读 `~/.codex/models_cache.json`。刷新失败会说明是 `codex` 缺失、非 0 退出、非法 JSON 还是空目录，并保留上一份可用目录。刷新后完全退出并重启 ChatGPT/Codex Desktop。

### Wopal 模型没有显示

```sh
bin/wopal-codex-gateway auth test
bin/wopal-codex-gateway models list
bin/wopal-codex-gateway models sync
bin/wopal-codex-gateway catalog refresh
```

都在报错之前先跑完，然后重启客户端。

### API Key 验证失败

```sh
bin/wopal-codex-gateway config show
bin/wopal-codex-gateway auth status
bin/wopal-codex-gateway auth test
```

确认 `wopalAi.baseUrl` 正确、Key 能访问对应 `/models`。需要重录就 `auth set`。

### 改了模型但当前会话还是旧的

```sh
bin/wopal-codex-gateway catalog refresh
```

重启客户端，或对已经加载旧 catalog 的会话重新打开/分叉。

### 看到 WebSocket 1012

发生在官方模型与 Wopal AI 互切、quota failover、或 reset 后 failback 时，`1012 Service Restart` 是预期的：网关用它让 Codex 重连到绑定正确 Provider 与凭证的 socket，并重放输入。

### 老会话遇到 legacy native compaction

很早的会话里可能有 OpenAI native opaque compaction。网关会通过官方线路把它物化成可继续使用的历史，不静默丢弃。如果此时官方额度已在保护区，这一步会等到 reset。新会话用 `wgcp1`，不会再产生这种状态。

### 还没到 97% / 99% 就停了

```sh
curl -fsS http://127.0.0.1:11435/_status
```

对比 `official_quota.five_hour_stop_percent` / `weekly_stop_percent`（基础阈值）与 `effective_five_hour_stop_percent` / `effective_weekly_stop_percent`（实际停止点），再看 `observed_five_hour_max_step` / `observed_weekly_max_step`。观测到大步长时实际停止点会提前，这是保护余量。

清零学习值：

```sh
bin/wopal-codex-gateway quota reset-learning
```

### 卸载后客户端还显示 Gateway 模型

```sh
bin/wopal-codex-gateway codex restore
```

然后**完全退出并重新启动** ChatGPT/Codex Desktop。客户端会在内存里缓存模型目录，只改磁盘不刷新 UI。

### 没收到 quota 切换通知

1. `command -v terminal-notifier` 是否可用；
2. 系统设置 → 通知里是否允许它发通知；
3. `runtime/config.json` 的 `notifications.enabled` 是否为 `false`；
4. 同一保护周期、同一窗口、同一被替换模型只通知一次，是否已经通知过；
5. `logs/gateway.log` 里搜 `quota_failover_notified`、`quota_recovery_notified`、`quota_failover_notify_unavailable`、`quota_failover_notify_failed`；
6. 保护周期内网关是否重启过——重启会丢掉「恢复通知」的定时器，该周期不再补发。


## 15. 安全与本地状态

### API Key

Wopal AI 的 Key 只存在 macOS Keychain：

```text
service: com.wopal.codex-gateway.wopal-ai
account: wopal-ai
```

网关生成的 `runtime/cliproxy-runtime.json` 里只有 `__KEYCHAIN__` 占位符；真实 Key 只在进程启动时读进内存。

### 本地监听

默认 `127.0.0.1:11435`，只服务本机用户，不是公开多租户 API。

### 日志

```text
logs/gateway.log        JSONL metadata，按请求与 turn 记录
logs/service.log        daemon 的 stdout
logs/service-error.log  daemon 的 stderr
```

三个文件共用 `runtime/config.json` 的 `logs.maxSizeMb` 与 `logs.maxBackups`（默认 10 MB、留 5 份）。请求与响应正文从不落盘，`/_status` 固定报告 `request_body_logging: "disabled"`。

每条路由记录带客户端指纹（`client_version`、`originator`、`user_agent`、`encoding`、`beta_features`）与请求形状摘要（工具面、工具数量与摘要、输入条目类型计数、顶层字段名）。同一版本同一模型形状变化时额外记一条 `client_request_contract_changed`。全是 header 与计数，仍然不含正文。

### 本地状态文件

| 文件 | 作用 |
| --- | --- |
| `runtime/config.json` | 你的配置 |
| `runtime/cliproxy-runtime.json` | 每次启动重生成的 SDK 配置，不含密钥 |
| `runtime/cliproxy-auth/` | SDK 要求的认证目录 |
| `runtime/model-metadata.json` | provider 返回的模型能力快照 |
| `runtime/official-quota-state.json` | 额度窗口与学习值，account key 是 SHA-256 |
| `runtime/codex-restore.json` | 安装前的 Codex 配置，restore 时用 |

`codex-restore.json` 在网关启用期间不要手工删。

### Git

`bin/`、`runtime/`、`logs/`、`recovery/` 都在 `.gitignore` 里。凭证、运行状态、日志、构建产物都不进版本库。


## 16. 开发与验证

`cmd/gateway` 只解析子命令并装配依赖；规则在 `internal/*` 各包里。CLIProxyAPI 不是第二个进程，它以 Go SDK 形式编译进来（`github.com/router-for-me/CLIProxyAPI/v8`），完整设计见 [`docs/DESIGN.md`](docs/DESIGN.md)。

### 标准命令

```sh
go build ./...
go vet ./...
gofmt -l cmd internal          # 应无输出
go test -race ./... -count=1
```

### 最短安装流程

```sh
go build -o bin/wopal-codex-gateway ./cmd/gateway

./scripts/gateway config wopal --base-url https://your-new-api.example.com/v1
./scripts/gateway auth set
./scripts/gateway models sync
./scripts/gateway models set deepseek-v4.1-flash --context-window 1000000 --reasoning --no-vision
./scripts/gateway install
./scripts/gateway status
```

然后完全退出并重启 ChatGPT/Codex Desktop。

### 工具调用模式

Wopal 模型统一用 `wopalAi.toolMode`，默认 `code_mode_only`：

| 模式 | 提供什么 |
| --- | --- |
| `code_mode_only` | 只有 `exec` 编排工具 |
| `code_mode` | 编排工具 + 直接工具 |
| `direct` | 只有直接工具 |

官方模型保留自己的模式。

```sh
bin/wopal-codex-gateway config wopal --tool-mode code_mode_only
bin/wopal-codex-gateway catalog refresh
```

也可以在 `runtime/config.json` 的 `wopalAi` 下设 `"toolMode"`。非法值直接报错。切到 `direct` 时，历史里带 `exec` 的对话要新建。改模式不重启网关，但 Codex 要读到新目录才生效。


