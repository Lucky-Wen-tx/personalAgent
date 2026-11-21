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
	pendingImages := m.pendingImages

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		newParams := func(items []responses.ResponseInputItemUnionParam) responses.ResponseNewParams {
			return responses.ResponseNewParams{
				Model: modelName,
				Input: responses.ResponseNewParamsInputUnion{OfInputItemList: items},
				Tools: openaiTools,
			}
		}

		// 图片只在这一轮请求里附上：会话历史里始终是纯文本，
		// 否则 base64 会跟着每一轮请求反复发送，几轮就撑爆上下文。
		input, attached := withImages(session.GetMessages(), pendingImages)
		resp, err := client.Responses.New(ctx, newParams(input))
		degraded := ""
		if err != nil && attached {
			// 模型或网关不支持图片输入时，去掉图片重试一次，别让这一轮直接报废
			resp, err = client.Responses.New(ctx, newParams(session.GetMessages()))
			if err == nil {
				degraded = "当前模型/网关不接受图片输入，本轮已降级为纯文本；图片请改用 html_check 或直接看文件。"
			}
		}
		if err != nil {
			return agentErrMsg{err: err}
		}
		// 图片已随本轮发出（或确认无法发送），不再重复使用
		for id := range pendingImages {
			delete(pendingImages, id)
		}

		var display []chatMessage
		if degraded != "" {
			display = append(display, chatMessage{Role: "error", Content: degraded})
		}
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
					Content: displayText(res),
				})
				// 图片挂到 callID 上，下一轮请求里随 function_call_output 一起发出
				if len(res.Images) > 0 {
					pendingImages[fc.CallID] = res.Images
				}

				// 4.3 写回 function_call_output（历史里只放文本，图片不落库）
				session.AddMessage(functionCallOutput(fc.CallID, res))
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
) tools.Result {
	t, ok := llmTools[name]
	if !ok {
		return tools.Result{Text: fmt.Sprintf(`{"error":"未知工具: %s"}`, name)}
	}
	res, err := t.Call(ctx, args)
	if err != nil {
		return tools.Result{Text: fmt.Sprintf(`{"error":%q}`, err.Error())}
	}
	return res
}

// displayText 是 TUI 里显示的版本：图片只给一行摘要，绝不把 base64 打进终端。
func displayText(res tools.Result) string {
	if len(res.Images) == 0 {
		return res.Text
	}
	var sb strings.Builder
	sb.WriteString(res.Text)
	for _, img := range res.Images {
		fmt.Fprintf(&sb, "\n[图片 %s，约 %d KB]", img.MIME, len(img.Base64)*3/4/1024)
	}
	return sb.String()
}

// historyText 是写回会话的版本：告诉模型这一轮曾附带图片，但不再存图片本身。
func historyText(res tools.Result) string {
	if len(res.Images) == 0 {
		return res.Text
	}
	return res.Text + fmt.Sprintf("\n[工具返回了 %d 张图片，已随本轮请求附上]", len(res.Images))
}

func functionCallOutput(callID string, res tools.Result) responses.ResponseInputItemUnionParam {
	return responses.ResponseInputItemUnionParam{
		OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
			CallID: openai.String(callID),
			Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
				OfString: openai.String(historyText(res)),
			},
		},
	}
}

// withImages 把待发送的图片临时挂到对应的 function_call_output 上，
// 返回新的消息切片（不改动 session 里的历史）。
func withImages(
	items []responses.ResponseInputItemUnionParam,
	images map[string][]tools.Image,
) ([]responses.ResponseInputItemUnionParam, bool) {
	if len(images) == 0 {
		return items, false
	}
	out := make([]responses.ResponseInputItemUnionParam, len(items))
	copy(out, items)
	attached := false
	for i, item := range out {
		fco := item.OfFunctionCallOutput
		if fco == nil || !fco.CallID.Valid() {
			continue
		}
		imgs, ok := images[fco.CallID.Value]
		if !ok || len(imgs) == 0 {
			continue
		}
		parts := make(responses.ResponseFunctionCallOutputItemListParam, 0, len(imgs)+1)
		if text := fco.Output.OfString.Value; strings.TrimSpace(text) != "" {
			parts = append(parts, responses.ResponseFunctionCallOutputItemParamOfInputText(text))
		}
		for _, img := range imgs {
			parts = append(parts, responses.ResponseFunctionCallOutputItemUnionParam{
				OfInputImage: &responses.ResponseInputImageContentParam{
					ImageURL: openai.String("data:" + img.MIME + ";base64," + img.Base64),
					Detail:   responses.ResponseInputImageContentDetailAuto,
				},
			})
		}
		out[i].OfFunctionCallOutput = &responses.ResponseInputItemFunctionCallOutputParam{
			CallID: fco.CallID,
			Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
				OfResponseFunctionCallOutputItemArray: parts,
			},
		}
		attached = true
	}
	return out, attached
}

// executePendingTool 审批通过后执行挂起的工具调用，写回 function_call_output，
// 返回 toolDoneMsg 供 Update 展示结果并触发下一轮循环。
func (m Model) executePendingTool(p pendingTool) tea.Cmd {
	session := m.session
	llmTools := m.llmTools
	pendingImages := m.pendingImages
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		res := runTool(ctx, llmTools, p.name, p.args)
		if len(res.Images) > 0 {
			pendingImages[p.callID] = res.Images
		}
		session.AddMessage(functionCallOutput(p.callID, res))
		return toolDoneMsg{display: []chatMessage{{Role: "tool_result", Content: displayText(res)}}}
	}
}

func formatErr(err error) string {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("HTTP %d: %s", apiErr.StatusCode, apiErr.RawJSON())
	}
	return err.Error()
}
