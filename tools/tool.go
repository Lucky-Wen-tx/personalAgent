package tools

import (
	"context"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// Handler 是工具真正的执行逻辑，参数是模型生成的 JSON 字符串
type Handler func(ctx context.Context, args string) (string, error)

// ToolDef 描述一个可以被 LLM 调用的工具
type ToolDef struct {
	Name        string
	Description string
	// Parameters 必须符合 JSON Schema 格式
	Parameters map[string]any
	Handler    Handler
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

// Call 执行工具
func (t *ToolDef) Call(ctx context.Context, args string) (string, error) {
	if t.Handler == nil {
		return "", nil
	}
	return t.Handler(ctx, args)
}