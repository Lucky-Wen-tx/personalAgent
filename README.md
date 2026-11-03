# personalAgent

一个用 Go 写的命令行旅游助手：Bubble Tea TUI + DeepSeek Responses API + 工具调用（ReAct 循环），支持接入 MCP server 扩展工具。

## 功能

- TUI 聊天界面：输入框、可滚动消息区、鼠标滚轮、思考中 spinner
- Agent 循环：模型自主调用工具，最多 10 轮
- 内置工具：当前时间、真实天气（Open-Meteo，免费无需 key）
- MCP 支持：stdio / streamable HTTP，启动时自动加载 MCP server 的工具

## 目录结构

```
.
├── main.go      # 配置、MCP 加载、程序入口
├── model.go     # Model 状态、样式、初始化
├── update.go    # 事件处理（按键、消息分发）
├── view.go      # 渲染（布局、viewport、状态栏）
├── agent.go     # Agent 循环：请求模型、工具调用、reasoning 回传
├── core/        # 会话历史
└── tools/       # 工具实现（时间 / 天气 / MCP 适配）
```

## 快速开始

1. 安装 Go 1.27+
2. 设置 API key（不要写进源码）：
   - bash：`export DEEPSEEK_API_KEY="你的key"`
   - PowerShell：`$env:DEEPSEEK_API_KEY="你的key"`
3. 运行：

```bash
go run .
```

> 注意：用 `go run .`（编译整个包），不要用 `go run main.go`（只编译单个文件）。

## 内置命令

- `Enter` 发送
- `/clear` 清空对话
- `/quit` 或 `Ctrl+C` 退出

## 接入 MCP

编辑 `main.go` 中的 `mcpServers`，例如接入本地文件系统：

```go
var mcpServers = []tools.MCPServer{
	{Name: "filesystem", Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "."}},
}
```

需要本机有 Node/npx。连接失败会自动降级为仅使用内置工具。
