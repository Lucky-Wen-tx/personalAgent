package tools

// 本文件把「读整份文件、整份重写」的粗粒度文件工具，补成一套可增量改代码的工具：
//   read_lines  按行读，带行号与 offset/limit，长文件可分段读完
//   edit_file   精确替换一段文本，替换前校验唯一性，避免整文件重写
//   glob        按模式找文件
//   grep        按正则搜内容
//
// 这套语义与常见编码智能体（Claude Code / Cursor / DSH）一致：模型不需要先把
// 整份 HTML 读进来再整份吐回去，改一行只花改一行的 token，也不会覆盖别人的改动。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// maxToolOutput 单次工具返回的字符上限，超出部分保留头尾。
	maxToolOutput = 8000
	// maxReadLines 一次 read_lines 最多返回的行数。
	maxReadLines = 2000
	// maxGlobResults / maxGrepMatches 搜索结果上限，防止一次性灌爆上下文。
	maxGlobResults = 200
	maxGrepMatches = 200
)

// NewCodeTools 构造增量编辑类工具，所有路径都限制在 root 内。
func NewCodeTools(root string) []*ToolDef {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		rootAbs = root
	}

	return []*ToolDef{
		newReadLinesTool(rootAbs),
		newEditFileTool(rootAbs),
		newGlobTool(rootAbs),
		newGrepTool(rootAbs),
	}
}

// safeRel 把模型给的路径解析到 root 内，并检查路径上的链接/联接，
// 防止 workspace 里放一个指向外部的 junction 绕过边界。
func safeRel(rootAbs, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		p = "."
	}
	abs, err := filepath.Abs(filepath.Join(rootAbs, p))
	if err != nil {
		return "", err
	}
	if err := inside(rootAbs, abs, p); err != nil {
		return "", err
	}
	if err := checkLinkComponents(rootAbs, abs, p); err != nil {
		return "", err
	}

	// 目标已存在：解析符号链接后再校验一次。
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, inside(rootAbs, resolved, p)
	} else if _, statErr := os.Lstat(abs); statErr == nil {
		return "", fmt.Errorf("路径无法解析，可能经过符号链接或目录联接，已拒绝: %s", p)
	}

	// 目标还不存在（例如要新建的文件）：找到最近的已存在祖先，解析后拼回剩余路径。
	for probe := filepath.Dir(abs); ; probe = filepath.Dir(probe) {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			rest, relErr := filepath.Rel(probe, abs)
			if relErr != nil {
				return "", relErr
			}
			full := filepath.Join(resolved, rest)
			return full, inside(rootAbs, full, p)
		} else if _, statErr := os.Lstat(probe); statErr == nil {
			return "", fmt.Errorf("路径无法解析，可能经过符号链接或目录联接，已拒绝: %s", p)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("无法解析路径: %s", p)
		}
	}
}

// inside 校验 target 是否位于 rootAbs 内。
func inside(rootAbs, target, given string) error {
	rel, err := filepath.Rel(rootAbs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径越权，必须位于工作目录内: %s", given)
	}
	return nil
}

// checkLinkComponents 逐段检查路径上的符号链接与重解析点。
// Windows 上目录联接（junction）在 Lstat 里表现为 ModeIrregular，EvalSymlinks
// 不会解析它，只能自己识别后拒绝。
func checkLinkComponents(rootAbs, abs, given string) error {
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == "." {
		return nil
	}
	cur := rootAbs
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil // 路径段还不存在，无需继续检查
		}
		if fi.Mode()&os.ModeIrregular != 0 {
			return fmt.Errorf("路径经过目录联接或重解析点，已拒绝: %s", given)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			resolved, rerr := filepath.EvalSymlinks(cur)
			if rerr != nil {
				return fmt.Errorf("路径经过无法解析的符号链接，已拒绝: %s", given)
			}
			if err := inside(rootAbs, resolved, given); err != nil {
				return err
			}
		}
	}
	return nil
}

// truncateOutput 按字符（rune）截断，保留头尾，避免切断 UTF-8 与丢掉尾部报错。
func truncateOutput(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// 先按 rune 安全地切出一段比 limit 长的前缀，再从中取头部
	headBudget := limit * 2 / 3
	head := s[:headBudget]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	tail := s[len(s)-limit/3:]
	for len(tail) > 0 && !utf8.ValidString(tail) {
		tail = tail[1:]
	}
	return fmt.Sprintf("%s\n\n...（中间省略 %d 字符，共 %d 字符）...\n\n%s", head, len(s)-len(head)-len(tail), len(s), tail)
}

// ---------- read_lines ----------

