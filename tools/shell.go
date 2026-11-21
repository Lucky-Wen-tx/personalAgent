package tools

// shell 工具：给模型一个「可控、可观测、可中断」的命令执行接口，而不是一个透传的
// 命令行窗口。与原实现相比补上了：
//
//   - 超时可配（默认 120s，上限 10min），不再硬编码 30s；调用方的 ctx 也能中断
//   - 输出保头保尾，超限时把完整日志留在磁盘并给路径（编译错误通常在尾部）
//   - 结构化结果：退出码、耗时、工作目录、是否超时，失败不再只藏在 header 文本里
//   - 后台任务：background=true 返回 job id，配合 job_output / job_kill / job_list
//   - 超时或被 kill 时杀进程树（Windows 用 taskkill /T），避免 dev server 残留
//   - 环境变量脱敏：KEY/TOKEN/SECRET/PASSWORD 等一律不进子进程；并注入非交互变量
//     （GIT_PAGER=cat、CI=1、NO_COLOR=1…），避免分页器/编辑器把命令挂死
//   - 没有 Git Bash 时自动降级到 PowerShell，不再「装不上就完全不可用」
//   - 审批改为「按命令动词解析」而不是正则黑名单：`grep "rm"` 不再误报，
//     `del` / `Remove-Item` / `iex (iwr ...)` / `xargs rm` / `find -delete` /
//     `git reset --hard` / 重定向到工作区外 等不再漏报

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultShellTimeout = 120 * time.Second
	maxShellTimeout     = 10 * time.Minute
	maxShellOutput      = 8000
	maxJobsKept         = 32
)

// ==================== shell 选择 ====================

type shellSpec struct {
	kind string // "sh" 或 "powershell"
	path string
}

// detectShell 优先用 sh（Git Bash 等），找不到时在 Windows 上退到 PowerShell。
func detectShell() shellSpec {
	if p := resolveShell(); p != "" {
		return shellSpec{kind: "sh", path: p}
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"pwsh.exe", "powershell.exe"} {
			if p, err := exec.LookPath(name); err == nil {
				return shellSpec{kind: "powershell", path: p}
			}
		}
	}
	return shellSpec{kind: "sh", path: "/bin/sh"}
}

// command 按 shell 类型组装命令。PowerShell 下强制 UTF-8 输出，避免中文乱码。
func (s shellSpec) command(command string) *exec.Cmd {
	if s.kind == "powershell" {
		script := "$ErrorActionPreference='Continue'; " +
			"[Console]::OutputEncoding=[Text.Encoding]::UTF8; " +
			"$OutputEncoding=[Text.Encoding]::UTF8; " + command
		return exec.Command(s.path, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	}
	return exec.Command(s.path, "-c", command)
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

// shellEnv 构造子进程环境：在继承环境的基础上，把 sh 同目录及其相邻的
// Git bin 目录补进 PATH 最前面。否则 sh 找不到 ls/rm/grep 等外部命令
// （它们随 Git 安装，但用户 PATH 里可能没有 Git 的 usr/bin）。
func shellEnv(shPath string) []string {
	env := os.Environ()
	if shPath == "" {
		return env
	}
	shDir := filepath.Dir(shPath)

	// 收集要前置的目录：sh 所在目录，以及 Git 安装根下常见的 bin 目录
	var prepend []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[strings.ToLower(d)] {
			return
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			seen[strings.ToLower(d)] = true
			prepend = append(prepend, d)
		}
	}
	// sh 在 <git>/usr/bin 时，Git 根为 <git>
	gitRoot := filepath.Dir(filepath.Dir(shDir))
	add(shDir)
	add(filepath.Join(gitRoot, "mingw64", "bin"))
	add(filepath.Join(gitRoot, "usr", "local", "bin"))

	sep := string(os.PathListSeparator)
	pathKey := "PATH"
	prefix := strings.Join(prepend, sep)
	for i, kv := range env {
		if len(kv) > 5 && strings.EqualFold(kv[:5], pathKey+"=") {
			env[i] = pathKey + "=" + prefix + sep + kv[5:]
			return env
		}
	}
	return append(env, pathKey+"="+prefix)
}

// sensitiveEnvMarkers 命中即从子进程环境中剔除，避免模型通过 shell 读到密钥。
var sensitiveEnvMarkers = []string{
	"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL",
	"APIKEY", "AUTH", "COOKIE", "SESSION",
}

// nonInteractiveEnv 让命令在无人守护的 TUI 里不会因为分页器、编辑器、交互提示而挂住。
var nonInteractiveEnv = map[string]string{
	"GIT_PAGER":           "cat",
	"PAGER":               "cat",
	"GIT_TERMINAL_PROMPT": "0",
	"GIT_ASKPASS":         "echo",
	"CI":                  "1",
	"NO_COLOR":            "1",
	"CLICOLOR":            "0",
	"FORCE_COLOR":         "0",
	"TERM":                "dumb",
	"PYTHONIOENCODING":    "utf-8",
	"PYTHONUTF8":          "1",
	"npm_config_yes":      "true",
}

func isSensitiveEnv(name string) bool {
	upper := strings.ToUpper(name)
	for _, m := range sensitiveEnvMarkers {
		if strings.Contains(upper, m) {
			return true
		}
	}
	return false
}

// buildEnv 在继承环境上做三件事：补 PATH、剔除敏感变量、注入非交互变量。
func buildEnv(spec shellSpec) []string {
	env := shellEnv(spec.path)
	out := make([]string, 0, len(env)+len(nonInteractiveEnv))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if _, overridden := nonInteractiveEnv[strings.ToUpper(name)]; overridden {
			continue
		}
		if isSensitiveEnv(name) {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(nonInteractiveEnv))
	for k := range nonInteractiveEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+nonInteractiveEnv[k])
	}
	return out
}

