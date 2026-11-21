package tools

// 看图能力：把工作区里的图片直接交给模型看（参考图、设计稿、自己生成的截图）。
// 与 MCP 截图工具共用同一条图片回传链路：Result.Images → function_call_output。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	// maxImageBase64 单张图片 base64 后的大小上限；超过就只给文字提示，避免请求被拒。
	maxImageBase64 = 5 << 20
	// maxImageBytes 读取本地图片的原始大小上限。
	maxImageBytes = 8 << 20
	// maxImagesPerResult 单次工具返回最多携带的图片数，防止一次塞爆上下文。
	maxImagesPerResult = 4
)

// NewImageTools 构造看图工具，路径限制在 root 内。
func NewImageTools(root string) []*ToolDef {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		rootAbs = root
	}
	return []*ToolDef{newReadImageTool(rootAbs)}
}

func newReadImageTool(rootAbs string) *ToolDef {
	return &ToolDef{
		Name: "read_image",
		Description: "读取工作区里的图片并直接交给模型「看」（支持 png / jpeg / gif / webp）。" +
			"用户给了参考图、设计稿，或想确认自己产出的截图时用它。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "图片路径，相对于工作目录"},
			},
			"required": []string{"path"},
		},
		RichHandler: func(_ context.Context, args string) (Result, error) {
			var p struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return Result{}, fmt.Errorf("参数解析失败: %w", err)
			}
			abs, err := safeRel(rootAbs, p.Path)
			if err != nil {
				return Result{}, err
			}
			info, err := os.Stat(abs)
			if err != nil {
				return Result{}, fmt.Errorf("找不到图片: %w", err)
			}
			if info.IsDir() {
				return Result{}, fmt.Errorf("%s 是目录，不是图片", p.Path)
			}
			if info.Size() > maxImageBytes {
				return Result{}, fmt.Errorf("图片过大（%.1f MB），上限 %d MB", float64(info.Size())/(1<<20), maxImageBytes>>20)
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return Result{}, fmt.Errorf("读取图片失败: %w", err)
			}
			mime := detectImageMIME(data, abs)
			if mime == "" {
				return Result{}, fmt.Errorf("不是受支持的图片格式（png / jpeg / gif / webp）：%s", p.Path)
			}
			b64 := base64.StdEncoding.EncodeToString(data)
			if len(b64) > maxImageBase64 {
				return Result{}, fmt.Errorf("图片编码后过大（%.1f MB），无法发送给模型", float64(len(b64))/(1<<20))
			}
			return Result{
				Text:   fmt.Sprintf("[图片] %s（%s，%d KB）", p.Path, mime, len(data)/1024),
				Images: []Image{{MIME: mime, Base64: b64}},
			}, nil
		},
	}
}

// detectImageMIME 先按内容嗅探；只有嗅探成「二进制流」时才按扩展名兜底，
// 文本文件伪装成 .png 一律拒绝。
func detectImageMIME(data []byte, name string) string {
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mime
	}
	if !strings.HasPrefix(mime, "application/octet-stream") {
		return ""
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}
