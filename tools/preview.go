package tools

// 前端智能体缺的那一环：写完的页面没法看、没法验。
// 这里补两个工具：
//   preview_start / preview_stop  在本地起一个静态服务器，模型可以用 http_request 访问自己刚写的页面
//   html_check                    不依赖浏览器的静态自检：doctype、viewport、lang、title、图片 alt、
//                                 重复 id、死链、本地资源是否存在、内联事件等
//
// 纯标准库实现，不引入 headless 浏览器依赖；需要真实渲染/截图时再走 MCP（playwright 等）。

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 预览服务器 ----------

type previewServer struct {
	srv  *http.Server
	root string
}

var (
	previewMu      sync.Mutex
	previewServers = map[int]*previewServer{}
)

// NewPreviewTools 构造预览与页面自检工具，静态文件根目录为 root。
func NewPreviewTools(root string) []*ToolDef {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		rootAbs = root
	}

	start := &ToolDef{
		Name:        "preview_start",
		Description: "在本地起一个静态服务器，把工作目录当作网站根目录，返回可访问的 URL。写完页面后用它 + http_request 自检是否 200、资源是否 404。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port": map[string]any{"type": "integer", "description": "监听端口，缺省 0 表示自动挑一个空闲端口"},
			},
			"required": []string{},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Port int `json:"port"`
			}
			if strings.TrimSpace(args) != "" {
				if err := json.Unmarshal([]byte(args), &p); err != nil {
					return "", fmt.Errorf("参数解析失败: %w", err)
				}
			}
			port, err := startPreview(rootAbs, p.Port)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("预览已启动：http://127.0.0.1:%d/（根目录 %s）\n用 http_request 访问该地址可拿到渲染前的 HTML；用 preview_stop 关闭。", port, rootAbs), nil
		},
	}

	stop := &ToolDef{
		Name:        "preview_stop",
		Description: "关闭 preview_start 启动的本地静态服务器。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port": map[string]any{"type": "integer", "description": "要关闭的端口；留空则关闭全部"},
			},
			"required": []string{},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Port int `json:"port"`
			}
			if strings.TrimSpace(args) != "" {
				if err := json.Unmarshal([]byte(args), &p); err != nil {
					return "", fmt.Errorf("参数解析失败: %w", err)
				}
			}
			return stopPreview(p.Port)
		},
	}

	return []*ToolDef{start, stop, newHTMLCheckTool(rootAbs)}
}

func startPreview(rootAbs string, port int) (int, error) {
	previewMu.Lock()
	defer previewMu.Unlock()

	if port != 0 {
		if _, ok := previewServers[port]; ok {
			return port, nil
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 0, fmt.Errorf("监听端口失败: %w", err)
	}
	actual := ln.Addr().(*net.TCPAddr).Port

	fs := http.FileServer(http.Dir(rootAbs))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 自检时不要缓存，否则改了页面还看到旧内容
		w.Header().Set("Cache-Control", "no-store")
		fs.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	previewServers[actual] = &previewServer{srv: srv, root: rootAbs}
	go func() {
		_ = srv.Serve(ln)
	}()
	return actual, nil
}

func stopPreview(port int) (string, error) {
	previewMu.Lock()
	defer previewMu.Unlock()

	if port == 0 {
		if len(previewServers) == 0 {
			return "没有正在运行的预览服务器。", nil
		}
		var ports []string
		for p, s := range previewServers {
			_ = s.srv.Close()
			ports = append(ports, strconv.Itoa(p))
			delete(previewServers, p)
		}
		sort.Strings(ports)
		return fmt.Sprintf("已关闭预览服务器：%s", strings.Join(ports, "、")), nil
	}
	s, ok := previewServers[port]
	if !ok {
		return "", fmt.Errorf("端口 %d 上没有正在运行的预览服务器", port)
	}
	_ = s.srv.Close()
	delete(previewServers, port)
	return fmt.Sprintf("已关闭预览服务器：%d", port), nil
}

// ---------- html_check ----------

type htmlFinding struct {
	line    int
	message string
}

func newHTMLCheckTool(rootAbs string) *ToolDef {
	return &ToolDef{
		Name:        "html_check",
		Description: "静态检查一个 HTML 页面：doctype、lang、viewport、title、图片 alt、重复 id、空链接、内联事件、本地资源是否存在。用它替代人眼验收。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "HTML 文件路径，相对于工作目录"},
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
			abs, err := safeRel(rootAbs, p.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return "", fmt.Errorf("读取文件失败: %w", err)
			}
			if len(data) > 2<<20 {
				return "", fmt.Errorf("文件过大（%d 字节），不适合静态检查", len(data))
			}
			text := strings.ReplaceAll(string(data), "\r\n", "\n")
			findings := checkHTML(text, filepath.Dir(abs))
			if len(findings) == 0 {
				return fmt.Sprintf("%s：未发现静态问题（注意这不能替代真实渲染与无障碍检测）。", p.Path), nil
			}
			sort.SliceStable(findings, func(i, j int) bool { return findings[i].line < findings[j].line })
			var sb strings.Builder
			fmt.Fprintf(&sb, "%s：发现 %d 个问题\n", p.Path, len(findings))
			for _, f := range findings {
				fmt.Fprintf(&sb, "%6d| %s\n", f.line, f.message)
			}
			return truncateOutput(sb.String(), maxToolOutput), nil
		},
	}
}