// ==================== 进程与结果 ====================

// killTree 结束进程及其子进程。Windows 上 sh 的子进程不会随父进程退出，
// 不收拾会留下占端口的 dev server。
func killTree(p *os.Process) {
	if p == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
	} else {
		_ = exec.Command("pkill", "-P", strconv.Itoa(p.Pid)).Run()
	}
	_ = p.Kill()
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

type shellResult struct {
	exitCode int
	duration time.Duration
	output   string
	timedOut bool
	logPath  string
	err      error
}

// format 把结果渲染成模型能直接读的固定字段格式。
func (r shellResult) format(command, cwd string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[cwd] %s\n", cwd)
	fmt.Fprintf(&sb, "[command] %s\n", shortenCommand(command, 200))
	fmt.Fprintf(&sb, "[exit code] %d\n", r.exitCode)
	fmt.Fprintf(&sb, "[duration] %.2fs\n", r.duration.Seconds())
	if r.timedOut {
		fmt.Fprintf(&sb, "[timeout] 已超时并终止进程树\n")
	}
	if r.err != nil && r.exitCode == -1 {
		fmt.Fprintf(&sb, "[error] %v\n", r.err)
	}
	if r.logPath != "" {
		fmt.Fprintf(&sb, "[full log] %s\n", r.logPath)
	}
	out := r.output
	if strings.TrimSpace(out) == "" {
		out = "（无输出）"
	}
	fmt.Fprintf(&sb, "[output]\n%s\n", out)
	return sb.String()
}

