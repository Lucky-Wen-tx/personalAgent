package main

import (
	"os"
	"path/filepath"
	"testing"

	"Agent/core"
)

func TestParseSkill(t *testing.T) {
	content := "---\nname: translate\ndescription: 中英互译\n---\n正文第一行\n正文第二行\n"
	s, err := parseSkill(content)
	if err != nil {
		t.Fatalf("parseSkill 出错: %v", err)
	}
	if s.Name != "translate" {
		t.Errorf("name = %q, want translate", s.Name)
	}
	if s.Description != "中英互译" {
		t.Errorf("description = %q, want 中英互译", s.Description)
	}
	if s.Instructions != "正文第一行\n正文第二行" {
		t.Errorf("instructions = %q", s.Instructions)
	}
}

func TestParseSkillInvalid(t *testing.T) {
	for name, content := range map[string]string{
		"无 frontmatter":   "正文",
		"frontmatter 未闭合": "---\nname: x\n",
		"缺少 name":         "---\ndescription: d\n---\n正文",
	} {
		if _, err := parseSkill(content); err == nil {
			t.Errorf("%s: 期望报错，实际通过了", name)
		}
	}
}

func TestLoadSkills(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		d := filepath.Join(dir, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("translate", "---\nname: translate\ndescription: d1\n---\n指令1")
	write("no-name", "---\ndescription: d2\n---\n指令2")                      // 解析失败
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil { // 无 SKILL.md，应跳过
		t.Fatal(err)
	}

	if _, err := LoadSkills(dir); err == nil {
		t.Error("含非法技能的目录应返回错误")
	}

	// 只放合法技能时正常加载
	os.RemoveAll(filepath.Join(dir, "no-name"))
	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("LoadSkills 出错: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "translate" {
		t.Errorf("skills = %+v, want 只有 translate", skills)
	}
}

func TestLoadSkillIntoSession(t *testing.T) {
	skills := []Skill{{Name: "translate", Description: "翻译", Instructions: "翻译规则"}}
	session := core.NewSession()
	loaded := map[string]bool{}

	msg, err := loadSkillIntoSession(skills, session, loaded, "translate")
	if err != nil {
		t.Fatalf("加载出错: %v", err)
	}
	if msg != "已加载技能：translate（翻译）" {
		t.Errorf("msg = %q", msg)
	}
	if !loaded["translate"] {
		t.Error("应记录已加载")
	}
	if got := len(session.GetMessages()); got != 1 {
		t.Errorf("session 消息数 = %d, want 1（指令已注入）", got)
	}

	// 重复加载：不重复注入
	_, err = loadSkillIntoSession(skills, session, loaded, "translate")
	if err != nil {
		t.Fatalf("重复加载出错: %v", err)
	}
	if got := len(session.GetMessages()); got != 1 {
		t.Errorf("重复加载后消息数 = %d, want 仍为 1", got)
	}

	// 不存在的技能
	if _, err := loadSkillIntoSession(skills, session, loaded, "nope"); err == nil {
		t.Error("未知技能应报错")
	}
}
