package main

// 长期记忆：一份跨进程保留的要点清单。
// 启动时从文件读入；/memory 或退出时由模型把「旧记忆 + 本次对话」合并成新版本写回。
// 记忆不存进 session，每次请求前临时作为 system 消息拼上（见 withMemory）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// memoryPath 放在工作区内，已被 .gitignore 忽略，不会混进提交。
const memoryPath = "workspace/memory.md"

// transcriptLimit 送去做摘要的对话字符上限，避免整段历史把总结请求撑爆。
const transcriptLimit = 20000

const memorySystemPrompt = "你在维护一份跨会话的长期记忆。根据「已有记忆」和「本次对话记录」，输出合并后的新记忆。" +
	"只保留以后还用得上的内容：用户偏好、项目约定、已完成的关键决定、未完成的事项。" +
	"不要记录临时命令输出、文件全文、寒暄。用中文，最多 15 行，每行一条，以「- 」开头，不要标题和解释。没有可记的就输出「- （暂无）」。"

// memoryMsg 是后台生成记忆的结果。
type memoryMsg struct {
	content string
	err     error
}

func LoadMemory() string {
	data, err := os.ReadFile(memoryPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveMemory(text string) error {
	if err := os.MkdirAll(filepath.Dir(memoryPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(memoryPath, []byte(strings.TrimSpace(text)+"\n"), 0o644)
}

// transcriptOf 把对话压成纯文本。只留用户与助手的正文：
// 思考过程、工具调用与工具输出不进记忆，否则摘要会被文件内容淹没。
func transcriptOf(messages []chatMessage) string {
	var sb strings.Builder
	for _, msg := range messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		fmt.Fprintf(&sb, "%s: %s\n", msg.Role, msg.Content)
	}
	out := sb.String()
	if len(out) > transcriptLimit {
		out = out[len(out)-transcriptLimit:]
	}
	return out
}

// summarizeMemory 让模型把旧记忆和本次对话合并成新记忆。
func summarizeMemory(ctx context.Context, client openai.Client, existing, transcript string) (string, error) {
	var sb strings.Builder
	sb.WriteString("已有记忆：\n")
	if strings.TrimSpace(existing) == "" {
		sb.WriteString("（空）\n")
	} else {
		sb.WriteString(existing + "\n")
	}
	sb.WriteString("\n本次对话记录：\n")
	sb.WriteString(transcript)

	resp, err := client.Responses.New(ctx, responses.ResponseNewParams{
		Model: modelName,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: []responses.ResponseInputItemUnionParam{
				responses.ResponseInputItemParamOfMessage(memorySystemPrompt, responses.EasyInputMessageRoleSystem),
				responses.ResponseInputItemParamOfMessage(sb.String(), responses.EasyInputMessageRoleUser),
			},
		},
	})
	if err != nil {
		return "", err
	}

	var out strings.Builder
	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				out.WriteString(c.Text)
			}
		}
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", fmt.Errorf("模型没有返回记忆内容")
	}
	return text, nil
}

// summarizeMemoryCmd 是 /memory 命令的后台任务：生成并写盘，结果通过 memoryMsg 回到 Update。
func (m Model) summarizeMemoryCmd() tea.Cmd {
	transcript := transcriptOf(m.messages)
	existing := m.memory
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		content, err := summarizeMemory(ctx, client, existing, transcript)
		if err != nil {
			return memoryMsg{err: err}
		}
		if err := SaveMemory(content); err != nil {
			return memoryMsg{err: err}
		}
		return memoryMsg{content: content}
	}
}

// saveMemoryOnExit 在程序退出时把本次对话并进长期记忆。
func (m Model) saveMemoryOnExit() {
	if len(m.messages) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	content, err := summarizeMemory(ctx, m.client, m.memory, transcriptOf(m.messages))
	if err != nil {
		fmt.Println("记忆保存失败:", err)
		return
	}
	if err := SaveMemory(content); err != nil {
		fmt.Println("记忆保存失败:", err)
		return
	}
	fmt.Println("长期记忆已保存到 " + memoryPath)
}

// withMemory 把记忆作为额外的 system 消息插在原有系统提示之后。
// 记忆每轮重新拼上，不写进 session，所以 /clear 不会清掉它，也不会在历史里堆积。
func withMemory(items []responses.ResponseInputItemUnionParam, memory string) []responses.ResponseInputItemUnionParam {
	if strings.TrimSpace(memory) == "" {
		return items
	}
	note := responses.ResponseInputItemParamOfMessage(
		"以下是与当前用户长期相关的记忆（来自之前的会话）：\n"+memory,
		responses.EasyInputMessageRoleSystem,
	)
	out := make([]responses.ResponseInputItemUnionParam, 0, len(items)+1)
	if len(items) > 0 {
		out = append(out, items[0])
	}
	out = append(out, note)
	if len(items) > 1 {
		out = append(out, items[1:]...)
	}
	return out
}

// memorySummary 是 TUI 里展示记忆用的截断版本。
func memorySummary(memory string) string {
	lines := strings.Count(memory, "\n") + 1
	if len(memory) > 200 {
		memory = memory[:200] + "…"
	}
	return fmt.Sprintf("已保存 %d 条要点：\n%s", lines, memory)
}