func shortenCommand(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ==================== 后台任务 ====================

type shellJob struct {
	id      string
	command string
	cwd     string
	logPath string
	started time.Time
	proc    *os.Process

	mu     sync.Mutex
	exit   int
	err    error
	killed bool
	done   chan struct{}
}

func (j *shellJob) snapshot() (running bool, exit int, killed bool, err error) {
	select {
	case <-j.done:
		j.mu.Lock()
		defer j.mu.Unlock()
		return false, j.exit, j.killed, j.err
	default:
		return true, 0, false, nil
	}
}

var (
	shellJobsMu sync.Mutex
	shellJobs   = map[string]*shellJob{}
	shellJobSeq int
)

func nextJobID() string {
	shellJobsMu.Lock()
	defer shellJobsMu.Unlock()
	shellJobSeq++
	return fmt.Sprintf("job-%d", shellJobSeq)
}

// CloseShellJobs 结束所有仍在运行的后台任务（程序退出时调用）。
func CloseShellJobs() {
	shellJobsMu.Lock()
	jobs := make([]*shellJob, 0, len(shellJobs))
	for _, j := range shellJobs {
		jobs = append(jobs, j)
	}
	shellJobsMu.Unlock()
	for _, j := range jobs {
		if running, _, _, _ := j.snapshot(); running {
			j.mu.Lock()
			j.killed = true
			j.mu.Unlock()
			killTree(j.proc)
		}
	}
}

func startJob(spec shellSpec, wsAbs, command string) (*shellJob, error) {
	f, err := os.CreateTemp("", "agent-job-*.log")
	if err != nil {
		return nil, fmt.Errorf("创建日志文件失败: %w", err)
	}
	cmd := spec.command(command)
	cmd.Dir = wsAbs
	cmd.Env = buildEnv(spec)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("启动命令失败: %w", err)
	}
	j := &shellJob{
		id:      nextJobID(),
		command: command,
		cwd:     wsAbs,
		logPath: f.Name(),
		started: time.Now(),
		proc:    cmd.Process,
		done:    make(chan struct{}),
	}
	shellJobsMu.Lock()
	shellJobs[j.id] = j
	// 只保留最近的若干个任务；优先清理已结束的，避免把还在跑的任务从表里删掉
	if len(shellJobs) > maxJobsKept {
		for id, old := range shellJobs {
			if id == j.id {
				continue
			}
			if running, _, _, _ := old.snapshot(); !running {
				delete(shellJobs, id)
			}
			if len(shellJobs) <= maxJobsKept {
				break
			}
		}
	}
	shellJobsMu.Unlock()

	go func() {
		waitErr := cmd.Wait()
		_ = f.Close()
		j.mu.Lock()
		j.exit = exitCode(waitErr)
		j.err = waitErr
		j.mu.Unlock()
		close(j.done)
	}()
	return j, nil
}

