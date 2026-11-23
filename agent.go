package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"Agent/tools"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// ==================== 状态机 ====================
type agentState int

const (
	stateIdle agentState = iota
	stateThinking
	stateAwaitingApproval
)

// ==================== 聊天消息 ====================
type chatMessage struct {
	Role    string // user / assistant / think / tool_call / tool_result / error / system
	Content string
}

// ==================== Bubble Tea Msg ====================
// 一轮 agent 循环执行完毕后返回的结果
type agentRoundMsg struct {
	display     []chatMessage
	hasToolCall bool
	pending     []pendingTool
}

// pendingTool 是一个需要人工审批才能执行的工具调用，审批通过前不执行、不写回输出。
type pendingTool struct {
	name   string
	callID string
	args   string
	reason string
}

// toolDoneMsg 是审批通过后工具执行完成的消息，携带结果供展示并触发下一轮循环。
type toolDoneMsg struct {
	display []chatMessage
}

// API 或工具执行出错
type agentErrMsg struct {
	err error
}

// ==================== 核心：执行一轮 Agent 循环 ====================
func (m Model) callAgentRound() tea.Cmd {
	// 捕获当前 Model 里的引用，避免闭包持有 Model 副本
	session := m.session
	client := m.client
	llmTools := m.llmTools
	openaiTools := m.openaiTools

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		resp, err := client.Responses.New(ctx, responses.ResponseNewParams{
			Model: modelName,
			Input: responses.ResponseNewParamsInputUnion{
				OfInputItemList: session.GetMessages(),
			},
			Tools: openaiTools,
		})
		if err != nil {
			return agentErrMsg{err: err}
		}

		var display []chatMessage
		hasToolCall := false

		// 先收集本轮所有 reasoning 文本，并判断是否发生了工具调用
		var reasoningTexts []string
		var pending []pendingTool
		for _, item := range resp.Output {
			if item.Type == "function_call" {
				hasToolCall = true
			}
			if item.Type != "reasoning" {
				continue
			}
			for _, c := range item.Content {
				if c.Text != "" {
					reasoningTexts = append(reasoningTexts, c.Text)
				}
			}
			for _, s := range item.Summary {
				if s.Text != "" {
					reasoningTexts = append(reasoningTexts, s.Text)
				}
			}
		}

		// DeepSeek 要求带 tools 的请求中，每个工具调用轮都必须回传 reasoning_text。
		// 模型本轮没输出思考文本时补一个占位符，否则下一轮请求会报 HTTP 400。
		if hasToolCall && len(reasoningTexts) == 0 {
			reasoningTexts = []string{" "}
		}

		// reasoning 必须写在 function_call 之前
		if len(reasoningTexts) > 0 {
			text := strings.Join(reasoningTexts, "\n")
			if strings.TrimSpace(text) != "" {
				display = append(display, chatMessage{Role: "think", Content: text})
			}

			contentBlocks := make([]responses.ResponseReasoningItemContentParam, 0, len(reasoningTexts))
			for _, t := range reasoningTexts {
				contentBlocks = append(contentBlocks, responses.ResponseReasoningItemContentParam{
					Type: "reasoning_text",
					Text: t,
				})
			}
			session.AddMessage(responses.ResponseInputItemUnionParam{
				OfReasoning: &responses.ResponseReasoningItemParam{
					Content: contentBlocks,
				},
			})
		}

		for _, item := range resp.Output {
			switch item.Type {

			// ---------- 最终回答 ----------
			case "message":
				for _, c := range item.Content {
					if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
						display = append(display, chatMessage{Role: "assistant", Content: c.Text})
					}
				}

			// ---------- 工具调用 ----------
			case "function_call":
				fc := item.AsFunctionCall()

				display = append(display, chatMessage{
					Role:    "tool_call",
					Content: fmt.Sprintf("%s(%s)", fc.Name, fc.Arguments),
				})

				// 4.1 写回 function_call
				session.AddMessage(responses.ResponseInputItemUnionParam{
					OfFunctionCall: &responses.ResponseFunctionToolCallParam{
						CallID:    fc.CallID,
						Name:      fc.Name,
						Arguments: fc.Arguments,
					},
				})

				// 4.2 预检：危险命令挂起等待人工审批，其余直接执行
				t, ok := llmTools[fc.Name]
				needApproval, reason := false, ""
				if ok && t.PreCheck != nil {
					needApproval, reason = t.PreCheck(fc.Arguments)
				}
				if needApproval {
					pending = append(pending, pendingTool{
						name:   fc.Name,
						callID: fc.CallID,
						args:   fc.Arguments,
						reason: reason,
					})
					continue
				}

				res := runTool(ctx, llmTools, fc.Name, fc.Arguments)
				display = append(display, chatMessage{
					Role:    "tool_result",
					Content: res,
				})

				// 4.3 写回 function_call_output
				session.AddMessage(responses.ResponseInputItemUnionParam{
					OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
						CallID: openai.String(fc.CallID),
						Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
							OfString: openai.String(res),
						},
					},
				})
			}
		}

		return agentRoundMsg{display: display, hasToolCall: hasToolCall, pending: pending}
	}
}

// ==================== 工具与错误 ====================
func runTool(
	ctx context.Context,
	llmTools map[string]*tools.ToolDef,
	name, args string,
) string {
	t, ok := llmTools[name]
	if !ok {
		return fmt.Sprintf(`{"error":"未知工具: %s"}`, name)
	}
	out, err := t.Call(ctx, args)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return out
}

// executePendingTool 审批通过后执行挂起的工具调用，写回 function_call_output，
// 返回 toolDoneMsg 供 Update 展示结果并触发下一轮循环。
func (m Model) executePendingTool(p pendingTool) tea.Cmd {
	session := m.session
	llmTools := m.llmTools
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		toolResult := runTool(ctx, llmTools, p.name, p.args)
		session.AddMessage(responses.ResponseInputItemUnionParam{
			OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
				CallID: openai.String(p.callID),
				Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
					OfString: openai.String(toolResult),
				},
			},
		})
		return toolDoneMsg{display: []chatMessage{{Role: "tool_result", Content: toolResult}}}
	}
}

func formatErr(err error) string {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("HTTP %d: %s", apiErr.StatusCode, apiErr.RawJSON())
	}
	return err.Error()
}
