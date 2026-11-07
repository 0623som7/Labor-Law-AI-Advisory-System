package api

import (
	"time"

	"laborlaw/internal/port"
)

// statuteDTO 对应 contract/openapi.yaml 的 components.schemas.Statute。
//
// 为什么要再声明一次：port.Statute 的字段没有 json tag（port 层不掌握 transport 细节），
// 直接序列化输出的是 ID / EffectiveAt 这样的 Go 字段名，而契约规定的是 snake_case。
// 对外形状由 api 层负责，改契约时这里跟着改，port 不动。
type statuteDTO struct {
	ID          string    `json:"id"`
	Law         string    `json:"law"`
	Article     string    `json:"article"`
	Title       string    `json:"title"`
	Text        string    `json:"text"`
	URL         string    `json:"url"`
	EffectiveAt time.Time `json:"effective_at"`
}

// statuteHitDTO 对应 components.schemas.StatuteHit。
//
// 分数和条文一起下发：前端要按相关度折叠"更多结果"，只给条文就只能按返回顺序猜。
type statuteHitDTO struct {
	Statute statuteDTO `json:"statute"`
	Score   float64    `json:"score"`
}

// toStatuteDTO 把 port.Statute 映射成对外形状。
func toStatuteDTO(s port.Statute) statuteDTO {
	return statuteDTO{
		ID:          s.ID,
		Law:         s.Law,
		Article:     s.Article,
		Title:       s.Title,
		Text:        s.Text,
		URL:         s.URL,
		EffectiveAt: s.EffectiveAt,
	}
}

// toStatuteHitDTOs 映射检索结果。
//
// 空结果返回空切片而不是 nil：契约写的是数组，nil 会序列化成 null，
// 前端遍历前还得多判一次空。
func toStatuteHitDTOs(hits []port.StatuteHit) []statuteHitDTO {
	out := make([]statuteHitDTO, 0, len(hits))
	for _, h := range hits {
		out = append(out, statuteHitDTO{
			Statute: toStatuteDTO(h.Statute),
			Score:   h.Score,
		})
	}
	return out
}
