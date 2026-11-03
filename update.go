package main

import (
	"strings"

	"Agent/core"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
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
				m.session.AddMessage(
					responses.ResponseInputItemParamOfMessage(
						systemPrompt,
						responses.EasyInputMessageRoleSystem,
					),
				)
				m.textarea.Reset()
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
			m.refreshViewport()
			return m, tea.Batch(m.spinner.Tick, m.callAgentRound())
		}

	// ---------- 一轮 Agent 循环完成 ----------
	case agentRoundMsg:
		m.messages = append(m.messages, msg.display...)
		m.loop++

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

	// ---------- 鼠标滚轮：转发给 viewport 滚动 ----------
	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
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
