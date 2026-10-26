# GAPS — Wopal Codex Gateway Design vs Implementation Divergence

> **Status**: Active
> **Updated**: 2026-10-26
> **Design Source**: `./DESIGN.md`（本文件对照的正式目标态设计）
> **Companion**: 追踪 Wopal Codex Gateway 的目标态与实现差距；全部关闭后删除本文件。

## Provider Configuration and Identity

### WPX-G1: 多 provider 配置与 Provider Registry 尚未落地 (P0)

**Current**: 用户现在只能配置 `wopal-ai`。还不能新增、修改或删除其他 provider，也不能为第三方模型设置 provider 归属、协议或 alias，因此 Gateway 还不能把多个外部 provider 作为同一套可管理资源使用。

<!-- Evidence: `internal/config/gateway_config.go:36-45,130-157,236-245`; `cmd/gateway/main.go:40-50`; `cmd/gateway/cli_config.go:13-23`; search confirmed `internal/provider/` is absent. -->

**Target**: 用户可以用 `providers set/remove` 管理多个外部 provider，并为模型配置稳定的 client-visible name。Gateway 用同一份 Provider Registry 向 CLI、模型目录和运行时路由提供一致的 provider / model 解析结果。

**Design**: `./DESIGN.md`（Interfaces and Contracts；Provider Registry and Routing）。

**Exit**:
- [ ] `providers set` 可以创建和更新 provider，并明确输出 `created` 或 `updated`
- [ ] `providers remove` 可以删除未被引用的 provider，`config show` 不再显示它
- [ ] 非法 provider id、重复 client-visible name 和非法协议会在写入前被拒绝
- [ ] 现有 `wopalAi` 配置和未知 version 1 字段在 provider 配置读写后保持不变

### WPX-G2: provider 级 Keychain 凭据管理尚未落地 (P0)

**Current**: `auth set/test/status` 只认识 Wopal AI 的固定凭据。用户即使配置了第三方 provider，也没有对应的命令把该 provider 的 API key 安全地保存、检查和验证。

<!-- Evidence: `internal/keychain/keychain.go:13-16,28-71`; `cmd/gateway/cli_auth.go:12-24,56-83`. -->

**Target**: 用户可以用同一组 `auth` 命令管理任意已配置 provider 的 Keychain 凭据；省略 `--provider` 时仍保持 Wopal AI 的原有行为。任何 provider key 都不会写入 JSON、运行时配置或日志。

**Design**: `./DESIGN.md`（Interfaces and Contracts；Credentials）。

**Exit**:
- [ ] `auth set/test/status --provider <id>` 能分别管理并报告指定 provider 的凭据状态
- [ ] 省略 `--provider` 时仍操作 `wopal-ai`，现有 Wopal 使用方式不变
- [ ] 两个 provider 使用彼此独立的 Keychain account，不会互相覆盖
- [ ] 配置文件、CLIProxyAPI runtime config 和日志中都不出现 provider API key

## Model Catalog and Protocols

### WPX-G3: provider 模型管理、models.dev 元数据与协议解析尚未落地 (P0)

**Current**: `models list/sync/add/set/remove/inspect` 目前只管理 Wopal AI。用户不能针对其他 provider 登记模型、设置 alias 或 protocol，也没有 models.dev 元数据来源来补齐协议和能力属性。

<!-- Evidence: `cmd/gateway/cli_models.go:14-39,46-63,84-165`; `cmd/gateway/model_patch.go:9-46`; `internal/catalog/model_metadata.go:18-22,31-73,106-171`; search found no `models.dev`, `provider.shape`, or `needs_protocol` carrier. -->

**Target**: 用户可以按 provider 查看、同步、添加和修改模型。discovery-enabled provider 从 OpenAI 风格 `/models` 获取 available set；native Gemini / Anthropic 通过 `models add` 明确登记。协议按显式设置、模型级 models.dev `provider.shape`、模型级 `provider.npm`、provider 级 `npm`、配置的 provider `protocol` 依次解析，无法判断时清楚标记 `needs_protocol`。

**Design**: `./DESIGN.md`（Model Catalog；Protocol Resolution；Attribute Precedence；Sync Rules）。

**Exit**:
- [ ] `models list/sync/add/set/remove/inspect --provider <id>` 对指定 provider 生效，省略参数仍保持 Wopal 行为
- [ ] discovery-enabled provider 只把已 configured models 变成可路由模型，`models sync` 不会偷偷注册新模型
- [ ] native Gemini / Anthropic 不调用 OpenAI 风格模型清单端点，显式 `models add` 后仍可登记
- [ ] protocol 解析遵守 `explicit → model provider.shape → model provider.npm → provider npm → provider protocol → needs_protocol`，`needs_protocol` 模型不进入 Codex catalog
- [ ] Wopal 元数据继续只使用 `runtime/model-metadata.json`，其他 provider 使用独立元数据 store
- [ ] models.dev 拉取失败或返回 304 时沿用上一份成功快照，不阻断其他 provider 同步

### WPX-G4: 四协议外部 provider 执行链尚未落地 (P0)

