package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

// dangerPatterns 危险命令模式：命中则需人工审批，附带触发原因。
var dangerPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`\brm\b`), "删除操作（rm）"},
	{regexp.MustCompile(`\bsudo\b`), "提权操作（sudo）"},
	{regexp.MustCompile(`(curl|wget)\b[^|]*\|\s*(bash|sh)`), "下载并执行（curl/wget | bash）"},
	{regexp.MustCompile(`\bpython(3)?\s+-c\s`), "内联代码执行（python -c）"},
	{regexp.MustCompile(`\beval\b`), "内联代码执行（eval）"},
	{regexp.MustCompile(`\bdd\b`), "磁盘写入（dd）"},
	{regexp.MustCompile(`\bmkfs\b`), "格式化文件系统（mkfs）"},
	{regexp.MustCompile(`\bchmod\s+\+x`), "赋予执行权限（chmod +x）"},
}

// resolveShell 定位 sh 可执行文件：先按 PATH 查找，找不到再回退到常见安装位置。
// 返回 sh 的绝对路径；都找不到时返回空字符串。
func resolveShell() string {
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	// Windows 下 Git Bash 的常见安装位置
	candidates := []string{
		`C:\Program Files\Git\usr\bin\sh.exe`,
		`C:\Program Files\Git\bin\sh.exe`,
		`C:\Program Files (x86)\Git\usr\bin\sh.exe`,
		`C:\Program Files (x86)\Git\bin\sh.exe`,
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// NewShellTool 构造 shell 命令执行工具。
// 命令在 workspace 目录内执行，单条超时 30 秒。
// 危险命令（删除、提权、下载执行、内联代码等）需人工审批后才执行。
func NewShellTool(workspace string) *ToolDef {
	wsAbs, err := filepath.Abs(workspace)
	if err != nil {
		wsAbs = workspace
	}
	// 确保工作区存在，避免首次执行因目录缺失失败
	_ = os.MkdirAll(wsAbs, 0o755)
	shPath := resolveShell()

	return &ToolDef{
		Name:        "run_shell",
		Description: "在工作区目录内执行一条 shell 命令并返回输出。适用于 ls、cat、grep、git、包管理等常规操作；涉及删除、提权、下载执行、内联代码等危险操作需先经用户确认。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "要执行的 shell 命令",
				},
			},
			"required": []string{"command"},
		},
		PreCheck: func(args string) (bool, string) {
			var p struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return false, ""
			}
			for _, d := range dangerPatterns {
				if d.re.MatchString(p.Command) {
					return true, d.reason
				}
			}
			return false, ""
		},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.Command == "" {
				return "", fmt.Errorf("缺少必填参数 command")
			}

			// 单条命令 30 秒超时
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			if shPath == "" {
				return "", fmt.Errorf("未找到 sh 可执行文件，请安装 Git Bash 或将其加入 PATH")
			}
			cmd := exec.CommandContext(ctx, shPath, "-c", p.Command)
			cmd.Dir = wsAbs
			// 用临时文件捕获输出，避免 Bubble Tea 终端环境下管道捕获可能失效的问题
			tmpFile, err := os.CreateTemp("", "agent_shell_*")
			if err != nil {
				return "", fmt.Errorf("创建临时文件失败: %w", err)
			}
			tmpName := tmpFile.Name()
			defer os.Remove(tmpName)
			cmd.Stdout = tmpFile
			cmd.Stderr = tmpFile
			execErr := cmd.Run()
			_ = tmpFile.Close()
			outBytes, _ := os.ReadFile(tmpName)
			out := string(outBytes)

			if ctx.Err() == context.DeadlineExceeded {
				out += "\n命令执行超时（30 秒）"
			}
			if len(out) > 8000 {
				out = out[:8000] + "\n...（已截断）"
			}
			// 附带工作目录、命令与执行状态，便于确认命令是否真正执行
			header := fmt.Sprintf("[工作目录] %s\n[命令] %s\n[执行错误] %v\n[输出字节] %d\n\n", wsAbs, p.Command, execErr, len(outBytes))
			return header + out, nil
		},
	}
}
