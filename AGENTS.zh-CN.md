---
name: Wopal Codex Gateway AGENT RULES
description: Go 本地路由网关仓库的 Agent 操作规则：路径契约、隐私边界、配置与额度语义。
---

# Agent 开发规范

## 规范引用

以下文档是规范，不是参考资料：

- DESIGN: `docs/DESIGN.md`

`docs/DESIGN.md` 的 `## Project Layout` 定义目录职责，`## Data and State Model` 定义每个状态文件的所有者与权限。改动目录结构或新增状态文件前先读这两节。

## 架构与目录

单 Go 二进制，控制面与数据面同进程。`cmd/gateway` 只解析子命令并装配依赖，规则在各 `internal/` 包内。

| 目录 | 职责 |
| --- | --- |
| `bin/` | 构建产物，唯一可执行入口 |
| `cmd/gateway/` | 进程入口、子命令装配 |
| `internal/config/` | 路径契约、版本化配置 |
| `internal/codex/` | `~/.codex/config.toml` 行级接管与恢复 |
| `internal/catalog/` | bundled catalog 读取、模型合并与能力解析 |
| `internal/proxy/` | 路由 middleware、官方直通、Wopal HTTP/WS |
| `internal/routing/` | 共享路由策略与无状态请求工具 |
| `internal/clientfp/` | 客户端版本指纹与请求形状变化 |
| `internal/quota/` | 额度窗口、阈值、failover 状态机 |
| `internal/compaction/` | `wgcp1` capsule、native restore |
| `internal/keychain/` | macOS Keychain 读写 |
| `internal/launchd/` | plist 渲染、launchctl、健康等待 |
| `internal/activity/` | JSONL metadata 日志与轮转 |
| `internal/notify/` | 额度切换与恢复通知 |
| `launchd/` | LaunchAgent plist 模板 |
| `scripts/` | 面向操作者的薄入口，转发到 `bin/` |
| `docs/` | 目标态设计文档 |
| `runtime/` | 用户配置与跨重启状态，不入 Git |
| `logs/` | 网关与服务日志，不入 Git |
| `recovery/` | 回滚材料，不入 Git，运行路径不读取 |

## 开发命令

| 场景 | 命令 |
| --- | --- |
| 构建 | `go build -o bin/wopal-codex-gateway ./cmd/gateway` |
| 编译检查 | `go build ./...` |
| 静态检查 | `go vet ./...` |
| 格式 | `gofmt -l cmd internal`（应无输出） |
| 全量测试 | `go test -race ./... -count=1` |
| 单包测试 | `go test -race ./internal/quota/ -count=1` |
| 操作入口 | `scripts/gateway <command>` |
| 文档门禁 | `python3 <space>/skills/dev-doc-master/scripts/verify-docset.py docs --main DESIGN.md` |
| 发布验证 | `scripts/validate-release.sh --base <已安装项目根目录>` |

## 实现规则

- **路径只有一个来源。** 所有目录与文件路径都来自 `internal/config/paths.go` 的 helper；不在别处拼 `filepath.Join(root, "runtime", ...)`。新增路径先加 helper。
- **四块目录各司其职。** 可执行文件只进 `bin/`，跨重启状态只进 `runtime/`，人类诊断输出只进 `logs/`，回滚材料只进 `recovery/`。不要新增第五处，也不要把临时文件塞进 `runtime/`。
- **`recovery/` 不被代码引用。** 正常运行路径不读它；它的存在只回答「当前版本不可用时怎么回到可用状态」。发布新版本前必须把当前可执行文件存一份到这里，否则回滚没有可用目标。
- **请求正文永不落盘。** 日志只写 metadata：状态码、模型、错误码、request id、耗时、header 指纹与形状计数。prompt、正文、工具参数、工具输出、凭证、原始账号标识都不得写入任何文件。改日志前先看 DESIGN 的 `### Client compatibility logging` 与 `## Data and State Model`。
- **SDK request logger 保持 nil。** `serve` 通过 `sdkapi.WithRequestLoggerFactory` 返回 nil 是关闭正文捕获的唯一机制。升级 CLIProxyAPI 后必须重新确认 DESIGN 日志规则里的三件事。
- **密钥只经 Keychain 写入。** 真实 API Key 不进入 `config.json`、`cliproxy-runtime.json`、日志或测试夹具。生成的 SDK 配置用 `__KEYCHAIN__` 占位。
- **配置改动保持向后兼容。** `runtime/config.json` 的 `version` 是硬门：不支持的版本在启动前失败。新增字段给默认值；移除字段时明确报错而不是静默忽略。
- **额度阈值语义固定。** 基础阈值 5 小时 `97`、每周 `99`，最小余量 `2`、`1`；`quota.maxStepCap` 只约束学习值，不改变基础阈值。修改停止点逻辑必须同时更新 `/_status` 的 `effective_*` 字段含义。
- **Codex 配置只改 managed keys。** 安装只触碰 `model_catalog_json`、`openai_base_url`、`[memories] extract_model`、`[memories] consolidation_model`；恢复时 `model` 仅在当前值是本 Gateway 记录曾暴露过的 client-visible name 时回退；官方 bare slug 保留用户当前选择。不要整份重写 `~/.codex/config.toml`，保留用户注释与无关配置。
- **写入都要原子。** 配置与状态文件使用临时文件加 rename，权限 `0600`；目录 `0700`。

