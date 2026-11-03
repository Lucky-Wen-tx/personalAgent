package main

import (
	"time"

	"Agent/core"
	"Agent/tools"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

// ==================== Model ====================
type Model struct {
	session     *core.Session
	client      openai.Client
	llmTools    map[string]*tools.ToolDef
	openaiTools []responses.ToolUnionParam

	textarea textarea.Model
	viewport viewport.Model
	spinner  spinner.Model

	messages []chatMessage
	state    agentState
	loop     int
	ready    bool

	width  int
	height int
}

// ==================== 样式 ====================
var (
	userStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	assistantStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	thinkStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	toolCallStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	toolResultSty  = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	systemStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Italic(true)
	statusStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	borderStyle    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("8"))
)

// ==================== 初始化 ====================
func initialModel(extraTools []*tools.ToolDef) Model {
	// --- textarea ---
	ta := textarea.New()
	ta.Placeholder = "输入消息，Enter 发送"
	ta.Focus()
	ta.CharLimit = 2000
	ta.SetHeight(1)
	ta.ShowLineNumbers = false

	// --- viewport ---
	vp := viewport.New(80, 20)

	// --- spinner（思考中动画）---
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))

	// --- session---
	session := core.NewSession()
	session.AddMessage(
		responses.ResponseInputItemParamOfMessage(
			systemPrompt,
			responses.EasyInputMessageRoleSystem,
		),
	)

	// --- 注册工具（内置 + MCP）---
	defs := []*tools.ToolDef{
		tools.NewGetCurrentTime(),
		tools.NewGetWeather(),
	}
	defs = append(defs, extraTools...)
	llmTools := make(map[string]*tools.ToolDef)
	for _, t := range defs {
		llmTools[t.Name] = t
	}
	// 转换为openai格式工具
	openaiTools := make([]responses.ToolUnionParam, 0, len(llmTools))
	for _, t := range llmTools {
		openaiTools = append(openaiTools, t.ToParam())
	}

	// --- client---
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithRequestTimeout(120*time.Second),
	)

	return Model{
		session:     session,
		client:      client,
		llmTools:    llmTools,
		openaiTools: openaiTools,
		textarea:    ta,
		viewport:    vp,
		spinner:     sp,
		messages:    []chatMessage{},
		state:       stateIdle,
	}
}

func (m Model) Init() tea.Cmd {
	return textarea.Blink
}
