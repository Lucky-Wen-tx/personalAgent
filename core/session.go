package core

import "github.com/openai/openai-go/v3/responses"

// Session 维护一次对话中的消息列表
type Session struct {
	messages []responses.ResponseInputItemUnionParam
}

func NewSession() *Session {
	return &Session{
		messages: make([]responses.ResponseInputItemUnionParam, 0),
	}
}

// AddMessage 追加一条或多条消息
func (s *Session) AddMessage(items ...responses.ResponseInputItemUnionParam) {
	s.messages = append(s.messages, items...)
}

// GetMessages 返回消息列表的副本（避免外部误改）
func (s *Session) GetMessages() []responses.ResponseInputItemUnionParam {
	out := make([]responses.ResponseInputItemUnionParam, len(s.messages))
	copy(out, s.messages)
	return out
}