func findJob(id string) (*shellJob, error) {
	shellJobsMu.Lock()
	defer shellJobsMu.Unlock()
	j, ok := shellJobs[id]
	if !ok {
		ids := make([]string, 0, len(shellJobs))
		for k := range shellJobs {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("没有这个 job：%s（现有：%s）", id, strings.Join(ids, "、"))
	}
	return j, nil
}

// tailOutput 取日志尾部，保证不切断 UTF-8。
func tailOutput(s string, n int) string {
	if len(s) <= n {
		return s
	}
	out := s[len(s)-n:]
	for len(out) > 0 && !utf8.RuneStart(out[0]) {
		out = out[1:]
	}
	return "…（仅显示日志末尾）\n" + out
}

// ==================== 前台执行 ====================

func runForeground(ctx context.Context, spec shellSpec, wsAbs, command string, timeout time.Duration) shellResult {
	if timeout <= 0 {
		timeout = defaultShellTimeout
	}
	if timeout > maxShellTimeout {
		timeout = maxShellTimeout
	}
	f, err := os.CreateTemp("", "agent-shell-*.log")
	if err != nil {
		return shellResult{exitCode: -1, err: err}
	}
	logPath := f.Name()

	cmd := spec.command(command)
	cmd.Dir = wsAbs
	cmd.Env = buildEnv(spec)
	cmd.Stdout = f
	cmd.Stderr = f

	start := time.Now()
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		_ = os.Remove(logPath)
		return shellResult{exitCode: -1, err: fmt.Errorf("启动命令失败: %w", err), duration: time.Since(start)}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var runErr error
	timedOut := false
	select {
	case runErr = <-done:
	case <-timer.C:
		timedOut = true
		killTree(cmd.Process)
		runErr = <-done
	case <-ctx.Done():
		timedOut = true
		killTree(cmd.Process)
		runErr = <-done
	}
	_ = f.Close()

	data, _ := os.ReadFile(logPath)
	res := shellResult{
		exitCode: exitCode(runErr),
		duration: time.Since(start),
		output:   string(data),
		timedOut: timedOut,
		err:      runErr,
	}
	if len(res.output) > maxShellOutput {
		res.output = truncateOutput(res.output, maxShellOutput)
		res.logPath = logPath
	} else {
		_ = os.Remove(logPath)
	}
	return res
}

// ==================== 命令审批 ====================

var segmentPattern = regexp.MustCompile(`&&|\|\||;|\||\n|\(|\)`)

// dangerousVerbs 以「段首动词」判定，避免 `grep "rm" file` 这类误报。
var dangerousVerbs = map[string]string{
	"rm": "删除文件或目录（rm）", "rmdir": "删除目录（rmdir）", "del": "删除文件（del）",
	"erase": "删除文件（erase）", "rd": "删除目录（rd）", "remove-item": "删除（Remove-Item）",
	"ri": "删除（Remove-Item 别名 ri）", "format": "格式化磁盘（format）",
	"mkfs": "格式化文件系统（mkfs）", "dd": "磁盘写入（dd）",
	"takeown": "夺取文件所有权（takeown）", "icacls": "修改文件权限（icacls）",
	"reg": "修改注册表（reg）", "shutdown": "关机或重启（shutdown）",
	"taskkill": "结束进程（taskkill）", "tracert": "网络诊断（tracert）",
	"mv": "移动或覆盖文件（mv）", "move": "移动或覆盖文件（move）",
	"truncate": "截断文件（truncate）", "chmod": "修改权限（chmod）", "chown": "修改所有者（chown）",
	"sudo": "提权（sudo）", "runas": "提权（runas）", "doas": "提权（doas）",
	"sc": "管理服务（sc）", "net": "管理用户或服务（net）",
	"eval": "内联代码执行（eval）", "iex": "内联代码执行（iex）",
	"invoke-expression": "内联代码执行（Invoke-Expression）",
	"start-process":     "启动新进程（Start-Process）",
}

var inlineExecVerbs = map[string]bool{
	"python": true, "python3": true, "node": true, "perl": true,
	"ruby": true, "php": true, "deno": true, "bun": true,
}

var pipeToShell = regexp.MustCompile(`(?i)\|\s*(?:sudo\s+)?(?:sh|bash|zsh|dash|ksh|pwsh|powershell|iex|invoke-expression)\b`)

var remoteFetch = regexp.MustCompile(`(?i)\b(curl|wget|invoke-webrequest|invoke-restmethod|iwr|irm)\b`)

var redirectPattern = regexp.MustCompile(`(?:^|[\s;&|])(?:>>?|2>|&>)\s*("[^"]*"|'[^']*'|[^\s;&|]+)`)

// reviewCommand 返回是否需要人工审批及原因。wsAbs 用于判断「工作区外」。
func reviewCommand(command, wsAbs string) (bool, string) {
	return reviewCommandAt(command, wsAbs, 0)
}

func reviewCommandAt(command, wsAbs string, depth int) (bool, string) {
	if strings.TrimSpace(command) == "" || depth > 3 {
		return false, ""
	}
	// 1) 下载/解码后直接丢给 shell 执行
	if pipeToShell.MatchString(command) {
		if remoteFetch.MatchString(command) {
			return true, "下载并直接执行（网络内容管道进 shell）"
		}
		return true, "把输出直接交给 shell 执行（管道进 shell）"
	}
	// 2) 重定向到工作区外
	if reason := reviewRedirects(command, wsAbs); reason != "" {
		return true, reason
	}
	// 3) 逐段看动词
	for _, seg := range segmentPattern.Split(command, -1) {
		if need, reason := reviewSegment(seg, wsAbs, depth); need {
			return true, reason
		}
	}
	return false, ""
}

func reviewSegment(seg, wsAbs string, depth int) (bool, string) {
	tokens := strings.Fields(strings.TrimSpace(seg))
	for len(tokens) > 0 && (tokens[0] == "&" || tokens[0] == "!") {
		tokens = tokens[1:]
	}
	// 跳过前缀：环境变量赋值、常见包装命令
	for len(tokens) > 0 {
		t := tokens[0]
		if strings.Contains(t, "=") && !strings.HasPrefix(t, "=") && !strings.Contains(t, " ") && isEnvAssignment(t) {
			tokens = tokens[1:]
			continue
		}
		switch strings.ToLower(t) {
		case "env", "nohup", "time", "command", "exec", "builtin", "call", "start":
			tokens = tokens[1:]
			continue
		}
		break
	}
	if len(tokens) == 0 {
		return false, ""
	}

	verb := normalizeVerb(tokens[0])
	if reason, ok := dangerousVerbs[verb]; ok {
		return true, reason
	}

	// 在 shell 里再执行一条命令：递归检查内部命令
	if isShellInterpreter(verb) {
		for i, t := range tokens[1:] {
			lt := strings.ToLower(t)
			if lt == "-c" || lt == "/c" || lt == "-command" || lt == "--command" {
				inner := strings.Join(tokens[i+2:], " ")
				inner = strings.Trim(inner, `"'`)
				if need, reason := reviewCommandAt(inner, wsAbs, depth+1); need {
					return true, reason
				}
			}
		}
		return false, ""
	}

	// python -c / node -e 之类的内联代码
	if inlineExecVerbs[verb] || strings.HasPrefix(verb, "python") {
		for _, t := range tokens[1:] {
			switch strings.ToLower(t) {
			case "-c", "-e", "-r", "--eval", "-p":
				return true, "内联代码执行（" + verb + " " + strings.ToLower(t) + "）"
			}
		}
	}

	switch verb {
	case "find":
		joined := strings.ToLower(strings.Join(tokens, " "))
		if strings.Contains(joined, "-delete") || strings.Contains(joined, "-exec") {
			return true, "find 直接删除或执行命令（-delete / -exec）"
		}
	case "xargs":
		if len(tokens) > 1 {
			if reason, ok := dangerousVerbs[normalizeVerb(tokens[1])]; ok {
				return true, "xargs 批量执行：" + reason
			}
		}
	case "git":
		if len(tokens) > 1 {
			sub := strings.ToLower(tokens[1])
			rest := strings.Join(tokens[2:], " ")
			switch sub {
			case "push":
				if strings.Contains(rest, "--force") || strings.Contains(rest, " -f") || strings.HasPrefix(rest, "-f") {
					return true, "强制推送（git push --force）"
				}
			case "reset":
				if strings.Contains(rest, "--hard") {
					return true, "丢弃本地改动（git reset --hard）"
				}
			case "clean":
				if strings.Contains(rest, "f") || strings.Contains(rest, "x") {
					return true, "删除未跟踪文件（git clean -f/-x）"
				}
			case "branch":
				if strings.Contains(rest, "-D") {
					return true, "强制删除分支（git branch -D）"
				}
			}
		}
	case "cd", "pushd":
		if len(tokens) == 1 {
			return true, "切换到用户主目录（工作区外）"
		}
		if len(tokens) > 1 {
			target := strings.Trim(tokens[1], `"'`)
			if reason := reviewDirTarget(target, wsAbs); reason != "" {
				return true, reason
			}
		}
	case "npm", "pnpm", "yarn":
		if len(tokens) > 1 {
			sub := strings.ToLower(tokens[1])
			if sub == "publish" || sub == "unpublish" {
				return true, "发布软件包（" + verb + " " + sub + "）"
			}
		}
	}
	return false, ""
}

func isEnvAssignment(t string) bool {
	name := t
	if i := strings.IndexByte(t, '='); i > 0 {
		name = t[:i]
	}
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func isShellInterpreter(verb string) bool {
	switch verb {
	case "sh", "bash", "zsh", "dash", "ksh", "pwsh", "powershell", "cmd":
		return true
	}
	return false
}

// normalizeVerb 取命令名：去掉路径、引号与 .exe/.cmd/.bat 后缀。
func normalizeVerb(t string) string {
	t = strings.Trim(t, `"'`)
	if i := strings.LastIndexAny(t, `\/`); i >= 0 {
		t = t[i+1:]
	}
	t = strings.ToLower(t)
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".com", ".ps1"} {
		t = strings.TrimSuffix(t, suffix)
	}
	return t
}

// reviewRedirects 检查 `>`、`>>`、`2>` 的目标是否在工作区外。
func reviewRedirects(command, wsAbs string) string {
	for _, m := range redirectPattern.FindAllStringSubmatch(command, -1) {
		target := strings.Trim(m[1], `"'`)
		lower := strings.ToLower(target)
		if target == "" || lower == "/dev/null" || lower == "$null" || lower == "nul" ||
			lower == "/dev/stderr" || lower == "/dev/stdout" || strings.HasPrefix(target, "&") {
			continue
		}
		if !isPathLike(target) {
			continue
		}
		if reason := reviewDirTarget(target, wsAbs); reason != "" {
			return "把输出重定向到工作区外：" + target
		}
	}
	return ""
}

func isPathLike(t string) bool {
	if strings.HasPrefix(t, "/") || strings.HasPrefix(t, `\`) || strings.HasPrefix(t, "~") {
		return true
	}
	if len(t) > 1 && t[1] == ':' {
		return true
	}
	return strings.Contains(t, string(filepath.Separator)) || strings.Contains(t, "/") || strings.HasPrefix(t, "..")
}

// reviewDirTarget 判断一个目录/文件目标是否落在工作区外。
func reviewDirTarget(target, wsAbs string) string {
	if target == "" || target == "-" || strings.HasPrefix(target, "~") || strings.HasPrefix(target, "$") {
		return "切换到或写入工作区外的位置：" + target
	}
	clean := strings.TrimSpace(target)
	if !filepath.IsAbs(clean) && !(len(clean) > 1 && clean[1] == ':') {
		clean = filepath.Join(wsAbs, clean)
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(wsAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "操作工作区外的路径：" + target
	}
	return ""
}

// ==================== 工具定义 ====================

// NewShellTools 构造 shell 相关工具：run_shell + job_output / job_kill / job_list。
func NewShellTools(workspace string) []*ToolDef {
	wsAbs, err := filepath.Abs(workspace)
	if err != nil {
		wsAbs = workspace
	}
	_ = os.MkdirAll(wsAbs, 0o755)
	spec := detectShell()

	return []*ToolDef{
		newRunShellTool(wsAbs, spec),
		newJobOutputTool(),
		newJobKillTool(),
		newJobListTool(),
	}
}

// NewShellTool 兼容旧调用方，只返回 run_shell。
func NewShellTool(workspace string) *ToolDef {
	return NewShellTools(workspace)[0]
}

func newRunShellTool(wsAbs string, spec shellSpec) *ToolDef {
	return &ToolDef{
		Name: "run_shell",
		Description: "在工作区目录内执行一条命令，返回退出码、耗时与输出。普通查看/搜索/构建直接执行；" +
			"删除、提权、下载执行、内联代码、写工作区外等危险操作会先请求用户确认。" +
			"长任务（dev server、watch）用 background=true，再用 job_output 看输出。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "要执行的命令"},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "超时毫秒数，缺省 120000，上限 600000",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "工作目录，相对工作区，缺省为工作区根；不允许指到工作区外",
				},
				"background": map[string]any{
					"type":        "boolean",
					"description": "是否后台运行（适合 dev server、长任务），返回 job id",
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
			return reviewCommand(p.Command, wsAbs)
		},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Command    string `json:"command"`
				TimeoutMS  int    `json:"timeout_ms"`
				Workdir    string `json:"workdir"`
				Background bool   `json:"background"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if strings.TrimSpace(p.Command) == "" {
				return "", fmt.Errorf("缺少必填参数 command")
			}

			cwd := wsAbs
			if strings.TrimSpace(p.Workdir) != "" {
				dir, err := safeRel(wsAbs, p.Workdir)
				if err != nil {
					return "", err
				}
				info, statErr := os.Stat(dir)
				if statErr != nil || !info.IsDir() {
					return "", fmt.Errorf("工作目录不存在: %s", p.Workdir)
				}
				cwd = dir
			}

			if p.Background {
				j, err := startJob(spec, cwd, p.Command)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("[job] %s\n[status] running\n[cwd] %s\n[command] %s\n[log] %s\n用 job_output 读取输出，job_kill 结束进程。",
					j.id, cwd, shortenCommand(p.Command, 200), j.logPath), nil
			}

			timeout := defaultShellTimeout
			if p.TimeoutMS > 0 {
				timeout = time.Duration(p.TimeoutMS) * time.Millisecond
			}
			res := runForeground(ctx, spec, cwd, p.Command, timeout)
			return res.format(p.Command, cwd), nil
		},
	}
}

