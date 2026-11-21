package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
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

	var errs []string
	for _, srv := range servers {
		c, err := connectMCP(ctx, srv)
		if err != nil {
			// 单个 server 失败不应该让其他 server 一起不可用
			errs = append(errs, err.Error())
			continue
		}
		res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
		if err != nil {
			_ = c.Close()
			errs = append(errs, fmt.Sprintf("MCP server「%s」列出工具失败: %v", srv.displayName(), err))
			continue
		}
		clients = append(clients, c)
		for _, mt := range res.Tools {
			defs = append(defs, mcpToolDef(c, mt))
		}
	}
	if len(errs) > 0 {
		return defs, closeAll, errors.New(strings.Join(errs, "; "))
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
		cmd, args := mcpCommand(srv.Command, srv.Args)
		c, err = client.NewStdioMCPClient(cmd, srv.Env, args...)
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

// mcpCommand 处理 Windows 上 npx 实际是 npx.cmd 的情况：CreateProcess 不能
// 直接执行 .cmd/.bat，必须经 cmd.exe 转一层，否则 stdio 启动直接失败。
func mcpCommand(name string, args []string) (string, []string) {
	if runtime.GOOS != "windows" {
		return name, args
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return name, args
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".cmd", ".bat":
		return "cmd.exe", append([]string{"/c", p}, args...)
	}
	return p, args
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
		RichHandler: func(ctx context.Context, args string) (Result, error) {
			arguments := map[string]any{}
			if strings.TrimSpace(args) != "" {
				if err := json.Unmarshal([]byte(args), &arguments); err != nil {
					return Result{}, fmt.Errorf("参数解析失败: %w", err)
				}
			}
			res, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
				Name:      name,
				Arguments: arguments,
			}})
			if err != nil {
				return Result{}, err
			}
			text, images := mcpContentParts(res.Content)
			if res.IsError {
				if text == "" {
					text = "工具执行失败"
				}
				return Result{}, errors.New(text)
			}
			return Result{Text: text, Images: images}, nil
		},
	}
}

// mcpContentParts 拆出 content 块里的文本与图片。
// 图片是「看图能力」的关键：截图、参考图都从这里进入模型，不能像以前那样丢掉。
func mcpContentParts(contents []mcp.Content) (string, []Image) {
	var (
		texts  []string
		images []Image
	)
	for _, c := range contents {
		switch v := c.(type) {
		case mcp.TextContent:
			texts = append(texts, v.Text)
		case mcp.ImageContent:
			if v.Data == "" {
				continue
			}
			mime := v.MIMEType
			if mime == "" {
				mime = "image/png"
			}
			// 单张图过大时不塞进请求，改为文字提示，避免请求直接被拒
			if len(v.Data) > maxImageBase64 {
				texts = append(texts, fmt.Sprintf("（一张 %s 图片过大，已跳过，约 %d KB）", mime, len(v.Data)/1024))
				continue
			}
			images = append(images, Image{MIME: mime, Base64: v.Data})
		}
	}
	return strings.Join(texts, "\n"), images
}
