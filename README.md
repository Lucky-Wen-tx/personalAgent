# personalAgent

一个用 Go 写的前端网页智能体：Bubble Tea TUI + DeepSeek Responses API + 工具调用（ReAct 循环），支持接入 MCP server 扩展工具。

## 功能

- TUI 聊天界面：输入框、可滚动消息区、鼠标滚轮、思考中 spinner
- Agent 循环：模型自主调用工具，最多 50 轮
- 内置工具：
  - 文件：`read_file` / `write_file` / `list_dir`
  - 增量改码：`read_lines`（带行号、offset/limit）/ `edit_file`（精确替换）/ `glob` / `grep`
  - 预览与自检：`preview_start` / `preview_stop`（本地静态服务器）/ `html_check`（doctype、viewport、alt、重复 id、死链等静态检查）
  - 其他：`read_image`（看图：参考图、设计稿、截图）、`run_shell`（超时可配、输出保头保尾、危险命令需 y/n 审批）、`job_output` / `job_kill` / `job_list`（后台长任务）、`http_request`、`load_skill`
- 技能包：`skills/` 下每个子目录一个 `SKILL.md`，由 `load_skill` 注入
- MCP 支持：stdio / streamable HTTP，单个 server 失败不影响其他 server

## 目录结构

```
.
├── main.go      # 配置、MCP 加载、程序入口
├── model.go     # Model 状态、样式、初始化
├── update.go    # 事件处理（按键、消息分发）
├── view.go      # 渲染（布局、viewport、状态栏）
├── agent.go     # Agent 循环：请求模型、工具调用、reasoning 回传
├── core/        # 会话历史
├── skills/      # 技能包（SKILL.md）
├── workspace/   # 智能体的工作目录：它写的网页都在这里
└── tools/       # 工具实现（文件 / 增量改码 / 预览自检 / shell / HTTP / MCP 适配）
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

## shell 工具

- 有 Git Bash 时用 `sh`，没有时自动降级到 PowerShell（中文输出强制 UTF-8），不再是「没装 Git 就完全不能用」。
- 超时默认 120 秒，可用 `timeout_ms` 调整（上限 10 分钟）；超时会杀进程树。
- 输出保留头尾，超过 8000 字符时把完整日志路径一起返回。
- 长任务用 `background: true`：先拿 `job-1` 这样的 id，再用 `job_output` 看输出、`job_kill` 结束。
- 子进程环境会剔除 `KEY` / `TOKEN` / `SECRET` / `PASSWORD` 等变量，并注入 `GIT_PAGER=cat`、`CI=1`、`NO_COLOR=1`，避免分页器把命令挂死。
- 危险命令按「命令动词」判断（删除、提权、下载执行、内联代码、`xargs rm`、`find -delete`、`git reset --hard`、写工作区外等）需 `y` 确认；`grep "rm"` 这类不再误报。

## 内置命令

- `Enter` 发送
- `/clear` 清空对话
- `/quit` 或 `Ctrl+C` 退出

## 看图

智能体要「看见」自己写的页面，靠两件事：

1. **内置 `read_image`**：读取工作区里的 png / jpeg / gif / webp，直接作为图片交给模型（参考图、设计稿都走它）。
2. **MCP 浏览器截图**：启动时自动连接 `@playwright/mcp`，提供 `browser_navigate` / `browser_take_screenshot` 等工具。
   默认无头、内存 profile、用系统自带 Edge（Windows 下免下载 Chromium）：

   ```
   npx -y @playwright/mcp@latest --headless --isolated --image-responses allow --browser msedge
   ```

图片走的是「一次性」链路：只在紧接着的那一轮请求里作为 `function_call_output` 的图片内容发出去，
会话历史里只留一行文字说明，避免 base64 每轮重复占用上下文。
如果模型或网关不支持图片输入，会自动去掉图片重试一次并在界面上提示，不会让整轮请求失败。

不需要浏览器时设 `AGENT_NO_BROWSER=1` 启动，即只保留内置工具。

## 接入 MCP

编辑 `main.go` 的 `defaultMCPServers()`，例如再加一个远程 MCP：

```go
return []tools.MCPServer{
	{Name: "browser", Command: "npx", Args: []string{"-y", "@playwright/mcp@latest", "--headless"}},
	{Name: "remote", URL: "https://example.com/mcp"},
}
```

需要本机有 Node/npx。单个 server 连接失败只会丢掉它自己的工具，不影响其他 server 和内置工具。