func newJobOutputTool() *ToolDef {
	return &ToolDef{
		Name:        "job_output",
		Description: "读取后台任务的输出（默认只看当前内容，wait_ms>0 时最多等指定毫秒直到任务结束）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_id":  map[string]any{"type": "string", "description": "run_shell 返回的 job id，例如 job-1"},
				"wait_ms": map[string]any{"type": "integer", "description": "最多等待毫秒数，缺省 0，上限 30000"},
			},
			"required": []string{"job_id"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				JobID  string `json:"job_id"`
				WaitMS int    `json:"wait_ms"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			j, err := findJob(p.JobID)
			if err != nil {
				return "", err
			}
			if p.WaitMS > 0 {
				if p.WaitMS > 30000 {
					p.WaitMS = 30000
				}
				select {
				case <-j.done:
				case <-time.After(time.Duration(p.WaitMS) * time.Millisecond):
				}
			}
			running, code, killed, runErr := j.snapshot()
			status := "exited"
			if running {
				status = "running"
			}
			data, _ := os.ReadFile(j.logPath)
			var sb strings.Builder
			fmt.Fprintf(&sb, "[job] %s\n[status] %s\n", j.id, status)
			if !running {
				fmt.Fprintf(&sb, "[exit code] %d\n", code)
			}
			if killed {
				sb.WriteString("[killed] 已被终止\n")
			}
			if runErr != nil && code == -1 {
				fmt.Fprintf(&sb, "[error] %v\n", runErr)
			}
			fmt.Fprintf(&sb, "[command] %s\n[cwd] %s\n[log] %s\n", shortenCommand(j.command, 200), j.cwd, j.logPath)
			fmt.Fprintf(&sb, "[elapsed] %.1fs\n[output]\n%s\n", time.Since(j.started).Seconds(), tailOutput(string(data), maxShellOutput))
			return sb.String(), nil
		},
	}
}

func newJobKillTool() *ToolDef {
	return &ToolDef{
		Name:        "job_kill",
		Description: "结束后台任务及其子进程（例如停在跑着的 dev server）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_id": map[string]any{"type": "string", "description": "要结束的 job id"},
			},
			"required": []string{"job_id"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				JobID string `json:"job_id"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			j, err := findJob(p.JobID)
			if err != nil {
				return "", err
			}
			if running, _, _, _ := j.snapshot(); !running {
				return fmt.Sprintf("[job] %s\n[status] 已结束，无需终止", j.id), nil
			}
			j.mu.Lock()
			j.killed = true
			j.mu.Unlock()
			killTree(j.proc)
			select {
			case <-j.done:
			case <-time.After(3 * time.Second):
			}
			running, code, _, _ := j.snapshot()
			status := "exited"
			if running {
				status = "still-running"
			}
			return fmt.Sprintf("[job] %s\n[status] %s\n[exit code] %d", j.id, status, code), nil
		},
	}
}

func newJobListTool() *ToolDef {
	return &ToolDef{
		Name:        "job_list",
		Description: "列出本次会话里启动过的后台任务及其状态。",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
		Handler: func(_ context.Context, _ string) (string, error) {
			shellJobsMu.Lock()
			jobs := make([]*shellJob, 0, len(shellJobs))
			for _, j := range shellJobs {
				jobs = append(jobs, j)
			}
			shellJobsMu.Unlock()
			if len(jobs) == 0 {
				return "（没有后台任务）", nil
			}
			sort.Slice(jobs, func(a, b int) bool { return jobs[a].started.Before(jobs[b].started) })
			var sb strings.Builder
			for _, j := range jobs {
				running, code, killed, _ := j.snapshot()
				status := "exited"
				if running {
					status = "running"
				}
				flag := ""
				if killed {
					flag = " killed"
				}
				fmt.Fprintf(&sb, "%s  %-9s exit=%d%s  %.0fs  %s\n", j.id, status, code, flag,
					time.Since(j.started).Seconds(), shortenCommand(j.command, 80))
			}
			return sb.String(), nil
		},
	}
}
