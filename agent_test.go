package main

import (
	"os"
	"path/filepath"
	"testing"

	"Agent/core"
	"Agent/tools"
)

func TestExecutePendingTool(t *testing.T) {
	shellTool := tools.NewShellTool("../workspace")
	m := Model{
		llmTools: map[string]*tools.ToolDef{"run_shell": shellTool},
		session:  core.NewSession(),
	}

	ws, _ := filepath.Abs("../workspace")
	target := filepath.Join(ws, "del_test2.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := pendingTool{name: "run_shell", callID: "call1", args: `{"command":"rm del_test2.txt;echo \"exit=$?\"; ls"}`}
	msg := m.executePendingTool(p)()

	done, ok := msg.(toolDoneMsg)
	if !ok {
		t.Fatalf("不是 toolDoneMsg: %v", msg)
	}
	t.Logf("tool_result: %q", done.display[0].Content)
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("rm 未删除文件，tool_result=%q", done.display[0].Content)
	}
}
