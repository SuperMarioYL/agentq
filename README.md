[English](README.en.md) | **简体中文**

<picture>
  <source media="(max-width: 640px) and (prefers-color-scheme: dark)" srcset="assets/presentation/hero-mobile-dark.svg">
  <source media="(max-width: 640px)" srcset="assets/presentation/hero-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="assets/presentation/hero-dark.svg">
  <img src="assets/presentation/hero-light.svg" width="960" alt="agentq — Bring scattered approvals into one queue.">
</picture>

**agentq 将多个编码 Agent 的审批请求汇入本地网页队列，把你的选择回传给等待中的会话。**

`Go 1.24+ · Python 3 demo` · [Apache-2.0](LICENSE) · [GitHub](https://github.com/SuperMarioYL/agentq) · [网站](https://agentq.lei6393.com)

## 为什么需要它

同时运行多个会话时，审批请求会散落在不同终端。agentq 用带会话标识的 ApprovalEnvelope 把问题汇到同一处，保留选项与上下文，再把答复交回原请求方。你仍然决定每条请求是否应当通过。

<picture>
  <source media="(max-width: 640px) and (prefers-color-scheme: dark)" srcset="assets/presentation/process-mobile-dark.svg">
  <source media="(max-width: 640px)" srcset="assets/presentation/process-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="assets/presentation/process-dark.svg">
  <img src="assets/presentation/process-light.svg" width="960" alt="A local approval round trip">
</picture>

## 架构

wrap 识别子进程的提示并生成 ApprovalEnvelope。使用 --daemon 时，它通过本地 HTTP 转发请求；serve 将信封与答复保存到 bbolt，通过 REST 和 WebSocket 提供队列，内嵌网页用于处理请求。attach 生成访问地址和二维码。默认 wrap 仍可使用 stdin/stdout 协议。

<picture>
  <source media="(max-width: 640px) and (prefers-color-scheme: dark)" srcset="assets/presentation/architecture-mobile-dark.svg">
  <source media="(max-width: 640px)" srcset="assets/presentation/architecture-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="assets/presentation/architecture-dark.svg">
  <img src="assets/presentation/architecture-light.svg" width="960" alt="One queue, explicit reply routes">
</picture>

## 安装

需要 Go 1.24+；下方自带协议示例还需要 Python 3。构建时可能下载 Go 依赖，示例只访问回环地址。

```bash
git clone https://github.com/SuperMarioYL/agentq.git
cd agentq
go build -o agentq ./cmd/agentq
```

## 快速开始

```bash
python3 examples/presentation_demo.py
```

脚本在临时目录构建并启动真实 daemon，提交一个构造审批请求，再通过 HTTP 选择 n。输出显示队列从 0 变成包含 demo-approval，答复返回原请求，随后队列回到 0。它不启动编码 Agent，不执行审批文字中的动作。

## 用法

```bash
# 自动启动或复用本地 daemon，并转发审批
./agentq wrap --daemon -- claude

# 显式启动手机可访问的本地网络服务
./agentq serve --lan --token-out ./agentq-token.txt
./agentq attach --token-file ./agentq-token.txt

# 选择 Cursor/Aider 风格的提示匹配器
./agentq wrap --daemon --agent cursor -- cursor-agent
```

第三方命令需自行安装。手机与主机应处于可连通的网络；默认 serve 绑定 127.0.0.1，只有二维码不能把回环服务变成 LAN 服务。

## 能力与集成

| 路由 | 行为 |
|---|---|
| POST /api/envelopes | 提交请求并等待答复或过期 |
| GET /api/queue | 读取当前未答复队列 |
| POST /api/queue/:id/answer | 提交 choice_key |
| /ws | 订阅队列和答复事件 |
| GET /schema/approval-envelope.json | 获取公开协议 schema |
| GET /healthz | 存活检查 |

/api 与 /ws 接受 bearer token 或 ?t=token。协议可以由自定义运行时直接调用，不要求使用 stdout 提示识别器。

<picture>
  <source media="(max-width: 640px) and (prefers-color-scheme: dark)" srcset="assets/presentation/integrations-mobile-dark.svg">
  <source media="(max-width: 640px)" srcset="assets/presentation/integrations-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="assets/presentation/integrations-dark.svg">
  <img src="assets/presentation/integrations-light.svg" width="960" alt="Prompt and protocol integration">
</picture>

## 配置与边界

| serve 参数 | 默认/用途 |
|---|---|
| --listen | 127.0.0.1:7777 |
| --lan | 显式改为监听所有接口 |
| --data-dir | XDG_DATA_HOME/agentq 或 ~/.agentq |
| --token | 不指定时生成 |
| --token-out | 将当前 token 写入文件 |
| --auto-approve | 可重复的 glob:choice 规则 |
| --auto-approve-file | 每行一条规则 |

自动批准默认关闭。规则按顺序匹配完整提示，* 可以跨越 /；第一条匹配且 choice 存在于当前选项时生效，否则进入人工队列。它是文本匹配，不是命令安全分析。wrap 的 --expiry 控制请求有效期；提示识别依赖相应输出格式，未知交互并不保证被捕获。

## 运行记录

v0.11.0 的真实本地 HTTP 往返。输出由示例从实际响应提取稳定字段，不包含动态时间戳；这不是手机、LAN 或第三方 Agent 兼容性验收。

[输入、命令和完整输出](docs/demo-results.json)

[保留的历史终端录屏](assets/demo.gif) · [录制脚本](docs/demo.tape)。本轮示例以以上可重放记录为准。

## 路线图

- [x] stdio 信封/答复协议和提示匹配器。
- [x] daemon、bbolt、REST、WebSocket 和网页队列。
- [x] wrap --daemon、二维码和显式 LAN 监听。
- [x] 可选自动批准规则。
- [ ] 更完整的团队协作与审计体验。

已发布修复记录见 CHANGELOG.md；第三方 Agent 的具体版本仍需实际接入验证。

## 开发与许可证

```bash
go test ./...
go build ./cmd/agentq
```

协议定义见 [approval.go](internal/protocol/approval.go) 和 [JSON Schema](docs/approval-envelope.schema.json)。

[Apache-2.0](LICENSE) · [Issues](https://github.com/SuperMarioYL/agentq/issues)
