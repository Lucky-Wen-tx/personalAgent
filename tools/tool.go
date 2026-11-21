package tools

import (
	"context"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// Handler 是工具真正的执行逻辑，参数是模型生成的 JSON 字符串
type Handler func(ctx context.Context, args string) (string, error)

// Image 是工具返回给模型的图片。Base64 是不带 data URL 前缀的编码，
// 由调用方按 MIME 拼成 data:image/png;base64,... 再送给模型。
type Image struct {
	MIME   string
	Base64 string
}

// Result 是工具的结构化结果：文本 + 可选图片。
// 只返回文本的工具可以继续用 Handler；要返回图片的工具用 RichHandler。
type Result struct {
	Text   string
	Images []Image
}

// RichHandler 是能返回图片的工具实现。
type RichHandler func(ctx context.Context, args string) (Result, error)

// ToolDef 描述一个可以被 LLM 调用的工具
type ToolDef struct {
	Name        string
	Description string
	// Parameters 必须符合 JSON Schema 格式
	Parameters map[string]any
	Handler    Handler
	// RichHandler 非空时优先使用，用于需要回传图片的工具（如看图、截图）
	RichHandler RichHandler
	// PreCheck 可选的预检：返回是否需要人工审批及原因。为 nil 表示无需审批。
	PreCheck func(args string) (needApproval bool, reason string)
}

// ToParam 转换成 OpenAI Responses API 的工具参数
func (t *ToolDef) ToParam() responses.ToolUnionParam {
	return responses.ToolUnionParam{
		OfFunction: &responses.FunctionToolParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  t.Parameters,
		},
	}
}

// Call 执行工具。RichHandler 优先；否则把文本结果包装成 Result。
func (t *ToolDef) Call(ctx context.Context, args string) (Result, error) {
	if t.RichHandler != nil {
		return t.RichHandler(ctx, args)
	}
	if t.Handler == nil {
		return Result{}, nil
	}
	out, err := t.Handler(ctx, args)
	return Result{Text: out}, err
}
