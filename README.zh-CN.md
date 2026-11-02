# Wopal Codex Gateway

[English](README.md) | 简体中文

Wopal Codex Gateway 是运行在 macOS 本机上的 Codex 模型路由网关。它让 ChatGPT / Codex Desktop 和 Codex CLI 在保留官方模型的同时，也能使用 Wopal AI 和其他已配置的外部 Provider 模型。

详细设计见 [`docs/DESIGN.md`](docs/DESIGN.md)。

## 快速开始

下面的命令都在项目根目录执行。

### 1. 构建

```sh
go build -o bin/wopal-codex-gateway ./cmd/gateway
```

### 2. 配置 Wopal AI 地址

```sh
./scripts/gateway config wopal --base-url https://your-api.example.com/v1
```

### 3. 保存 API Key

```sh
./scripts/gateway auth set
```

API Key 保存到 macOS Keychain，不写入 `runtime/config.json`。

### 4. 同步模型

```sh
./scripts/gateway models sync
```

### 5. 启用 Gateway

```sh
./scripts/gateway install
```

安装完成后，完全退出并重新启动 ChatGPT / Codex Desktop，让客户端重新读取模型目录和 Gateway 配置。

### 6. 检查状态

```sh
./scripts/gateway status
```

正常应该看到：

```text
installed=yes loaded=yes healthy=yes
```

## 日常使用

查看当前配置：

```sh
./scripts/gateway config show
```

查看服务状态：

```sh
./scripts/gateway status
```

查看运行状态和官方额度保护：

```sh
curl -fsS http://127.0.0.1:11435/_status
```

上游模型列表变化后重新同步：

```sh
./scripts/gateway models sync
```

修改模型上下文长度：

```sh
./scripts/gateway models set deepseek-v4.1-flash --context-window 1000000
```

查看某个模型最终采用的配置：

```sh
./scripts/gateway models inspect deepseek-v4.1-flash
```

## 手工修改 `runtime/config.json` 后怎么生效

推荐优先使用上面的 CLI 命令修改配置。

如果直接修改了模型、模型能力或 `toolMode`，执行：

```sh
./scripts/gateway catalog refresh
```

然后完全退出并重新启动 ChatGPT / Codex Desktop。

如果修改了其他运行配置，最简单可靠的做法是重启 Gateway：

```sh
./scripts/gateway restart
```

修改监听地址或端口时必须重启服务。

## 服务管理

```sh
./scripts/gateway start
./scripts/gateway stop
./scripts/gateway restart
./scripts/gateway status
```

`stop` 只停止 Gateway，不恢复 Codex 配置。

## 停用 Gateway

正常停用：

```sh
./scripts/gateway disable
```

它会恢复 Gateway 安装前保存的 Codex 配置并卸载 Gateway 服务。完成后，完全退出并重新启动 ChatGPT / Codex Desktop。

## 常见问题

### 模型没有显示

```sh
./scripts/gateway auth test
./scripts/gateway models sync
```

然后完全退出并重新启动 ChatGPT / Codex Desktop。

### 改了模型配置但当前会话还是旧的

```sh
./scripts/gateway catalog refresh
```

然后重新启动客户端。已经加载过旧模型 metadata 的老会话，建议新开或分叉会话。

### 服务起不来

```sh
./scripts/gateway status
tail -n 100 logs/service-error.log
```

### API Key 有问题

```sh
./scripts/gateway auth status
./scripts/gateway auth test
```

需要重新保存时：

```sh
./scripts/gateway auth set
```

## 更新与回滚

更新代码后重新构建并重新安装：

```sh
go build -o bin/wopal-codex-gateway ./cmd/gateway
./scripts/gateway install
```

如果新版本无法正常工作：

```sh
recovery/start-gateway.sh
```

更多命令：

```sh
./scripts/gateway help
```

架构、Provider、catalog、quota、compaction 和内部状态说明见 [`docs/DESIGN.md`](docs/DESIGN.md)。
