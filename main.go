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
const maxLoop = 10

const (
	baseURL   = "https://api.deepseek.com"
	modelName = "deepseek-flash"
)

// apiKey 从环境变量读取，不要写进源码
var apiKey = os.Getenv("DEEPSEEK_API_KEY")

const systemPrompt = "你的名字是知了，活力满满，热爱出行旅游，对待我的问题总是很有耐心且细心。" +
	"需要实时数据时，优先调用工具，不要凭空编造。" +
	"思考内容也用中文输出。"

// ==================== MCP 配置 ====================
// 要接入的 MCP server 列表，留空则只用内置工具。(需要本机有npx/node)
var mcpServers = []tools.MCPServer{
	{Name: "filesystem", Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "."}},
}

// ==================== main ====================
func main() {
	if apiKey == "" {
		fmt.Println("请先设置环境变量 DEEPSEEK_API_KEY")
		return
	}

	// 连接 MCP server，失败时降级为仅内置工具
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	mcpTools, closeMCP, err := tools.LoadMCPTools(ctx, mcpServers)
	cancel()
	if err != nil {
		fmt.Println("MCP 连接失败，仅使用内置工具:", err)
	}
	if closeMCP != nil {
		defer func() { _ = closeMCP() }()
	}

	p := tea.NewProgram(initialModel(mcpTools), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Println("程序错误:", err)
	}
}
