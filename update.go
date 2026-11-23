package main

import (
	"strings"

	"Agent/core"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// ==================== Update ====================
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	// ---------- 窗口尺寸变化 ----------
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		m.ready = true
		return m, nil

	// ---------- 按键 ----------
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "enter":
			// 审批等待中：enter 确认执行
			if m.state == stateAwaitingApproval && m.pendingApproval != nil {
				p := *m.pendingApproval
				m.pendingApproval = nil
				m.state = stateThinking
				return m, tea.Batch(m.spinner.Tick, m.executePendingTool(p))
			}
			// 思考中：不允许发送
			if m.state != stateIdle {
				return m, nil
			}
			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				return m, nil
			}

			// 内置命令
			switch input {
			case "/exit", "/quit":
				return m, tea.Quit
			case "/clear":
				m.messages = nil
				m.session = core.NewSession()
				// 会话重建后技能也需重新加载（clear 原地清空，保持与 load_skill 工具共享同一 map）
				clear(m.loadedSkills)
				m.session.AddMessage(
					responses.ResponseInputItemParamOfMessage(
						systemPrompt,
						responses.EasyInputMessageRoleSystem,
					),
				)
				m.textarea.Reset()
				m.refreshViewport()
				return m, nil
			case "/memory":
				// 生成长期记忆需要调模型，不能阻塞 TUI，转成后台任务
				m.textarea.Reset()
				m.messages = append(m.messages, chatMessage{Role: "system", Content: "正在整理长期记忆…"})
				m.refreshViewport()
				return m, m.summarizeMemoryCmd()
			}

			// /skill <名称>：手动加载技能
			if input == "/skill" || strings.HasPrefix(input, "/skill ") {
				name := strings.TrimSpace(strings.TrimPrefix(input, "/skill"))
				if name == "" {
					m.messages = append(m.messages, chatMessage{Role: "system", Content: "用法：/skill <技能名称>"})
					m.refreshViewport()
					return m, nil
				}
				msg, err := loadSkillIntoSession(m.skills, m.session, m.loadedSkills, name)
				if err != nil {
					m.messages = append(m.messages, chatMessage{Role: "error", Content: err.Error()})
				} else {
					m.messages = append(m.messages, chatMessage{Role: "system", Content: msg})
				}
				m.refreshViewport()
				return m, nil
			}

			// 正常发送
			m.textarea.Reset()
			m.messages = append(m.messages, chatMessage{Role: "user", Content: input})
			m.session.AddMessage(
				responses.ResponseInputItemParamOfMessage(
					input,
					responses.EasyInputMessageRoleUser,
				),
			)
			m.state = stateThinking
			m.loop = 0
			// 发送消息后回到底部并跟随新回复
			m.followBottom = true
			m.refreshViewport()
			return m, tea.Batch(m.spinner.Tick, m.callAgentRound())

		case "y":
			if m.state == stateAwaitingApproval && m.pendingApproval != nil {
				p := *m.pendingApproval
				m.pendingApproval = nil
				m.state = stateThinking
				return m, tea.Batch(m.spinner.Tick, m.executePendingTool(p))
			}

		case "n", "esc":
			if m.state == stateAwaitingApproval && m.pendingApproval != nil {
				p := *m.pendingApproval
				m.pendingApproval = nil
				rejectResult := `{"error":"用户拒绝执行该命令"}`
				m.session.AddMessage(responses.ResponseInputItemUnionParam{
					OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
						CallID: openai.String(p.callID),
						Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
							OfString: openai.String(rejectResult),
						},
					},
				})
				m.messages = append(m.messages, chatMessage{Role: "tool_result", Content: rejectResult})
				// 取队列下一个审批
				if len(m.pendingQueue) > 0 {
					m.pendingApproval = &m.pendingQueue[0]
					m.pendingQueue = m.pendingQueue[1:]
					m.state = stateAwaitingApproval
					m.refreshViewport()
					return m, nil
				}
				// 无剩余挂起，继续下一轮
				if m.loop < maxLoop {
					m.state = stateThinking
					m.refreshViewport()
					return m, tea.Batch(m.spinner.Tick, m.callAgentRound())
				}
				m.state = stateIdle
				m.messages = append(m.messages, chatMessage{Role: "error", Content: "达到最大循环次数，强制结束"})
				m.refreshViewport()
				return m, nil
			}
		}

	// ---------- 一轮 Agent 循环完成 ----------
	case agentRoundMsg:
		m.messages = append(m.messages, msg.display...)
		m.loop++

		// 有需要审批的命令：进入审批状态，暂停循环
		if len(msg.pending) > 0 {
			m.pendingApproval = &msg.pending[0]
			m.pendingQueue = msg.pending[1:]
			m.state = stateAwaitingApproval
			m.refreshViewport()
			return m, nil
		}

		if msg.hasToolCall && m.loop < maxLoop {
			m.refreshViewport()
			return m, m.callAgentRound()
		}

		if m.loop >= maxLoop {
			m.messages = append(m.messages, chatMessage{
				Role: "error", Content: "达到最大循环次数，强制结束",
			})
		}

		m.state = stateIdle
		m.refreshViewport()
		return m, nil

	// ---------- 出错 ----------
	case agentErrMsg:
		m.messages = append(m.messages, chatMessage{
			Role: "error", Content: formatErr(msg.err),
		})
		m.state = stateIdle
		m.refreshViewport()
		return m, nil

	// ---------- 审批通过后工具执行完成 ----------
	case toolDoneMsg:
		m.messages = append(m.messages, msg.display...)
		// 取队列下一个审批
		if len(m.pendingQueue) > 0 {
			m.pendingApproval = &m.pendingQueue[0]
			m.pendingQueue = m.pendingQueue[1:]
			m.state = stateAwaitingApproval
			m.refreshViewport()
			return m, nil
		}
		// 无剩余挂起，继续下一轮
		if m.loop < maxLoop {
			m.refreshViewport()
			return m, tea.Batch(m.spinner.Tick, m.callAgentRound())
		}
		m.state = stateIdle
		m.messages = append(m.messages, chatMessage{Role: "error", Content: "达到最大循环次数，强制结束"})
		m.refreshViewport()
		return m, nil

	// ---------- 长期记忆生成完成 ----------
	case memoryMsg:
		if msg.err != nil {
			m.messages = append(m.messages, chatMessage{Role: "error", Content: "记忆生成失败: " + msg.err.Error()})
		} else {
			m.memory = msg.content
			m.messages = append(m.messages, chatMessage{Role: "system", Content: memorySummary(msg.content)})
		}
		m.refreshViewport()
		return m, nil

	// ---------- 鼠标滚轮：转发给 viewport 滚动 ----------
	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			// 用户手动滚回底部时恢复自动跟随
			m.followBottom = m.viewport.AtBottom()
			return m, cmd
		}

	// ---------- spinner 帧更新 ----------
	case spinner.TickMsg:
		// 只在思考中继续转，空闲时停止 tick 链
		if m.state != stateThinking {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	// 其他消息交给 textarea
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}
