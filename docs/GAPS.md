# GAPS — Wopal Codex Gateway Design vs Implementation Divergence

> **Status**: Active
> **Updated**: 2026-10-29
> **Design Source**: `./DESIGN.md`（本文件对照的正式目标态设计）
> **Companion**: 追踪 Wopal Codex Gateway 的目标态与实现差距；全部关闭后删除本文件。

## Catalog Trust and Recovery

### WPX-G12: Codex 自动升级后 catalog 不能自愈 (P0)

**Current**: ChatGPT / Codex Desktop 自动升级后，native Codex artifact 的 identity 会变化。Gateway 会正确停止 bare official 路由，但用户必须先遇到请求失败，再手工执行 `catalog refresh` 才能恢复官方模型；同一时间的客户端重试还会重复得到 route error。

<!-- Evidence: `internal/catalog/native_artifact.go:153-202` returns an artifact-drift error; `cmd/gateway/runtime_snapshot.go:50-67` requires the baseline verifier to pass at snapshot construction; `cmd/gateway/cli_catalog.go:16-37` owns the manual refresh + reload path. -->

**Target**: 已有有效 official baseline 时，Gateway 把 native Codex identity 漂移当作可恢复运行事件。启动阶段或第一个 bare-name 请求发现漂移后自动执行与手工 refresh 相同的 H1 → bundled catalog → H2、collision 检查和原子发布；成功后当前请求重新解析一次路由，无需用户手工 refresh 或重启服务。自动恢复失败仍保持 fail-closed，且不改变 quota-only fallback 合同。

**Design**: `./DESIGN.md`（Provider Registry and Routing；Model Catalog > Catalog Self-Healing）。

**Exit**:
- [ ] 已有有效 baseline 且 native artifact identity 变化时，Gateway 能在启动或第一个 bare-name 请求上自动恢复，并让恢复后的 official slug 立即可路由
- [ ] daemon 内同一 drift 只触发一次自动 refresh；CLI 与 daemon 通过同一跨进程锁串行化 refresh，自动流程有有界超时和固定短退避，不形成 refresh 风暴
- [ ] 自动与手工 refresh 复用 H1/H2、external-name collision、candidate 构建与同一磁盘提交协议；baseline 作为 official routing commit point，写入错误可回滚，进程崩溃留下的 catalog-ahead-of-baseline 状态会被下一次 drift recovery 修复；runtime snapshot 只有构建成功才 swap
- [ ] 启动期 recovery 失败时 Gateway 以 degraded 模式启动：bare-name 请求返回 `503 catalog_refresh_failed`，含 `/` 的 external exact route 仍可服务；运行期失败保持同一语义
- [ ] 手工 `catalog refresh` 在 daemon 运行时等待 active runtime baseline 切换确认；reload 未收敛时命令返回失败而不是把“已写磁盘”当成完整成功
- [ ] baseline 缺失或损坏仍要求手工 `catalog refresh` 建立信任起点，不被自动恢复静默替代
- [ ] catalog recovery 不触发 `officialFallback`；官方自动 fallback 仍只由 quota protection 触发

## Observability

### WPX-G13: `/_status` 不能解释 catalog freshness 与自动恢复状态 (P1)

**Current**: `/_status` 能看到 provider、roles、quota 和 runtime reload，但看不到 official baseline 是否仍匹配当前 native Codex artifact，也不知道最近一次自动 catalog recovery 是否成功。出现官方路由异常时，用户仍需要直接查日志和文件才能判断是否是 Codex 升级导致。

<!-- Evidence: `cmd/gateway/runtime_status.go:91-159` builds status without catalog identity or recovery state. -->

**Target**: `/_status.catalog` 直接说明 catalog 是否 current、正在 refreshing，还是最近一次自动 refresh 失败，并展示最近实际验证过的 baseline/current SHA-256、检查时间和最近自动恢复结果。catalog recovery 与现有 runtime `reload` 各自报告自己的状态。

**Design**: `./DESIGN.md`（Status；Model Catalog > Catalog Self-Healing）。

