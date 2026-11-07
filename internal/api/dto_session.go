package api

import (
	"time"

	"laborlaw/internal/port"
)

// sessionDTO 是会话在 HTTP 上的形状，字段名逐字来自
// contract/openapi.yaml 的 components.schemas.Session。
//
// port.Session 不打 tag，直接序列化会输出 ID/UserID/Title 这样的大写键，
// 前端拿到的就是 undefined。形状在这里定，port 保持原样。
type sessionDTO struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Scene     string    `json:"scene"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// chatMessageDTO 对应 contract/openapi.yaml 的 components.schemas.ChatMessage。
//
// 不输出 prompt_tokens / trace_id：那是计量与排障字段，属于内部数据，
// 出现在响应里等于把成本结构和链路 ID 交给前端。
type chatMessageDTO struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// toSessionDTO 把会话映射成契约形状。
func toSessionDTO(s *port.Session) sessionDTO {
	if s == nil {
		return sessionDTO{}
	}
	return sessionDTO{
		ID:        s.ID,
		Title:     s.Title,
		Scene:     s.Scene,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

// toSessionDTOs 批量映射。空输入返回空切片而不是 nil：前端会对 data 直接 .map()。
func toSessionDTOs(items []port.Session) []sessionDTO {
	out := make([]sessionDTO, 0, len(items))
	for i := range items {
		out = append(out, toSessionDTO(&items[i]))
	}
	return out
}

// toChatMessageDTOs 批量映射消息。
//
// role 原样透传：port.Role 的取值与 openapi 的 enum 一致，转换层不再做映射，
// 否则多一处取值集合就多一处会漂移的地方。
func toChatMessageDTOs(items []port.ChatMessage) []chatMessageDTO {
	out := make([]chatMessageDTO, 0, len(items))
	for _, m := range items {
		out = append(out, chatMessageDTO{
			ID:        m.ID,
			SessionID: m.SessionID,
			Role:      string(m.Role),
			Content:   m.Content,
			CreatedAt: m.CreatedAt,
		})
	}
	return out
}
