# GAPS — Wopal Codex Gateway

> **Status**: Active
> **Updated**: 2026-10-20
> **Design Source**: `./DESIGN.md` (the design this document measures against)
> **Companion**: Tracks divergence from the gateway target state; entries close as implementation catches up, and this file is removed when none remain.

## Command Surface

### WCG-G1: 安装入口没有验证服务真的启动 (P0)

**Current**: 直接运行 `service install` 时，命令打印“已启动”就结束。二进制缺失、端口冲突或 daemon 启动失败时，用户仍然会看到成功提示。

**Target**: `service install` 和 `codex install` 只在 `/_health` 通过后报告成功；失败时给出可读原因。

**Design**: `./DESIGN.md` (Interfaces and Contracts)

**Exit**:
- [ ] 二进制缺失时，`service install` 失败并说明缺少什么
- [ ] daemon 未能在限定时间内健康时，安装命令失败并给出日志位置
- [ ] 正常安装结束后，`/_health` 立即返回健康

### WCG-G2: 命令行入口分散在两个构建产物里 (P0)

**Current**: 用户需要先构建 Node CLI，再构建 Go daemon，并记住哪个命令走哪个入口。`scripts/gateway` 与直接调用 CLI 的行为不一致。

**Target**: 一个 Go 二进制提供全部子命令；构建、安装、服务和模型管理都从同一入口执行。

**Design**: `./DESIGN.md` (Interfaces and Contracts)

**Exit**:
- [ ] 只构建一次就能得到可用的完整 CLI
- [ ] `serve`、`codex install`、`service`、`models`、`auth` 都由同一个可执行文件提供
- [ ] 旧入口不再出现在用户文档中

### WCG-G3: 配置没有版本号，CLI 与 daemon 各自解析 (P0)

**Current**: 配置文件没有版本字段。CLI 与 daemon 对同一文件使用不同的结构，字段支持范围也不一致。

**Target**: `runtime/config.json` 带 `version: 1`，CLI 与 daemon 共用同一份解析和校验规则；不支持的版本在启动前失败。

**Design**: `./DESIGN.md` (Interfaces and Contracts)

**Exit**:
- [ ] 缺少 `version` 或版本不是 1 时，启动与配置命令都拒绝继续
- [ ] 未知版本不会导致静默降级
- [ ] CLI 与 daemon 对同一份配置得到相同结果

## Codex Integration

### WCG-G4: 恢复流程会覆盖用户后来选择的官方模型 (P1)

**Current**: 卸载或恢复时，Gateway 会把 `model` 恢复成安装前的值，即使用户之后已经手动选择了其他官方模型。

**Target**: 恢复只在当前 `model` 属于 Gateway 提供的 `wopal-ai/` 模型时回退；用户后来选择的官方模型保持不变。

**Design**: `./DESIGN.md` (Codex managed keys)

**Exit**:
- [ ] 安装后选择官方模型再恢复，官方选择保留
- [ ] 安装后停留在 Wopal 模型再恢复，回到安装前的模型
- [ ] 恢复流程不会留下 Gateway 专属 catalog 引用

### WCG-G5: 模型目录刷新依赖外部 Codex 命令的隐式行为 (P2)

**Current**: 模型目录刷新依赖 `codex debug models --bundled` 的输出形状；命令不存在或输出变化时，用户只看到笼统失败。

**Target**: 刷新失败时明确指出是 Codex 命令不可用还是输出不符合预期，并保留上一份可用目录。

**Design**: `./DESIGN.md` (Codex model catalog)

**Exit**:
- [ ] Codex 命令缺失时，用户看到明确原因
- [ ] 输出不是合法 catalog 时，已有目录不被破坏
- [ ] 成功刷新后，官方模型可见性保持不变

## Observability

### WCG-G6: 请求正文可能进入 logs 目录 (P0)

**Current**: CLIProxyAPI 的请求日志在关闭详细日志后，仍会把大请求体暂存到 `logs` 目录中的临时文件。用户无法从产品行为上确认 prompt 不会落盘。

**Target**: Gateway 使用 metadata-only request logger，任何请求正文都不写入磁盘；`logs` 目录只包含脱敏的运行记录。

**Design**: `./DESIGN.md` (Data and State Model)

**Exit**:
- [ ] 发起一次包含正文的推理后，`logs` 目录不出现 request-body 或 response-body 临时文件
- [ ] 上游报错时，诊断只包含状态码、错误码、模型、request id 和耗时
- [ ] `/_status` 能确认请求正文日志处于关闭状态

### WCG-G7: 状态页只显示基础阈值，看不到实际停止点 (P1)

**Current**: `/_status` 显示 97% 和 99%，但当观测到的单步更大时，实际会更早停止。用户看到 97% 会误以为提前停是故障。

**Target**: 状态页同时显示基础阈值、实际生效阈值和已观测最大单步。

**Design**: `./DESIGN.md` (Status)

**Exit**:
- [ ] 正常状态下，实际阈值等于基础阈值
- [ ] 观测到更大单步后，实际阈值下降且状态页可见
- [ ] 状态页不暴露账号标识或凭证

### WCG-G8: 学到的最大单步没有上限或重置手段 (P1)

**Current**: 一次异常大的单步会被永久记住，之后每次都在很低的百分比提前 failover，用户无法恢复默认行为。

**Target**: 最大单步有可配置上限，并提供一条显式命令清除学习值。

**Design**: `./DESIGN.md` (Data and State Model)

**Exit**:
- [ ] 异常大单步不会把 5 小时阈值拉到低于配置下限
- [ ] 清除学习值后，阈值回到基础值
- [ ] 清除动作记录在 metadata 日志中

### WCG-G9: 日志没有轮转上限 (P2)

**Current**: `gateway.log`、`service.log` 与服务错误日志持续追加，长期运行会无限增长。

**Target**: 日志按大小和份数轮转，总占用有上限。

**Design**: `./DESIGN.md` (Data and State Model)

**Exit**:
- [ ] 超过单个文件上限后产生新文件
- [ ] 保留份数达到上限后最旧文件被清理
- [ ] 轮转不影响 `/_status` 与健康检查

## Routing and Protocol

### WCG-G10: 文档宣传的 Ollama 路由在运行时不存在 (P2)

**Current**: CLI 帮助和模型目录仍出现 `ollama/<model>`，但 daemon 明确拒绝启用 Ollama，选择该模型的用户会遇到不可用。

**Target**: 产品只提供 official 与 `wopal-ai/*` 两类模型；Ollama 不出现在帮助、目录或配置示例中。

**Design**: `./DESIGN.md` (Capability Scope)

**Exit**:
- [ ] 帮助信息只列出 official 与 `wopal-ai/*`
- [ ] 模型目录不包含 Ollama 条目
- [ ] 未知命名空间得到明确拒绝

### WCG-G11: Go 代码集中在一个 main 包，模块边界不可见 (P2)

**Current**: 路由、额度、压缩、通知、配置都在同一个包里；维护者无法从目录结构看出谁拥有哪条规则。

**Target**: 配置、Codex 集成、路由、额度、压缩、通知、日志各自拥有独立包和测试入口。

**Design**: `./DESIGN.md` (Module Architecture)

**Exit**:
- [ ] 每个模块有独立包和对应测试
- [ ] `cmd/gateway` 只负责装配
- [ ] 包之间没有循环依赖

## Reference Documents

| Document | Purpose |
| --- | --- |
| `../README.zh-CN.md` | 中文使用说明 |
| `../README.md` | English user guide |
