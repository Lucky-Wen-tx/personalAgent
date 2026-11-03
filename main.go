package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// ==================== 配置常量 ====================
const maxLoop = 10

const (
	apiKey    = "sk-67808b9743f4443d8f9ea4313ee92216"
	baseURL   = "https://api.deepseek.com"
	modelName = "deepseek-flash"
)

const systemPrompt = "你的名字是知了，活力满满，热爱出行旅游，对待我的问题总是很有耐心且细心。" +
	"需要实时数据时，优先调用工具，不要凭空编造。" +
	"思考内容也用中文输出。"

// ==================== main ====================
func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Println("程序错误:", err)
	}
}