func newReadLinesTool(rootAbs string) *ToolDef {
	return &ToolDef{
		Name:        "read_lines",
		Description: "按行读取文件，返回带行号的内容。长文件用 offset/limit 分段读完，不要整份读。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "文件路径，相对于工作目录"},
				"offset": map[string]any{"type": "integer", "description": "起始行号，从 1 开始，缺省为 1"},
				"limit":  map[string]any{"type": "integer", "description": "最多读取的行数，缺省 200，上限 2000"},
			},
			"required": []string{"path"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
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
			if isBinary(data) {
				return "", fmt.Errorf("这是二进制文件（%d 字节），不支持按行读取", len(data))
			}
			text := strings.ReplaceAll(string(data), "\r\n", "\n")
			lines := strings.Split(text, "\n")
			total := len(lines)

			offset := p.Offset
			if offset < 1 {
				offset = 1
			}
			limit := p.Limit
			if limit <= 0 {
				limit = 200
			}
			if limit > maxReadLines {
				limit = maxReadLines
			}
			if offset > total {
				return fmt.Sprintf("（文件只有 %d 行，offset=%d 超出范围）", total, offset), nil
			}
			end := offset - 1 + limit
			if end > total {
				end = total
			}

			var sb strings.Builder
			for i := offset; i <= end; i++ {
				fmt.Fprintf(&sb, "%6d| %s\n", i, lines[i-1])
			}
			header := fmt.Sprintf("[%s] 第 %d-%d 行 / 共 %d 行\n", p.Path, offset, end, total)
			out := header + sb.String()
			if end < total {
				out += fmt.Sprintf("...（还有 %d 行，用 offset=%d 继续读）", total-end, end+1)
			}
			return truncateOutput(out, maxToolOutput), nil
		},
	}
}

// ---------- edit_file ----------

func newEditFileTool(rootAbs string) *ToolDef {
	return &ToolDef{
		Name:        "edit_file",
		Description: "精确替换文件中的一段文本：old_string 必须在文件中唯一出现（或用 replace_all 全部替换）。改代码优先用它，不要整份重写文件。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "文件路径，相对于工作目录"},
				"old_string":  map[string]any{"type": "string", "description": "要被替换的原文，必须与文件内容完全一致（含缩进）"},
				"new_string":  map[string]any{"type": "string", "description": "替换后的文本；留空表示删除这段内容"},
				"replace_all": map[string]any{"type": "boolean", "description": "old_string 出现多次时是否全部替换，缺省 false"},
			},
			"required": []string{"path", "old_string"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.OldString == "" {
				return "", fmt.Errorf("old_string 不能为空")
			}
			if p.OldString == p.NewString {
				return "", fmt.Errorf("old_string 与 new_string 相同，无需修改")
			}
			abs, err := safeRel(rootAbs, p.Path)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(abs)
			if err != nil {
				return "", fmt.Errorf("文件不存在: %w", err)
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return "", fmt.Errorf("读取文件失败: %w", err)
			}
			text := string(data)
			count := strings.Count(text, p.OldString)
			switch {
			case count == 0:
				return "", fmt.Errorf("未找到要替换的文本：请先用 read_lines 确认原文（含缩进与空白）完全一致")
			case count > 1 && !p.ReplaceAll:
				lines := matchLines(text, p.OldString)
				return "", fmt.Errorf("old_string 出现了 %d 次（第 %v 行），请补充上下文使其唯一，或设 replace_all=true", count, lines)
			}

			updated := strings.ReplaceAll(text, p.OldString, p.NewString)
			if err := writeFileAtomic(abs, []byte(updated), info.Mode().Perm()); err != nil {
				return "", err
			}
			return fmt.Sprintf("已修改 %s（%d 处替换）\n%s", p.Path, count, contextSnippet(updated, p.NewString)), nil
		},
	}
}

// matchLines 返回 old_string 每次出现所在的行号（从 1 开始）。
func matchLines(text, sub string) []int {
	var lines []int
	offset := 0
	for {
		i := strings.Index(text[offset:], sub)
		if i < 0 {
			return lines
		}
		pos := offset + i
		lines = append(lines, strings.Count(text[:pos], "\n")+1)
		offset = pos + len(sub)
	}
}

// contextSnippet 返回替换位置附近几行，便于确认改对了地方。
func contextSnippet(text, needle string) string {
	if needle == "" {
		return ""
	}
	pos := strings.Index(text, needle)
	if pos < 0 {
		return ""
	}
	start := strings.LastIndex(text[:pos], "\n") + 1
	lineNo := strings.Count(text[:start], "\n") + 1
	lines := strings.Split(text[start:], "\n")
	end := len(lines)
	if end > 4 {
		end = 4
	}
	var sb strings.Builder
	for i := 0; i < end; i++ {
		fmt.Fprintf(&sb, "%6d| %s\n", lineNo+i, lines[i])
	}
	return sb.String()
}