var (
	reImg     = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	reButton  = regexp.MustCompile(`(?is)<button\b[^>]*>(.*?)</button>`)
	reATag    = regexp.MustCompile(`(?is)<a\b[^>]*>`)
	reID      = regexp.MustCompile(`(?is)\bid\s*=\s*["']([^"']+)["']`)
	reTitle   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reLang    = regexp.MustCompile(`(?is)<html\b[^>]*\blang\s*=\s*["'][^"']+["']`)
	reView    = regexp.MustCompile(`(?is)<meta\b[^>]*name\s*=\s*["']viewport["']`)
	reEvent   = regexp.MustCompile(`(?is)\son[a-z]+\s*=\s*["']`)
	reSrcAttr = regexp.MustCompile(`(?is)\b(?:src|href)\s*=\s*["']([^"']+)["']`)
	reAltAttr = regexp.MustCompile(`(?is)\balt\s*=\s*["'][^"']*["']`)
)

// checkHTML 返回静态问题列表，baseDir 用于检查本地资源是否存在。
func checkHTML(text, baseDir string) []htmlFinding {
	var out []htmlFinding
	add := func(idx int, format string, a ...any) {
		out = append(out, htmlFinding{line: lineAt(text, idx), message: fmt.Sprintf(format, a...)})
	}

	lower := strings.ToLower(strings.TrimSpace(text))
	if !strings.HasPrefix(lower, "<!doctype html") {
		add(0, "缺少 <!DOCTYPE html>，浏览器会进入怪异模式")
	}
	if !reLang.MatchString(text) {
		add(0, "<html> 缺少 lang 属性（影响读屏与语音合成）")
	}
	if !reView.MatchString(text) {
		add(0, "缺少 <meta name=\"viewport\">，移动端会按桌面宽度缩放")
	}
	if m := reTitle.FindStringSubmatchIndex(text); m == nil {
		add(0, "缺少 <title>")
	} else if strings.TrimSpace(text[m[2]:m[3]]) == "" {
		add(m[0], "<title> 为空")
	}

	// 图片 alt
	for _, loc := range reImg.FindAllStringIndex(text, -1) {
		tag := text[loc[0]:loc[1]]
		if !reAltAttr.MatchString(tag) {
			add(loc[0], "<img> 缺少 alt：%s", shorten(tag, 60))
		}
	}

	// 按钮可访问名
	for _, m := range reButton.FindAllStringSubmatchIndex(text, -1) {
		tag := text[m[0]:m[1]]
		inner := text[m[2]:m[3]]
		if strings.TrimSpace(stripTags(inner)) == "" && !strings.Contains(strings.ToLower(tag), "aria-label") {
			add(m[0], "<button> 没有可见文字也没有 aria-label，读屏读不出来")
		}
	}

	// 空链接
	for _, loc := range reATag.FindAllStringIndex(text, -1) {
		tag := text[loc[0]:loc[1]]
		if strings.Contains(tag, `href="#"`) || strings.Contains(strings.ToLower(tag), "javascript:") || !strings.Contains(strings.ToLower(tag), "href") {
			add(loc[0], "<a> 没有有效 href：%s", shorten(tag, 60))
		}
	}

	// 重复 id
	seen := map[string]int{}
	for _, m := range reID.FindAllStringSubmatchIndex(text, -1) {
		id := strings.TrimSpace(text[m[2]:m[3]])
		if id == "" {
			continue
		}
		if first, ok := seen[id]; ok {
			add(m[0], `id="%s" 重复（首次出现在第 %d 行）`, id, first)
			continue
		}
		seen[id] = lineAt(text, m[0])
	}

	// 内联事件
	for _, loc := range reEvent.FindAllStringIndex(text, -1) {
		add(loc[0], "内联事件处理器（on*=）：不利于 CSP，也不符合「结构/样式/行为分离」")
	}

	// 本地资源是否存在
	for _, m := range reSrcAttr.FindAllStringSubmatchIndex(text, -1) {
		raw := strings.TrimSpace(text[m[2]:m[3]])
		// 只检查相对路径：站内绝对路径（/x）与外部链接无法用本地文件判断
		if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "/") ||
			strings.Contains(raw, "://") || strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "mailto:") {
			continue
		}
		raw = strings.SplitN(raw, "?", 2)[0]
		raw = strings.SplitN(raw, "#", 2)[0]
		target := filepath.Join(baseDir, filepath.FromSlash(strings.TrimPrefix(raw, "/")))
		if _, err := os.Stat(target); err != nil {
			add(m[0], "引用的本地资源不存在：%s", raw)
		}
	}

	return out
}

// lineAt 返回下标所在的 1 基行号。
func lineAt(text string, idx int) int {
	if idx <= 0 {
		return 1
	}
	if idx > len(text) {
		idx = len(text)
	}
	return strings.Count(text[:idx], "\n") + 1
}

// stripTags 去掉标签，用于判断按钮内是否有可见文字。
func stripTags(s string) string {
	var sb strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				sb.WriteRune(r)
			}
		}
	}
	return html.UnescapeString(sb.String())
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
