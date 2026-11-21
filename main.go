package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"Agent/tools"

	tea "github.com/charmbracelet/bubbletea"
)

// ==================== 配置常量 ====================
const maxLoop = 50

const (
	baseURL   = "https://api.beichenapi.com/v1"
	modelName = "deepseek-v4.1-flash"
)

// apiKey 从环境变量读取，不要写进源码
var apiKey = os.Getenv("DEEPSEEK_API_KEY")

const systemPrompt = "你精通前端网页设计，会先判断需要什么素材，自行到网络上搜集" +
	"有内置工具、skill提供给你使用，只能在工作区内行动，工作区是workspace" +
	"思考内容用中文输出。"

// ==================== 工作目录 ====================
// fileRoot 是文件操作的根目录（项目工作目录）；shellWorkspace 是 shell 命令执行的工作区目录。
const (
	fileRoot       = "workspace"
	shellWorkspace = "workspace"
)

// ==================== MCP 配置 ====================
// 接一个浏览器 MCP，给智能体「看图」的能力：打开自己写的页面、截图、看渲染结果。
// 默认用系统自带的 Edge（--browser msedge），省掉下载 Chromium；不需要时设 AGENT_NO_BROWSER=1。
var mcpServers = defaultMCPServers()

func defaultMCPServers() []tools.MCPServer {
	if os.Getenv("AGENT_NO_BROWSER") != "" {
		return nil
	}
	args := []string{
		"-y", "@playwright/mcp@latest",
		"--headless",                 // 不弹窗口
		"--isolated",                 // profile 只放内存，不碰本地浏览器数据
		"--image-responses", "allow", // 截图以图片内容返回，模型才看得到
		"--console-level", "warning", // 顺手带回控制台告警
	}
	if runtime.GOOS == "windows" {
		args = append(args, "--browser", "msedge") // 用系统 Edge，免下载
	}
	return []tools.MCPServer{{Name: "browser", Command: "npx", Args: args}}
}

// ==================== main ====================
func main() {
	if apiKey == "" {
		fmt.Println("请先设置环境变量 DEEPSEEK_API_KEY")
		return
	}

	// 连接 MCP server，失败时降级为仅内置工具
	if len(mcpServers) > 0 {
		fmt.Println("正在连接 MCP server（首次启动需要下载 Playwright MCP，请稍候）…")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	mcpTools, closeMCP, err := tools.LoadMCPTools(ctx, mcpServers)
	cancel()
	if err != nil {
		fmt.Println("MCP 连接失败，仅使用内置工具:", err)
	}
	if closeMCP != nil {
		defer func() { _ = closeMCP() }()
	}

	// 加载技能包（skills/ 目录下每个子目录一个 SKILL.md），失败时降级为无技能
	skills, err := LoadSkills("skills")
	if err != nil {
		fmt.Println("技能加载失败，仅使用内置能力:", err)
	}

	p := tea.NewProgram(initialModel(mcpTools, skills, fileRoot, shellWorkspace), tea.WithAltScreen(), tea.WithMouseCellMotion())
	// 退出时结束后台任务，避免 dev server 之类的子进程残留
	defer tools.CloseShellJobs()
	if _, err := p.Run(); err != nil {
		fmt.Println("程序错误:", err)
	}
}
