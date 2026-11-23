package main

import (
	"context"
	"fmt"
	"os"
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
// 浏览器等扩展能力通过 MCP 接入，当前暂不接，留空则只用内置工具。
var mcpServers = []tools.MCPServer{}

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
	finalModel, err := p.Run()
	if err != nil {
		fmt.Println("程序错误:", err)
		return
	}
	// 退出的最后一件事：把本次对话并进长期记忆
	if fm, ok := finalModel.(Model); ok {
		fm.saveMemoryOnExit()
	}
}
