package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ==================== 布局 ====================
func (m *Model) layout() {
	statusH := 1
	textareaH := m.textarea.Height() + 2
	viewportH := m.height - textareaH - statusH - 2
	if viewportH < 3 {
		viewportH = 3
	}
	m.viewport.Width = m.width - 4
	m.viewport.Height = viewportH
	m.textarea.SetWidth(m.width - 4)
	m.refreshViewport()
}

// 把 messages 渲染进 viewport
func (m *Model) refreshViewport() {
	var sb strings.Builder
	for _, msg := range m.messages {
		switch msg.Role {
		case "user":
			sb.WriteString(userStyle.Render("> "))
			sb.WriteString(msg.Content)
		case "assistant":
			sb.WriteString(msg.Content)
		case "think":
			sb.WriteString(thinkStyle.Render("💭 " + msg.Content))
		case "tool_call":
			sb.WriteString(toolCallStyle.Render("→ 调用 " + msg.Content))
		case "tool_result":
			sb.WriteString(toolResultSty.Render("← 返回 " + msg.Content))
		case "error":
			sb.WriteString(errorStyle.Render("✗ " + msg.Content))
		case "system":
			sb.WriteString(systemStyle.Render("⚙ " + msg.Content))
		}
		sb.WriteString("\n\n")
	}
	m.viewport.SetContent(sb.String())
	m.viewport.GotoBottom()
}

// ==================== View ====================
func (m Model) View() string {
	if !m.ready {
		return "初始化中..."
	}

	var status string
	switch m.state {
	case stateIdle:
		status = statusStyle.Render("● 就绪    [Enter 发送 · /quit 退出 · /clear 清空]")
	case stateThinking:
		status = statusStyle.Render(fmt.Sprintf("%s 思考中… 第 %d/%d 轮", m.spinner.View(), m.loop+1, maxLoop))
	}

	viewportBox := borderStyle.Width(m.width - 2).Render(m.viewport.View())
	textareaBox := borderStyle.Width(m.width - 2).Render(m.textarea.View())

	return lipgloss.JoinVertical(
		lipgloss.Left,
		viewportBox,
		textareaBox,
		status,
	)
}
