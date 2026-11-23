package main

import (
	"sort"
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

	// skills 是已加载的技能包；loadedSkills 记录已注入会话的技能，避免重复注入
	skills       []Skill
	loadedSkills map[string]bool

	// followBottom 为 true 时，新消息到达自动滚到底部；
	// 用户向上滚动后置为 false，保留当前位置，滚回底部时恢复。
	followBottom bool

	// pendingApproval 当前等待用户确认的命令；pendingQueue 是同轮剩余待审批命令。
	pendingApproval *pendingTool
	pendingQueue    []pendingTool

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
func initialModel(extraTools []*tools.ToolDef, skills []Skill, fileRoot, shellWorkspace string) Model {
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

	// --- 注册工具（内置 + skill + MCP）---
	loadedSkills := make(map[string]bool)
	defs := []*tools.ToolDef{
		newLoadSkillTool(skills, session, loadedSkills),
	}
	defs = append(defs, tools.NewFileTools(fileRoot)...)
	defs = append(defs, tools.NewCodeTools(fileRoot)...)
	defs = append(defs, tools.NewPreviewTools(fileRoot)...)
	defs = append(defs, tools.NewHTTPTool())
	defs = append(defs, tools.NewShellTools(shellWorkspace)...)
	defs = append(defs, extraTools...)
	llmTools := make(map[string]*tools.ToolDef)
	for _, t := range defs {
		// 同名工具只保留先注册的：内置工具优先，避免 MCP 用同名工具顶替需要审批的 run_shell
		if _, ok := llmTools[t.Name]; ok {
			continue
		}
		llmTools[t.Name] = t
	}
	// 转换为openai格式工具（按名称排序，保证每次启动的工具顺序一致）
	names := make([]string, 0, len(llmTools))
	for n := range llmTools {
		names = append(names, n)
	}
	sort.Strings(names)
	openaiTools := make([]responses.ToolUnionParam, 0, len(llmTools))
	for _, n := range names {
		openaiTools = append(openaiTools, llmTools[n].ToParam())
	}

	// --- client---
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithRequestTimeout(120*time.Second),
	)

	return Model{
		session:      session,
		client:       client,
		llmTools:     llmTools,
		openaiTools:  openaiTools,
		textarea:     ta,
		viewport:     vp,
		spinner:      sp,
		messages:     []chatMessage{},
		state:        stateIdle,
		followBottom: true,
		skills:       skills,
		loadedSkills: loadedSkills,
	}
}

func (m Model) Init() tea.Cmd {
	return textarea.Blink
}
