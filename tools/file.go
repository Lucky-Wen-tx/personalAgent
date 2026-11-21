package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NewFileTools 构造文件操作内置工具：read_file / write_file / list_dir。
// 所有路径都限制在 root 目录内，越权路径拒绝。
func NewFileTools(root string) []*ToolDef {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		rootAbs = root
	}

	// resolve 把相对路径解析为 root 内的绝对路径，越权则报错。
	resolve := func(p string) (string, error) {
		if strings.TrimSpace(p) == "" {
			p = "."
		}
		abs, err := filepath.Abs(filepath.Join(rootAbs, p))
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(rootAbs, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("路径越权，必须位于工作目录内: %s", p)
		}
		return abs, nil
	}

	readFile := &ToolDef{
		Name:        "read_file",
		Description: "读取指定文件的内容并返回。路径相对于工作目录。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "文件路径，相对于工作目录，例如 src/main.go",
				},
			},
			"required": []string{"path"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			abs, err := resolve(p.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return "", fmt.Errorf("读取文件失败: %w", err)
			}
			if isBinary(data) {
				return "", fmt.Errorf("这是二进制文件（%d 字节），不支持直接读取", len(data))
			}
			// 按字符截断并保留尾部，避免切断多字节字符、也避免丢掉文件末尾的内容
			return truncateOutput(string(data), maxToolOutput), nil
		},
	}

	writeFile := &ToolDef{
		Name:        "write_file",
		Description: "将内容写入指定文件（覆盖已有内容），自动创建父目录。路径相对于工作目录。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "文件路径，相对于工作目录",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "要写入的完整内容",
				},
			},
			"required": []string{"path", "content"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			abs, err := resolve(p.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return "", fmt.Errorf("创建目录失败: %w", err)
			}
			if err := os.WriteFile(abs, []byte(p.Content), 0o644); err != nil {
				return "", fmt.Errorf("写入文件失败: %w", err)
			}
			return fmt.Sprintf("已写入 %s（%d 字符）", p.Path, len(p.Content)), nil
		},
	}

	listDir := &ToolDef{
		Name:        "list_dir",
		Description: "列出指定目录的内容（文件与子目录）。路径相对于工作目录，缺省为工作目录根。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "目录路径，相对于工作目录，留空则列工作目录根",
				},
			},
			"required": []string{},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Path string `json:"path"`
			}
			if args != "" {
				if err := json.Unmarshal([]byte(args), &p); err != nil {
					return "", fmt.Errorf("参数解析失败: %w", err)
				}
			}
			abs, err := resolve(p.Path)
			if err != nil {
				return "", err
			}
			entries, err := os.ReadDir(abs)
			if err != nil {
				return "", fmt.Errorf("读取目录失败: %w", err)
			}
			var lines []string
			for _, e := range entries {
				kind := "文件"
				if e.IsDir() {
					kind = "目录"
				}
				lines = append(lines, fmt.Sprintf("%s  %s", kind, e.Name()))
			}
			sort.Strings(lines)
			if len(lines) == 0 {
				return "（空目录）", nil
			}
			return strings.Join(lines, "\n"), nil
		},
	}

	return []*ToolDef{readFile, writeFile, listDir}
}
