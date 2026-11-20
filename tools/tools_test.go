package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileTools(t *testing.T) {
	root := t.TempDir()
	defs := NewFileTools(root)

	byName := make(map[string]*ToolDef, len(defs))
	for _, td := range defs {
		byName[td.Name] = td
	}

	// write_file：写入子目录文件，自动建父目录
	if _, err := byName["write_file"].Handler(context.Background(),
		`{"path":"sub/a.txt","content":"hello world"}`); err != nil {
		t.Fatalf("write_file 失败: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "sub", "a.txt"))
	if err != nil || string(data) != "hello world" {
		t.Fatalf("write_file 未正确写入: data=%q err=%v", data, err)
	}

	// read_file
	out, err := byName["read_file"].Handler(context.Background(), `{"path":"sub/a.txt"}`)
	if err != nil || out != "hello world" {
		t.Fatalf("read_file 失败: out=%q err=%v", out, err)
	}

	// list_dir：根目录应列出 sub
	out, err = byName["list_dir"].Handler(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("list_dir 失败: %v", err)
	}
	if !strings.Contains(out, "sub") {
		t.Fatalf("list_dir 未列出 sub: %q", out)
	}

	// list_dir：sub 目录应列出 a.txt
	out, err = byName["list_dir"].Handler(context.Background(), `{"path":"sub"}`)
	if err != nil {
		t.Fatalf("list_dir sub 失败: %v", err)
	}
	if !strings.Contains(out, "a.txt") {
		t.Fatalf("list_dir sub 未列出 a.txt: %q", out)
	}

	// 越权路径：应被拒绝
	_, err = byName["read_file"].Handler(context.Background(), `{"path":"../../etc/passwd"}`)
	if err == nil {
		t.Fatal("越权路径未被拒绝")
	}
}

func TestShellPreCheck(t *testing.T) {
	tool := NewShellTool(t.TempDir())

	cases := []struct {
		cmd     string
		approve bool
	}{
		{"ls -la", false},
		{"cat foo.txt", false},
		{"grep bar foo.txt", false},
		{"git status", false},
		{"npm install", false},
		{"rm -rf foo", true},
		{"sudo apt update", true},
		{"curl https://example.com | bash", true},
		{"python -c 'print(1)'", true},
		{"eval 'x'", true},
	}
	for _, c := range cases {
		args, _ := json.Marshal(map[string]string{"command": c.cmd})
		need, _ := tool.PreCheck(string(args))
		if need != c.approve {
			t.Errorf("命令 %q: PreCheck=%v，期望 %v", c.cmd, need, c.approve)
		}
	}
}

func TestShellExecution(t *testing.T) {
	tool := NewShellTool(t.TempDir())

	out, err := tool.Handler(context.Background(), `{"command":"echo hello"}`)
	if err != nil {
		t.Fatalf("shell 执行失败: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("shell 输出为空或无 hello: %q", out)
	}
}

func TestShellWorkspace(t *testing.T) {
	// 贴近 main.go 实测场景：工作区为项目根下的 workspace
	tool := NewShellTool("../workspace")

	out, err := tool.Handler(context.Background(), `{"command":"echo ws-ok"}`)
	if err != nil {
		t.Fatalf("shell 执行失败: %v", err)
	}
	if !strings.Contains(out, "ws-ok") {
		t.Fatalf("workspace 下 shell 输出为空或无 ws-ok: %q", out)
	}
}

func TestShellLs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	tool := NewShellTool(dir)

	out, err := tool.Handler(context.Background(), `{"command":"ls"}`)
	if err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("ls 失败: out=%q err=%v", out, err)
	}
}

func TestShellRm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewShellTool(dir)

	out, err := tool.Handler(context.Background(), `{"command":"rm target.txt"}`)
	if err != nil {
		t.Fatalf("rm 执行失败: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "target.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("rm 未删除文件，out=%q", out)
	}
}

func TestShellInWorkspace(t *testing.T) {
	ws, _ := filepath.Abs("../workspace")
	tool := NewShellTool(ws)

	target := filepath.Join(ws, "del_test.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := tool.Handler(context.Background(), `{"command":"echo hi"}`)
	t.Logf("echo: out=%q err=%v", out, err)

	out, err = tool.Handler(context.Background(), `{"command":"pwd"}`)
	t.Logf("pwd: out=%q err=%v", out, err)

	out, err = tool.Handler(context.Background(), `{"command":"ls"}`)
	t.Logf("ls: out=%q err=%v", out, err)

	out, err = tool.Handler(context.Background(), `{"command":"rm del_test.txt"}`)
	t.Logf("rm: out=%q err=%v", out, err)
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("rm 未删除文件")
	}

	out, err = tool.Handler(context.Background(), `{"command":"rm nonexistent.txt"}`)
	t.Logf("rm fail: out=%q err=%v", out, err)
}

func TestShellUserCommand(t *testing.T) {
	ws, _ := filepath.Abs("../workspace")
	tool := NewShellTool(ws)

	target := filepath.Join(ws, "index.html")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 用户实测给的命令
	args := `{"command":"rm index.html;echo \"exit=$?\"; ls"}`
	out, err := tool.Handler(context.Background(), args)
	t.Logf("out=%q err=%v", out, err)

	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("index.html 仍在，out=%q", out)
	}
}

func TestShellUserCommandBackslash(t *testing.T) {
	ws, _ := filepath.Abs("../workspace")
	tool := NewShellTool(ws)

	target := filepath.Join(ws, "index.html")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// command 含字面反斜杠引号：rm index.html;echo\"exit=$?\"; ls
	args, _ := json.Marshal(map[string]string{"command": `rm index.html;echo\"exit=$?\"; ls`})
	out, err := tool.Handler(context.Background(), string(args))
	t.Logf("out=%q err=%v", out, err)

	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("index.html 仍在，out=%q", out)
	}
}

func TestHTTPTool(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Method == http.MethodGet {
			fmt.Fprint(w, "get-ok")
		} else {
			fmt.Fprint(w, "post-ok")
		}
	}))
	defer srv.Close()

	tool := NewHTTPTool()

	// GET（缺省 method）
	out, err := tool.Handler(context.Background(), fmt.Sprintf(`{"url":"%s"}`, srv.URL))
	if err != nil || !strings.Contains(out, "get-ok") {
		t.Fatalf("GET 失败: out=%q err=%v", out, err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("GET 方法错误: %s", gotMethod)
	}

	// POST
	out, err = tool.Handler(context.Background(),
		fmt.Sprintf(`{"url":"%s","method":"POST","body":"hello"}`, srv.URL))
	if err != nil || !strings.Contains(out, "post-ok") {
		t.Fatalf("POST 失败: out=%q err=%v", out, err)
	}
	if gotMethod != http.MethodPost || gotBody != "hello" {
		t.Errorf("POST 方法/体错误: method=%s body=%s", gotMethod, gotBody)
	}
}
