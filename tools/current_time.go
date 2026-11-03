package tools

import (
	"context"
	"time"
)

// NewGetCurrentTime 返回一个获取当前时间的工具
func NewGetCurrentTime() *ToolDef {
	return &ToolDef{
		Name:        "get_current_time",
		Description: "获取当前系统时间，返回格式 YYYY-MM-DD HH:MM:SS（东八区）",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
		Handler: func(ctx context.Context, args string) (string, error) {
			// 东八区
			loc, err := time.LoadLocation("Asia/Shanghai")
			if err != nil {
				loc = time.Local
			}
			return time.Now().In(loc).Format("2006-01-02 15:04:05"), nil
		},
	}
}