# Model Catalog and Capability Sources

> **Status**: Active
> **Updated**: 2026-10-24
> **Parent**: [项目设计](./DESIGN.md)

## Goal

用户只配置一个 provider，模型同步后尽量自动得到准确能力。
用户只维护例外项，Codex 所需的复杂目录由网关生成。
工具模式可统一选择，升级和模型切换问题能从日志定位。

## Configuration Ownership

| 文件 | 谁维护 | 内容 |
| --- | --- | --- |
| `runtime/config.json` | 用户 / CLI | provider 地址、工具模式、模型名、用户能力覆盖、排除名单 |
| `runtime/model-metadata.json` | 同步命令 | provider 返回的能力快照、来源、采集时间 |
| Codex 的 `wopal-codex-gateway-models.json` | 目录生成器 | Codex 接口需要的完整有效配置；可重新生成 |

`_links/agents/codex/` 指向 Codex 配置位置，不另存一套用户配置。
API key 继续由 Keychain 持有。能力快照不包含凭证或请求正文。

## User Configuration

```json
{
  "version": 1,
  "wopalAi": {
    "baseUrl": "https://example.com/v1",
    "toolMode": "code_mode_only",
    "models": [
      { "name": "model-a" },
      { "name": "model-b", "contextWindow": 131072, "vision": false }
    ]
  }
}
```

省略能力表示由能力来源解析；显式 `false` 是用户覆盖，必须保留。
同一 provider 的所有模型共用工具模式，默认 `code_mode_only`。
允许 `code_mode`、`direct`，非法值直接报错。
`direct` 缺少 `exec`；切换到它时，已有代码编排历史的对话需要新建对话。
官方模型保留自己的模式，因此全 provider 配置仍不等于任意切换都兼容。

## Synchronization and Resolution

能力按字段解析：**用户覆盖 > provider 明确返回的能力 > 网关默认值**。
网关默认值统一为 **1,000,000 tokens 上下文、支持图片、支持推理**。
这些值是用户选定的默认策略；检查输出将来源标为 `default`，与 provider 提供的规格区分。
用户覆盖与检测快照分开存储，同步不能把默认值写成逐模型的用户覆盖。

- 同步读取可用模型和接口明确提供的能力，复用现有过滤、排除与去重规则。
- provider 提供什么才采用什么；模型名字、`owned_by`、创建时间不能证明上下文或图片能力。
- 不根据 `free`、`flash` 等后缀猜测能力或自动去后缀匹配。
- provider 缺失的字段直接使用网关默认值，每个字段保留其来源。
- 默认值集中定义，新增模型只保存模型名；用户通过 `models set` 修正个别模型。
- provider 查询失败时保留原配置和最后成功快照，报告失败。
- 已保存的手工值一律保留；无法区分过去自动填入和用户填写的值时，不自动清空。
- `models set` 保留批量更新，增加清除指定覆盖、恢复自动解析的操作。

若 provider 只返回模型名，自动得到全部能力在信息上不可实现。
此时使用用户选定的默认值，个别差异由用户覆盖。
第三方能力目录、模型名模糊匹配、逐模型探测调用均在本方案范围之外。

## Inspection

`models inspect` 显示每个字段的有效值和来源，例如：

```text
model-a  context=1000000 [default]  vision=true  [default]
model-b  context=131072 [user]                vision=false [user]
```

同步结果报告新增、删除、保留覆盖和使用默认能力的模型数量。
刷新结果提示目录位置和使用方式，避免用户继续编辑生成文件。
Codex 的全局 `model_context_window` 和 `model_auto_compact_token_limit` 可覆盖目录值。
检查输出应提示这类冲突，由用户决定是否恢复逐模型配置；命令保留用户全局设置。

## Persistence and Restoration

模型选择器切回官方模型只改变当前模型选择，用户的 provider 和逐模型覆盖继续保存在
`runtime/config.json`。目录刷新可据此重新生成外部模型条目。

`codex restore` 用 `runtime/codex-restore.json` 中的安装前值恢复网关接管项：
目录路径、官方接口地址、两个 Memory 模型设置。当前模型为 `wopal-ai/*` 时也恢复
安装前的模型选择，当前为官方模型时保留该选择。其余 Codex 设置保留当前内容。
恢复点只保存这些字段，不是整份 Codex 配置或官方模型目录的备份。
恢复会移除生成目录，provider 配置、逐模型覆盖和 Keychain 密钥继续保留。
用户对生成目录的直接编辑由目录刷新覆盖；持久调整应通过 CLI 写入用户配置。

## Generated Catalog

Codex 自定义目录提供完整模型条目，普通 provider 的模型名列表不能直接替代它。
生成器只输出必需字段和有意覆盖默认行为的字段。
当前 Codex 没有目录级共享模板或文件引用，跨模型的有效基础指令仍须展开。

- 外部条目省略等价默认值、空值和 Codex 未识别的字段。
- 官方指令中完全相同的 `base_instructions` 与 `model_messages.instructions_template` 只保留后一份。
- 每个条目至少保留一份有效指令；外部模型使用简短、共享的编码助手指令。
- 官方模型的其他有效指令、可见性和行为保持不变。
- 保留技能、插件使用说明、工具模式、上下文、模态等确实影响行为的值。
- 不为追求文件体积而删除模型、压缩指令含义或复制官方模型能力给外部模型。

## Compatibility Verification

精简前后分别交由当前安装的 `codex debug models` 读取，对比归一化后的模型条目。
除明确的模式或能力调整外，有效值须一致。保存有效 Codex 版本作为证据。
覆盖指令缺失、默认值等价、用户覆盖保留、默认能力来源、同步失败和非法模式。
测试通过与当前线上生效分别记录，发布仍遵守启动前告知要求。

## Reference Documents

- [同步逻辑](../internal/catalog/wopal_models.go)
- [目录生成器](../internal/catalog/catalog.go)
