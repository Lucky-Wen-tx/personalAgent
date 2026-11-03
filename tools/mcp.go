package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPServer 描述一个要连接的 MCP server。
// stdio 方式填 Command/Args/Env；远程方式填 URL（streamable HTTP）。
type MCPServer struct {
	Name    string   // 仅用于错误提示
	Command string   // stdio：启动命令，如 "npx"
	Args    []string // stdio：命令参数
	Env     []string // stdio：附加环境变量（拼在系统环境变量之后）
	URL     string   // 远程：streamable HTTP 地址
}

// LoadMCPTools 连接所有 MCP server，把暴露的 tools 转成 ToolDef。
// 返回的 close 函数用于退出时释放连接（stdio 会结束子进程）。
func LoadMCPTools(ctx context.Context, servers []MCPServer) ([]*ToolDef, func() error, error) {
	var (
		defs    []*ToolDef
		clients []*client.Client
	)
	closeAll := func() error {
		var errs []string
		for _, c := range clients {
			if err := c.Close(); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			return errors.New(strings.Join(errs, "; "))
		}
		return nil
	}

	for _, srv := range servers {
		c, err := connectMCP(ctx, srv)
		if err != nil {
			_ = closeAll()
			return nil, nil, err
		}
		clients = append(clients, c)

		res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
		if err != nil {
			_ = closeAll()
			return nil, nil, fmt.Errorf("MCP server「%s」列出工具失败: %w", srv.displayName(), err)
		}
		for _, mt := range res.Tools {
			defs = append(defs, mcpToolDef(c, mt))
		}
	}
	return defs, closeAll, nil
}

func connectMCP(ctx context.Context, srv MCPServer) (*client.Client, error) {
	var (
		c   *client.Client
		err error
	)
	switch {
	case srv.Command != "":
		c, err = client.NewStdioMCPClient(srv.Command, srv.Env, srv.Args...)
	case srv.URL != "":
		c, err = client.NewStreamableHttpClient(srv.URL)
	default:
		return nil, errors.New("MCPServer 需要 Command 或 URL")
	}
	if err != nil {
		return nil, fmt.Errorf("MCP server「%s」连接失败: %w", srv.displayName(), err)
	}
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "learn-agent", Version: "0.1.0"},
	}}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("MCP server「%s」握手失败: %w", srv.displayName(), err)
	}
	return c, nil
}

func (s MCPServer) displayName() string {
	if s.Name != "" {
		return s.Name
	}
	if s.Command != "" {
		return s.Command
	}
	return s.URL
}

// mcpToolDef 把 MCP tool 包装成 ToolDef，参数 schema 原样透传。
func mcpToolDef(c *client.Client, mt mcp.Tool) *ToolDef {
	schema := map[string]any{}
	if len(mt.RawInputSchema) > 0 {
		_ = json.Unmarshal(mt.RawInputSchema, &schema)
	} else if b, err := json.Marshal(mt.InputSchema); err == nil {
		_ = json.Unmarshal(b, &schema)
	}
	name := mt.Name

	return &ToolDef{
		Name:        name,
		Description: mt.Description,
		Parameters:  schema,
		Handler: func(ctx context.Context, args string) (string, error) {
			arguments := map[string]any{}
			if strings.TrimSpace(args) != "" {
				if err := json.Unmarshal([]byte(args), &arguments); err != nil {
					return "", fmt.Errorf("参数解析失败: %w", err)
				}
			}
			res, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
				Name:      name,
				Arguments: arguments,
			}})
			if err != nil {
				return "", err
			}
			text := mcpContentText(res.Content)
			if res.IsError {
				if text == "" {
					text = "工具执行失败"
				}
				return "", errors.New(text)
			}
			return text, nil
		},
	}
}

// mcpContentText 拼接 content 块里的文本，非文本块（图片等）忽略。
func mcpContentText(contents []mcp.Content) string {
	var parts []string
	for _, c := range contents {
		if tc, ok := c.(mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}