**Current**: Gateway 目前只把 Wopal AI 编译成一个 OpenAI-compatible 上游。第三方模型还不能按各自协议走 OpenAI Chat、OpenAI Responses、Anthropic Messages 或 Gemini，也没有 provider 级受限 header 注入。

<!-- Evidence: `internal/config/config.go:18-88`; repository search found no gateway carrier for `openai-responses`, `anthropic-messages`, or `gemini` protocol buckets. -->

**Target**: 一个 provider 可以同时拥有四种协议的模型。用户只选择模型，Gateway 根据该模型的 protocol 选择正确上游地址和 CLIProxyAPI executor；第三方 Responses 使用兼容模式，header 只允许安全的字面量或 `$CPA-SESSION-ID`。

**Design**: `./DESIGN.md`（Gateway Config；Provider Registry and Routing）。

**Exit**:
- [ ] 四种 protocol 的模型分别请求到设计规定的 chat、responses、messages、gemini endpoint
- [ ] 同一 provider 的不同 protocol 可以共用同一把 Keychain 凭据并使用各自 `baseUrls` 覆盖
- [ ] 外部 Responses 桶启用 `is-compat` 与 `disable-codex-cloaking`，第三方端点不会收到 Codex 专有形状
- [ ] `$CPA-SESSION-ID` 能注入指定非密钥 header，任意 `$<client-header>` 与凭据类 header 会被拒绝
- [ ] 客户端 `Authorization`、账号标识和 Cookie 不会被复制到第三方 provider

### WPX-G5: 多 provider catalog 与 official native baseline 尚未落地 (P0)

**Current**: `catalog refresh` 只合并官方 bundled models 和 Wopal models，并直接调用当前 `codex` 入口。它还不会生成 official slug baseline，也没有把 baseline 绑定到真正携带 bundled catalog 的 native Codex binary。

<!-- Evidence: `internal/catalog/catalog.go:55-77,121-185,227-271`; `internal/config/paths.go:127-140` has no `official-models.json` path. -->

**Target**: `catalog refresh` 把所有已配置外部模型合入 Codex catalog，同时把官方 slug 集合绑定到 authoritative native Codex binary 的 canonical path + SHA-256。刷新期间 native binary 发生变化或出现名称碰撞时，旧 catalog 和 baseline 保持可用，不提交混合状态。

**Design**: `./DESIGN.md`（Provider Registry and Routing；Model Catalog；Data and State Model）。

**Exit**:
- [ ] catalog 同时包含官方模型、Wopal 模型和所有可路由的已配置外部模型
- [ ] `catalog refresh` 用同一个 native Codex binary 完成 H1、bundled catalog 读取和 H2，只有 H1=H2 才提交结果
- [ ] `runtime/official-models.json` 保存官方 slug 与 native artifact identity，文件使用原子写入和 `0600` 权限
- [ ] external client-visible name 与 official slug 冲突时刷新失败，并保留刷新前的 catalog 与 baseline

## External Execution and Routing

### WPX-G6: exact Route Table、official baseline 校验与 SIGHUP 热重载尚未落地 (P0)

**Current**: 路由仍按模型名字前缀分类：`wopal-ai/` 走 Wopal，带 `/` 的其他名字直接拒绝，其他 bare name 一律当官方模型。运行中的 daemon 只在启动时读取配置，也不会响应 SIGHUP，因此 alias 删除、provider 修改或 baseline 更新不能原子地切换到新路由快照。

<!-- Evidence: `internal/routing/routing.go:21-45`; `internal/proxy/middleware.go:93-164`; `internal/proxy/websocket_routing.go:14-39`; `cmd/gateway/serve.go:49-65,82` only handles SIGINT/SIGTERM. -->

**Target**: 只有 exact Route Table 中已注册的 external name 才走外部 provider；其他 bare name 必须命中已验证 official baseline 才走官方链路，否则明确报 route error。CLI 修改配置或 catalog 后通过 SIGHUP 原子换入完整新快照，失败时继续使用旧快照并在状态中说明原因。

**Design**: `./DESIGN.md`（Provider Registry and Routing）。

**Exit**:
- [ ] 已注册 external name 精确命中对应 provider，未知 namespace 和未知 bare name 都不会隐式回落到 official
- [ ] official bare slug 只有在当前 native artifact 与 baseline 一致时才走官方链路；baseline 缺失或失配时 fail closed
- [ ] 删除或改名 bare external alias 后，即使旧会话继续发送旧名字，也得到 route error 而不是进入官方链路
- [ ] CLI 成功修改 provider/model/alias 或刷新 catalog 后，daemon 通过 SIGHUP 原子切换到新快照，进行中的请求不受影响
- [ ] reload 构建失败时旧快照继续服务，错误可以从 `/_status` 看到；手工编辑配置后通过 `service restart` 生效
- [ ] WebSocket 会话切换到不同 provider 时会安全重连并让 Codex 重放请求，而不是把新模型请求发到旧上游连接

## Model Roles and Codex State

### WPX-G7: `officialFallback` 与 `compaction` 两个动态 role 尚未落地 (P0)

**Current**: 官方额度回退模型、Gateway compaction 模型和 Codex memories 模型都写死为同一个 Wopal 模型。用户不能查看、设置或重置两个 role，也不能把这些用途指向其他 provider 的模型。