**Exit**:
- [ ] `/_status.catalog.status` 能区分 `current`、`refreshing`、`refresh_failed`
- [ ] 状态包含 baseline 更新时间、baseline SHA-256、最近实际验证过的 current SHA-256 与检查时间，并且读取 `/_status` 本身不会每次重新哈希整个 binary
- [ ] 自动恢复发生后能看到最近一次结果、时间和脱敏错误；尚未发生自动恢复时该块可以省略
- [ ] catalog recovery 状态与现有 `reload` 状态分开表达，二者不会互相覆盖
- [ ] 状态输出继续不包含 API key、prompt、工具参数/输出或原始账号标识

## External Provider Logging

### WPX-G14: external response model 映射被误报为 warning (P1)

**Current**: 外部 provider 可以把请求模型映射到带版本号、canonical name 或内部模型名，并在响应里返回不同的 `response.model`。请求本身成功时，CLIProxyAPI 仍会把这种名称差异写成 `upstream served model ... for requested model ...` warning，长期运行会制造大量无行动价值的告警。

<!-- Evidence: Gateway captures embedded service stdout in `cmd/gateway/serve.go:250-324`; CLIProxyAPI emits the warning from `internal/runtime/executor/helps/usage_helpers.go` in the reference source. -->

**Target**: Gateway 把 external provider 的 served model 名称作为 usage metadata，而不是路由正确性或 provider 健康信号。请求模型与响应模型名称不同不会写 warning；真正的上游失败、鉴权、路由和协议错误仍按原级别记录。

**Design**: `./DESIGN.md`（Key Decisions；Client Compatibility Logging）。

**Exit**:
- [ ] external provider 成功返回与请求名不同的 `response.model` 时，`service.log` 不再出现 model-substitution warning
- [ ] served model 若被 usage accounting 使用仍可保留为 metadata，路由和请求模型归属不被响应名称改写
- [ ] 其他 CLIProxyAPI warning / error 不因这项过滤被整体降级或吞掉
- [ ] request-body logging 仍为 disabled，日志过滤不会引入请求正文或 credential 捕获

## Quota Protection

### WPX-G15: 账户级 quota 快照被误当成单请求步长 (P0)

**Current**: Gateway 会把同一账户 60 秒内两次 quota 百分比快照的正向差值当成“单步消耗”，并把观测到的最大值持久化用于提前保护。在多个子代理、会话或请求并发时，这个差值可能是多条请求的账户总消耗，因此一次高并发任务可以把后续窗口的 5 小时保护线长期从 97% 提前到 94%。

<!-- Evidence: `internal/quota/guard.go:769-805` learns from adjacent account-level samples; `internal/quota/guard.go:837-861` exposes learned/effective fields; `internal/quota/learning.go` persists the learned values. -->

**Target**: 额度保护只依赖上游直接提供的当前账户使用百分比和固定停止点：5 小时 97%、每周 99%。账户级 telemetry 用于判断当前窗口、reset time、freshness 和是否达到停止点，不再反推单请求或单推理 step 的额度消耗。既有 version 1 配置里的 `quota.maxStepCap` 可以继续被读取和保留，但不影响运行时保护决定。

**Design**: `./DESIGN.md`（Key Decisions；Quota Protection；Compatibility Contract）。

**Exit**:
- [ ] 5 小时窗口在 96% 时仍保持 official 路由，达到 97% 才进入保护；每周窗口在 98% 时保持 official 路由，达到 99% 才进入保护
- [ ] 多个并发请求、会话或子代理造成的账户使用百分比快速上涨不会改变 97% / 99% 停止点，也不会影响后续额度窗口
- [ ] version 1 配置即使带有 `quota.maxStepCap` 仍可正常加载和保存，但该字段不改变保护行为
- [ ] 已有 quota state 中的 learned-step 数据不会改变保护决定；运行时不再需要人工执行 learning reset 才能恢复固定停止点
- [ ] `quota reset-learning` 不再出现在 CLI help，也不再作为可执行 quota 子命令
- [ ] `/_status` 不再报告 observed learned-step 值；兼容保留的 `effective_*_stop_percent` 始终与固定停止点一致
- [ ] 额度保护通知、`officialFallback` 路由和 reset 后自动 failback 的既有行为保持不变

## Reference Documents

| Document | Purpose |
| --- | --- |
| `research/opencode-multi-protocol-spike.md` | OpenCode 四协议接入与 CLIProxyAPI 配置差异的实测证据 |