// writeFileAtomic 先写临时文件再改名，避免写到一半中断留下半截源码。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".edit-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	_ = os.Chmod(tmpName, perm)
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("替换文件失败: %w", err)
	}
	return nil
}

// ---------- glob ----------

func newGlobTool(rootAbs string) *ToolDef {
	return &ToolDef{
		Name:        "glob",
		Description: "按通配符查找文件，例如 **/*.html、src/**/*.css。返回相对路径列表。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "通配符模式，支持 *、?、**（匹配任意层目录）"},
			},
			"required": []string{"pattern"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Pattern string `json:"pattern"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if strings.TrimSpace(p.Pattern) == "" {
				return "", fmt.Errorf("缺少必填参数 pattern")
			}
			pattern := p.Pattern
			// 不含分隔符的模式按「任意层级下的同名文件」处理，例如 *.html
			if !strings.Contains(filepath.ToSlash(pattern), "/") {
				pattern = "**/" + pattern
			}
			re, err := globToRegexp(pattern)
			if err != nil {
				return "", err
			}
			var found []string
			err = filepath.WalkDir(rootAbs, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if isIgnoredDir(d.Name()) {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				rel, err := filepath.Rel(rootAbs, path)
				if err != nil {
					return nil
				}
				if re.MatchString(filepath.ToSlash(rel)) {
					found = append(found, filepath.ToSlash(rel))
				}
				return nil
			})
			if err != nil {
				return "", fmt.Errorf("遍历目录失败: %w", err)
			}
			sort.Strings(found)
			if len(found) == 0 {
				return "（没有匹配的文件）", nil
			}
			note := ""
			if len(found) > maxGlobResults {
				note = fmt.Sprintf("\n...（还有 %d 个匹配未列出）", len(found)-maxGlobResults)
				found = found[:maxGlobResults]
			}
			return truncateOutput(strings.Join(found, "\n")+note, maxToolOutput), nil
		},
	}
}

// globToRegexp 把通配符转成正则：** 跨目录、* 不跨目录、? 单字符。
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	pattern = filepath.ToSlash(pattern)
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					sb.WriteString("(?:.*/)?")
				} else {
					sb.WriteString(".*")
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

func isIgnoredDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", ".idea", ".vscode":
		return true
	}
	return false
}

// ---------- grep ----------

func newGrepTool(rootAbs string) *ToolDef {
	return &ToolDef{
		Name:        "grep",
		Description: "在工作目录里按正则搜索文件内容，返回「文件:行号: 内容」。用 include 限定文件类型，例如 *.html。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "正则表达式，例如 TODO|href=\"#\""},
				"path":    map[string]any{"type": "string", "description": "搜索起点目录，缺省为工作目录根"},
				"include": map[string]any{"type": "string", "description": "文件名通配符，例如 *.html、*.css"},
			},
			"required": []string{"pattern"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Include string `json:"include"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				return "", fmt.Errorf("正则无效: %w", err)
			}
			startAbs, err := safeRel(rootAbs, p.Path)
			if err != nil {
				return "", err
			}

			var matches []string
			truncated := false
			err = filepath.WalkDir(startAbs, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if isIgnoredDir(d.Name()) {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				if p.Include != "" {
					ok, matchErr := filepath.Match(p.Include, d.Name())
					if matchErr != nil {
						return fmt.Errorf("include 通配符无效: %w", matchErr)
					}
					if !ok {
						return nil
					}
				}
				info, statErr := d.Info()
				if statErr != nil || info.Size() > 2<<20 {
					return nil
				}
				data, readErr := os.ReadFile(path)
				if readErr != nil || isBinary(data) {
					return nil
				}
				rel, relErr := filepath.Rel(rootAbs, path)
				if relErr != nil {
					return nil
				}
				for i, line := range strings.Split(string(data), "\n") {
					if !re.MatchString(line) {
						continue
					}
					text := strings.TrimRight(line, "\r")
					if len(text) > 200 {
						text = text[:200] + "…"
					}
					matches = append(matches, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), i+1, strings.TrimSpace(text)))
					if len(matches) >= maxGrepMatches {
						truncated = true
						return filepath.SkipAll
					}
				}
				return nil
			})
			if err != nil {
				return "", fmt.Errorf("搜索失败: %w", err)
			}
			if len(matches) == 0 {
				return "（没有匹配）", nil
			}
			out := strings.Join(matches, "\n")
			if truncated {
				out += fmt.Sprintf("\n...（已达 %d 条上限，请缩小搜索范围）", maxGrepMatches)
			}
			return truncateOutput(out, maxToolOutput), nil
		},
	}
}

// isBinary 用前 8KB 是否含 NUL 判断二进制文件。
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}