## 测试

- 可测的纯逻辑走 TDD：先写失败的测试，再写实现。路径契约、配置解析与校验、能力解析、额度阈值计算、路由分类都属于这一层。
- 以下边界用真实宿主验证，不要用假实现冒充已覆盖：Keychain 读写、`launchctl` 装载与健康等待、WebSocket 升级、`codex debug models --bundled` 输出、ChatGPT/Codex Desktop 的实际模型选择器。
- 涉及请求正文的任何改动，必须跑 `TestServedGatewayNeverWritesPromptBodies`。去掉 nil factory 后该测试必须失败，这是它有效的判据。
- 提交前跑完整门禁：`go build ./... && go vet ./... && gofmt -l cmd internal && go test -race ./... -count=1`。

## 发布验证

`go test` 只能证明代码逻辑正确，不能证明真实 Codex 客户端能用这个二进制。在把发布候选装到正在运行的 Gateway 之前，必须先在不碰运行实例的前提下完成端到端验证。

执行 `scripts/validate-release.sh`。它会先构建当前工作树，再把构建产物指向一个临时项目根目录、自由回环端口启动，并把 `CODEX_HOME` 一起重定向到同一个临时目录。正在运行的安装不会被重启、不会被改配置、不会被写入。

脚本本身就是契约：任何一项不通过都 fail closed，并且逐项打印 PASS/FAIL。

| 层次 | 证明什么 |
| --- | --- |
| build | 当前工作树能编译出 `bin/wopal-codex-gateway` |
| isolation | 临时端口不等于已安装端口；运行前后已安装实例的 `/_health` instance id 完全一致 |
| L1 catalog | `/v1/models` 里外部模型与 Wopal AI 模型同时可见 |
| L2 HTTP | `POST /v1/chat/completions` 与 `POST /v1/responses` 都经外部 provider 返回 200 且有正文 |
| L3a websocket | 同一条 socket 连续两轮保持会话上下文 |
| L3b websocket | 切到另一个 provider 的模型时以 1012 关闭，让客户端重发完整输入 |
| L3c websocket | 未注册模型以 1012 fail closed，而不是静默回退 |
| log evidence | turn 日志的 provider 是真正服务的那个；日志仍然只含 metadata |

让这套验证可信的几条规则：

- **绝不用「先把生产切过去」来验证发布候选。** 已安装的 LaunchAgent 固定绝对二进制路径，原地替换等于在任何检查跑之前就把坏候选变成故障。
- **验证期间绝不把 `CODEX_HOME` 指向真实的 `~/.codex`。** 脚本把它重定向进临时根目录；验证过程不得改写操作者的 Codex 配置。
- **验证协议层，不只验证顺路。** Chat Completions、Responses、WebSocket 帧循环是三套独立传输，过了一套不代表过了另外两套。
- **日志证据是检查项，不是附赠品。** turn 事件带真实 provider，验证跑完可以事后审计。若日志分不清两个 provider，这次验证就不构成证据。
- **部分通过等于失败。** L1/L2 通过但任一 L3 失败，不得安装该候选。

默认参数针对 OpenCode Go 的低价模型，可用 `--provider`、`--model`、`--base-url` 或 `WG_VALIDATE_*` 覆盖。不要用自己不愿为几句单句对话付钱的模型做验证。

脚本通过之后，脚本无法替代的人工检查仍然有效：ChatGPT/Codex Desktop 的模型选择器要同时显示官方与外部模型，真实会话要能跨越模型切换继续。这些是 Plan 的 `### User Validation`，只有用户能勾选。

## Code Review Rules

通用审查方法见本空间装配的 `df-implement-review`（代码变更）与 `df-proposal-review`（Plan/提案）。只在 diff 引入问题时报缺陷，并给出具体触发条件、受影响行为与改动代码中的证据；不报既有问题、风格偏好与 CI 已覆盖的内容。

### 本项目检查点

- 日志与错误路径是否仍满足 metadata-only：新增字段里有没有 prompt、正文、工具参数、工具输出、凭证。
- 路径是否只经 `internal/config` 的 helper 获取；有没有在别处重复拼接目录名。
- 配置 schema 改动是否向后兼容：新字段有默认值，被移除的段有明确报错。
- 额度与 failover 改动是否改变基础阈值语义，`/_status` 字段是否仍能解释提前停的原因。
- `~/.codex/config.toml` 的写入是否仍限于 managed keys，是否保留注释与无关配置。
- 涉及凭证的代码路径是否只经 Keychain，测试夹具里有没有真实密钥。
- 新增状态文件是否在 `runtime/` 且已写进 DESIGN 的 `## Data and State Model`。

## User-Supplied Rules


