package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"Agent/core"
	"Agent/tools"

	"github.com/openai/openai-go/v3/responses"
)

// ==================== Skill ====================
// Skill 是一个可复用的指令包：SKILL.md 的 frontmatter 声明名称和描述，
// 正文是给模型的指令。加载后指令以 system 消息注入 session，跨轮次持久生效。

type Skill struct {
	Name         string
	Description  string
	Instructions string
}

// LoadSkills 从目录加载全部技能：每个子目录一个技能，内含 SKILL.md。
// 目录不存在或个别文件解析失败时返回错误，由调用方决定是否降级。
func LoadSkills(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取技能目录 %s: %w", dir, err)
	}

	skills := make([]Skill, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err != nil {
			continue // 没有 SKILL.md 的目录直接跳过
		}
		s, err := parseSkill(string(data))
		if err != nil {
			return nil, fmt.Errorf("解析技能 %s: %w", e.Name(), err)
		}
		skills = append(skills, s)
	}
	return skills, nil
}

// parseSkill 解析 SKILL.md：--- 包裹的 frontmatter 声明 name/description，其后为正文。
func parseSkill(content string) (Skill, error) {
	const delim = "---"
	if !strings.HasPrefix(content, delim+"\n") {
		return Skill{}, fmt.Errorf("缺少 frontmatter")
	}
	rest := content[len(delim)+1:]
	end := strings.Index(rest, "\n"+delim)
	if end < 0 {
		return Skill{}, fmt.Errorf("frontmatter 未闭合")
	}
	body := strings.TrimPrefix(rest[end+len("\n"+delim):], "\n")

	var s Skill
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "name":
			s.Name = strings.TrimSpace(v)
		case "description":
			s.Description = strings.TrimSpace(v)
		}
	}
	if s.Name == "" {
		return Skill{}, fmt.Errorf("缺少 name")
	}
	s.Instructions = strings.TrimSpace(body)
	return s, nil
}

// findSkill 按名称查找技能
func findSkill(skills []Skill, name string) (Skill, error) {
	for _, s := range skills {
		if s.Name == name {
			return s, nil
		}
	}
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	return Skill{}, fmt.Errorf("未找到技能：%s（可用：%s）", name, strings.Join(names, "、"))
}

// loadSkillIntoSession 加载技能：指令以 system 消息注入 session，并记入 loaded
// 避免重复注入。返回的文本供调用方展示。
func loadSkillIntoSession(skills []Skill, session *core.Session, loaded map[string]bool, name string) (string, error) {
	s, err := findSkill(skills, name)
	if err != nil {
		return "", err
	}
	if loaded[name] {
		return fmt.Sprintf("技能 %s 此前已加载", s.Name), nil
	}
	session.AddMessage(
		responses.ResponseInputItemParamOfMessage(
			s.Instructions,
			responses.EasyInputMessageRoleSystem,
		),
	)
	loaded[name] = true
	return fmt.Sprintf("已加载技能：%s（%s）", s.Name, s.Description), nil
}

// newLoadSkillTool 构造 load_skill 工具：模型判断需要时自行调用，
// 技能指令注入 session 后跨轮次生效。
func newLoadSkillTool(skills []Skill, session *core.Session, loaded map[string]bool) *tools.ToolDef {
	desc := "加载一个技能包，将其指令注入当前对话（跨轮次生效）。可用技能："
	if len(skills) == 0 {
		desc += "（无）"
	} else {
		for _, s := range skills {
			desc += fmt.Sprintf("%s（%s）；", s.Name, s.Description)
		}
	}

	return &tools.ToolDef{
		Name:        "load_skill",
		Description: desc,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "技能名称",
				},
			},
			"required": []string{"name"},
		},
		Handler: func(_ context.Context, args string) (string, error) {
			var p struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.Name == "" {
				return "", fmt.Errorf("缺少必填参数 name")
			}
			return loadSkillIntoSession(skills, session, loaded, p.Name)
		},
	}
}
