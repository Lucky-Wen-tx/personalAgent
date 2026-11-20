package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpToolClient 网络请求共用，带 30 秒超时
var httpToolClient = &http.Client{Timeout: 30 * time.Second}

// NewHTTPTool 构造通用 HTTP 请求工具。
func NewHTTPTool() *ToolDef {
	return &ToolDef{
		Name:        "http_request",
		Description: "发起 HTTP 请求并返回响应状态码与响应内容。支持自定义请求方法、请求头和请求体，可用于访问网页或 API。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "请求的 URL，必须以 http:// 或 https:// 开头",
				},
				"method": map[string]any{
					"type":        "string",
					"description": "请求方法，如 GET、POST、PUT、DELETE，缺省为 GET",
				},
				"headers": map[string]any{
					"type":                 "object",
					"description":          "请求头，键值对形式",
					"additionalProperties": map[string]any{"type": "string"},
				},
				"body": map[string]any{
					"type":        "string",
					"description": "请求体内容，GET 请求通常留空",
				},
			},
			"required": []string{"url"},
		},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				URL     string            `json:"url"`
				Method  string            `json:"method"`
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.URL == "" {
				return "", fmt.Errorf("缺少必填参数 url")
			}
			if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
				return "", fmt.Errorf("url 必须以 http:// 或 https:// 开头")
			}
			method := p.Method
			if method == "" {
				method = http.MethodGet
			}
			var bodyReader io.Reader
			if p.Body != "" {
				bodyReader = strings.NewReader(p.Body)
			}
			req, err := http.NewRequestWithContext(ctx, method, p.URL, bodyReader)
			if err != nil {
				return "", fmt.Errorf("创建请求失败: %w", err)
			}
			for k, v := range p.Headers {
				req.Header.Set(k, v)
			}
			resp, err := httpToolClient.Do(req)
			if err != nil {
				return "", fmt.Errorf("请求失败: %w", err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			if err != nil {
				return "", fmt.Errorf("读取响应失败: %w", err)
			}
			text := string(data)
			const maxLen = 8000
			if len(text) > maxLen {
				text = text[:maxLen] + "\n...（已截断）"
			}
			return fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, text), nil
		},
	}
}