<!-- Evidence: `internal/routing/routing.go:16-18`; `internal/compaction/compaction.go:17-22`; `internal/codex/codex_config.go:26-34,280-285`; `cmd/gateway/cli_config.go:13-23` has no role command. -->

**Target**: 用户只管理 `officialFallback` 和 `compaction` 两个 role。role 保存 provider + upstream model，不受 alias 改名影响；额度保护使用 `officialFallback`，Gateway compaction 与 Codex memories 共用 `compaction`。

**Design**: `./DESIGN.md`（Interfaces and Contracts；Model Roles；Official Fallback）。

**Exit**:
- [ ] `config role` 能显示两个 role 的 provider、upstream model、client model、来源和 ready/unavailable/invalid 状态
- [ ] `config role set/reset` 能设置或恢复默认 role，并拒绝不存在的 provider/model
- [ ] `providers remove` 在任一 role 仍引用该 provider 时拒绝删除
- [ ] 官方额度进入保护区时只使用当前 `officialFallback`；429、5xx、连接失败、WebSocket 失败和普通鉴权错误不会触发自动回退
- [ ] Gateway compaction 与 Codex memories 都使用当前 `compaction` role，且 role 执行失败不会继续套用另一个 role

### WPX-G8: Codex restore 尚未记录所有曾暴露的 external name (P0)

**Current**: `codex restore` 只有在当前 model 以 `wopal-ai/` 开头时才恢复安装前模型。第三方 bare alias 或其他 external client-visible name 在后来被删除、改名后，restore 还不知道它曾由 Gateway 暴露。

<!-- Evidence: `internal/codex/codex_config.go:16-24,302-319`; current restore state contains no exposed-name set. -->

**Target**: Gateway 在接管期间累计记录每次成功 catalog refresh 暴露过的 external client-visible name。restore 遇到这些名字时回到安装前模型；用户后来选择的 official bare slug 保持不变。

**Design**: `./DESIGN.md`（Codex Integration and Restore）。

**Exit**:
- [ ] 每次成功 catalog refresh 都把当次 external client-visible names 合并进 restore state，旧名字不会因删除或改名丢失
- [ ] 当前 model 是任一累计 external name 时，`codex restore` 恢复安装前 model
- [ ] 当前 model 是 official bare slug 时，`codex restore` 保留用户当前选择
- [ ] alias 新增、删除和改名后执行 restore，都不会把已经失效的第三方名字留在当前 model

## Recovery and Observability

### WPX-G9: rollback 还没有恢复完整 Codex-facing 状态 (P1)

**Current**: 现有 recovery 流程主要负责停止当前 daemon 并把上一版 daemon 拉起来。它不会先让当前版本恢复 Codex 配置，也不会在上一版健康后用上一版重新安装自己的 catalog 与 memories model。

<!-- Evidence: `recovery/start-gateway.sh:43-79`; `README.zh-CN.md:611-637` documents stop → start old daemon and says Codex config need not change. -->

**Target**: 回滚按一个固定、可中止的顺序完成：当前版本先 `codex restore`，上一版 daemon 健康后再由上一版执行 `codex install`。任何一步失败都停止，不留下新旧版本混合的 Codex-facing 状态。

**Design**: `./DESIGN.md`（Rollback）。

**Exit**:
- [ ] 回滚流程按 `当前版 codex restore → 上一版健康 → 上一版 codex install` 的顺序执行
- [ ] 任一步失败都会停止后续动作并给出可诊断错误
- [ ] 成功回滚后 Codex catalog 只包含上一版能路由的模型，第三方 bare alias 不会残留在 catalog 或当前 model

### WPX-G10: `/_status` 还不能解释多 provider 与 reload 状态 (P1)

**Current**: `/_status` 目前主要报告监听地址、官方额度、通知、Wopal 状态和请求正文日志开关。用户看不到有哪些 provider、两个 role 当前解析到什么模型、provider 凭据是否已配置，也看不到最近一次 SIGHUP reload 是否失败。

<!-- Evidence: `cmd/gateway/serve.go:134-143`. -->

**Target**: `/_status` 能用脱敏信息解释当前运行快照：provider、两个 role、credential 是否配置、官方 quota/failover、服务状态和最近 reload 结果。任何 API key、prompt 或原始账号标识都不会出现在响应里。

**Design**: `./DESIGN.md`（Status；Provider Registry and Routing）。

**Exit**:
- [ ] `/_status` 列出当前 provider 及两个 role 的解析状态，并只用布尔/状态值表示 credential 是否配置
- [ ] `/_status` 保留官方 quota/failover、服务状态和 `request_body_logging=disabled`
- [ ] SIGHUP reload 失败后状态能显示失败原因，同时明确当前仍在使用上一份成功快照
- [ ] 状态响应不包含 API key、prompt、工具参数、工具输出或原始账号标识

## Reference Documents

| Document | Purpose |
| --- | --- |
| `research/opencode-multi-protocol-spike.md` | OpenCode 四协议接入与 CLIProxyAPI 配置差异的实测证据 |
