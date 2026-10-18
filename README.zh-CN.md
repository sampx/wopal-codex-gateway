# Wopal Codex Gateway

[English](README.md) | 简体中文

Wopal Codex Gateway 是一个本地网关，让 Codex 客户端可以同时使用 ChatGPT / Codex 官方模型和第三方 OpenAI 兼容模型。

项目架构与行为规范见 [`docs/DESIGN.md`](docs/DESIGN.md)。

## 主要能力

- 未带命名空间的模型继续走 ChatGPT / OpenAI 官方线路。
- `wopal-ai/<model>` 路由到已配置的 OpenAI 兼容 Wopal AI 上游。
- 把 Codex WebSocket 流量桥接到第三方 HTTP / SSE provider。
- 切换 provider 时保持会话状态连续，包括 compaction 兼容处理。
- 新的会话 compaction 统一通过 `wopal-ai/deepseek-v4.1-flash` 执行，并保存为 Gateway 自己的可移植 `wgcp1` capsule，不再产生新的 OpenAI native opaque compaction state。
- Gateway 安装期间，Codex Memory extraction / consolidation 统一使用 `wopal-ai/deepseek-v4.1-flash`。
- 在官方订阅额度完全耗尽前进行 sampling-step 级自动保护与故障转移：
  - 5 小时（`300` 分钟）窗口达到 **98% 已使用**时停止继续消耗普通官方推理额度；
  - Weekly（`10080` 分钟）窗口达到 **99% 已使用**时进入保护；
  - `gpt-5.6-luna` / `gpt-6-luna` 保留为官方恢复模型；
  - `codex-auto-review` 保留给权限自动审批流程；
  - 普通官方模型在下一次 `response.create` / `response.append` sampling step 前自动切到 `wopal-ai/deepseek-v4.1-flash`；
  - 额度保护期间，用户直接选择的 `wopal-ai/*` 模型继续正常运行；
  - 官方额度 reset 后，在下一 sampling step 自动重连回原本请求的官方模型。

## Quota Guard

Daemon 会观察 ChatGPT 官方线路返回的 Codex rate-limit metadata，并根据窗口时长识别 5 小时与 Weekly 窗口，而不是依赖 `primary` / `secondary` 的固定位置。

达到保护阈值后，Gateway 不再把下一次普通官方 sampling request 转发到 ChatGPT：HTTP 请求会直接改写到 Wopal fallback 模型；已经建立的官方 WebSocket 会在下一次 `response.create` / `response.append` 前使用 `1012` 关闭，让 Codex 重连并重放完整 input，再由 Wopal bridge 继续执行。`gpt-5.6-luna`、`gpt-6-luna` 和 `codex-auto-review` 保持官方豁免。

Quota reset 后，fallback WebSocket 会在下一 sampling step 触发重连，使 Codex 回到原本请求的官方模型。

旧 OpenAI native compaction 仍通过兼容恢复路径处理；新的 compaction 统一使用可移植 `wgcp1` 状态。

`GET /_status` 可以查看最新 quota 百分比与 reset time，不暴露原始账号 ID 或凭证。Quota 状态保存在 Git 忽略的 `runtime/` 目录中，并使用 ChatGPT account ID 的哈希作为 key，因此 daemon 重启后保护状态仍然有效，同时不会落盘原始 account ID。

## 安全与本地状态

运行时配置、日志、生成的二进制、认证状态都不会提交到 Git。

Wopal AI API key 存储在 macOS Keychain，由 daemon 运行时读取，不会写入生成的 CLIProxyAPI runtime configuration。

Gateway 默认只监听本机地址，并设计为 macOS 当前用户的本地服务。

## 开发

环境要求：

- Go
- Node.js 22+
- pnpm

验证命令：

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

Go daemon 入口位于 `cmd/gatewayd`。
